package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"
)

// FileName is the name of the SQLite file inside pogfarm's data directory.
const FileName = "pogfarm.db"

// BusyTimeout is how long a connection waits for a lock held by another
// connection before giving up with SQLITE_BUSY.
const BusyTimeout = 5 * time.Second

// maxReaders caps the read-only connection pool.
const maxReaders = 8

//go:embed migrations
var embedded embed.FS

// Path returns the database path for a data directory, which is the data_dir
// setting of pogfarm's configuration.
func Path(dataDir string) string {
	return filepath.Join(dataDir, FileName)
}

// Store is the open database. It is safe for concurrent use.
type Store struct {
	writer *sql.DB
	reader *sql.DB
}

// Open opens the database at path, creating the file and its directory when
// they do not exist, and applies every migration that has not been applied
// yet. The caller must Close the store.
func Open(ctx context.Context, path string) (*Store, error) {
	migrations, err := fs.Sub(embedded, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: embedded migrations: %w", err)
	}
	return open(ctx, path, migrations)
}

// open is Open with the migration files taken from fsys.
func open(ctx context.Context, path string, fsys fs.FS) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: database path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("store: create directory: %w", err)
	}

	// The writer goes first: it creates the file and switches it to WAL,
	// which is recorded in the file and so holds for the readers too.
	// Transactions start with BEGIN IMMEDIATE so they take the write lock
	// up front instead of failing halfway when upgrading from a read lock.
	writer, err := sql.Open("sqlite", dsn(path,
		"_pragma", "journal_mode(WAL)",
		"_txlock", "immediate",
	))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// One connection: every write in the process queues here, which is what
	// keeps "database is locked" from ever happening.
	writer.SetMaxOpenConns(1)

	if err := migrate(ctx, writer, fsys); err != nil {
		writer.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	reader, err := sql.Open("sqlite", dsn(path, "_pragma", "query_only(1)"))
	if err != nil {
		writer.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	reader.SetMaxOpenConns(maxReaders)
	if err := reader.PingContext(ctx); err != nil {
		reader.Close()
		writer.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	return &Store{writer: writer, reader: reader}, nil
}

// dsn builds the driver's data source name. The pragmas in it are applied to
// every connection the pool opens, which matters because foreign_keys and
// busy_timeout are per-connection settings. extra is key, value pairs added
// to the ones every connection gets.
func dsn(path string, extra ...string) string {
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", BusyTimeout.Milliseconds()))
	q.Add("_pragma", "foreign_keys(1)")
	for i := 0; i+1 < len(extra); i += 2 {
		q.Add(extra[i], extra[i+1])
	}
	// The path is escaped so that characters such as '?' and '%' in a
	// directory name are not read as part of the URI.
	return "file:" + (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath() + "?" + q.Encode()
}

// Reader returns the pool of read-only connections. Use it for every query
// that does not change data; a write through it fails.
func (s *Store) Reader() *sql.DB { return s.reader }

// Writer returns the single write connection. Statements and transactions on
// it run one at a time, in the order they arrive. A transaction started here
// holds up every other write until it ends, so keep it short and do no slow
// work (network calls, subprocesses) inside it.
func (s *Store) Writer() *sql.DB { return s.writer }

// WriteTx runs fn inside a write transaction. The transaction commits when fn
// returns nil and rolls back when fn returns an error or panics. fn must use
// the transaction it is given: calling Writer or WriteTx from inside it
// waits for the connection this transaction holds and never returns.
func (s *Store) WriteTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return writeTx(ctx, s.writer, fn)
}

func writeTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// After a successful Commit this is a no-op.
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Close closes all connections. Closing the last one checkpoints the
// write-ahead log into the main file.
func (s *Store) Close() error {
	return errors.Join(s.reader.Close(), s.writer.Close())
}
