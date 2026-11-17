package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

// files builds an in-memory migrations directory from name, body pairs.
func files(pairs ...string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for i := 0; i+1 < len(pairs); i += 2 {
		fsys[pairs[i]] = &fstest.MapFile{Data: []byte(pairs[i+1])}
	}
	return fsys
}

// openTest opens a store at path with the given migrations and closes it when
// the test ends.
func openTest(t *testing.T, path string, fsys fstest.MapFS) *Store {
	t.Helper()
	s, err := open(context.Background(), path, fsys)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return s
}

// itemsFS is a one-table schema for tests that only need somewhere to write.
func itemsFS() fstest.MapFS {
	return files("0001_items.sql", `CREATE TABLE items (id INTEGER PRIMARY KEY, worker INTEGER NOT NULL, n INTEGER NOT NULL);`)
}

func appliedList(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT version, name, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var version int64
		var name, appliedAt string
		if err := rows.Scan(&version, &name, &appliedAt); err != nil {
			t.Fatal(err)
		}
		if appliedAt == "" {
			t.Errorf("migration %d has no applied_at", version)
		}
		out = append(out, fmt.Sprintf("%d:%s", version, name))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func TestPath(t *testing.T) {
	if got, want := Path("data"), filepath.Join("data", "pogfarm.db"); got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

func TestOpenCreatesFileAndAppliesMigrations(t *testing.T) {
	// The directory does not exist yet either.
	path := filepath.Join(t.TempDir(), "nested", "data", FileName)
	fsys := files(
		"0002_b.sql", `CREATE TABLE b (id INTEGER PRIMARY KEY, a_id INTEGER NOT NULL REFERENCES a(id));`,
		"0001_a.sql", `CREATE TABLE a (id INTEGER PRIMARY KEY); INSERT INTO a (id) VALUES (1); INSERT INTO a (id) VALUES (2);`,
		"README.md", `not a migration`,
	)
	s, err := open(context.Background(), path, fsys)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database file not created: %v", err)
	}
	if got, want := strings.Join(appliedList(t, s.Reader()), ","), "1:a,2:b"; got != want {
		t.Errorf("applied = %q, want %q", got, want)
	}
	// Every statement of a multi-statement file ran.
	var n int
	if err := s.Reader().QueryRow(`SELECT count(*) FROM a`).Scan(&n); err != nil || n != 2 {
		t.Errorf("rows in a = %d, %v; want 2", n, err)
	}
	if !tableExists(t, s.Reader(), "b") {
		t.Error("table b missing")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestReopenAppliesOnlyNewMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	// Neither statement can run twice: the table would already exist, and a
	// second insert would leave two rows.
	fsys := files("0001_a.sql", `CREATE TABLE a (id INTEGER PRIMARY KEY, note TEXT); INSERT INTO a (note) VALUES ('once');`)

	s, err := open(context.Background(), path, fsys)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	var firstAppliedAt string
	if err := s.Reader().QueryRow(`SELECT applied_at FROM schema_migrations WHERE version = 1`).Scan(&firstAppliedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Writer().Exec(`INSERT INTO a (note) VALUES ('kept')`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Same migrations: nothing to do.
	s, err = open(context.Background(), path, fsys)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// One more migration: only that one runs.
	fsys["0002_b.sql"] = &fstest.MapFile{Data: []byte(`CREATE TABLE b (id INTEGER PRIMARY KEY);`)}
	s = openTest(t, path, fsys)
	if got, want := strings.Join(appliedList(t, s.Reader()), ","), "1:a,2:b"; got != want {
		t.Errorf("applied = %q, want %q", got, want)
	}
	var n int
	if err := s.Reader().QueryRow(`SELECT count(*) FROM a`).Scan(&n); err != nil || n != 2 {
		t.Errorf("rows in a = %d, %v; want 2 (the migration's and the one written later)", n, err)
	}
	var appliedAt string
	if err := s.Reader().QueryRow(`SELECT applied_at FROM schema_migrations WHERE version = 1`).Scan(&appliedAt); err != nil {
		t.Fatal(err)
	}
	if appliedAt != firstAppliedAt {
		t.Errorf("migration 1 was recorded again: applied_at %q, was %q", appliedAt, firstAppliedAt)
	}
}

// The real entry point with the migrations embedded in the binary.
func TestOpenWithEmbeddedMigrations(t *testing.T) {
	path := Path(t.TempDir())
	for i := range 2 {
		s, err := Open(context.Background(), path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if !tableExists(t, s.Reader(), "schema_migrations") {
			t.Error("schema_migrations missing")
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
}

func TestEmbeddedMigrationsAreWellFormed(t *testing.T) {
	sub, err := embedded.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	if len(sub) == 0 {
		t.Fatal("no embedded files")
	}
	fsys, err := fsSub()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadMigrations(fsys); err != nil {
		t.Errorf("embedded migrations: %v", err)
	}
}

func TestOpenEmptyPath(t *testing.T) {
	if _, err := Open(context.Background(), ""); err == nil {
		t.Error("want an error for an empty path")
	}
}

func TestOpenPathWithSpecialCharacters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "odd dir?x=1&y %41 #z")
	path := filepath.Join(dir, FileName)
	s := openTest(t, path, itemsFS())
	if _, err := s.Writer().Exec(`INSERT INTO items (worker, n) VALUES (1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("database is not at the requested path: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("stray files next to the data directory: %v", entries)
	}
}

// pragmas reads the three settings from one connection.
func pragmas(t *testing.T, conn *sql.Conn) (journal string, foreignKeys, busyTimeout int) {
	t.Helper()
	ctx := context.Background()
	if err := conn.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	return journal, foreignKeys, busyTimeout
}

func TestPragmas(t *testing.T) {
	s := openTest(t, filepath.Join(t.TempDir(), FileName), itemsFS())
	ctx := context.Background()

	check := func(label string, conn *sql.Conn) {
		t.Helper()
		journal, foreignKeys, busyTimeout := pragmas(t, conn)
		if journal != "wal" {
			t.Errorf("%s: journal_mode = %q, want wal", label, journal)
		}
		if foreignKeys != 1 {
			t.Errorf("%s: foreign_keys = %d, want 1", label, foreignKeys)
		}
		if want := int(BusyTimeout.Milliseconds()); busyTimeout != want || want <= 0 {
			t.Errorf("%s: busy_timeout = %d, want %d", label, busyTimeout, want)
		}
	}

	w, err := s.Writer().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	check("writer", w)
	w.Close()

	// foreign_keys and busy_timeout are per connection, so hold several
	// reader connections at once to see that each new one gets them.
	var conns []*sql.Conn
	for i := range 4 {
		c, err := s.Reader().Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		check(fmt.Sprintf("reader %d", i), c)
	}
	for _, c := range conns {
		c.Close()
	}
}

func TestWALSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s, err := open(context.Background(), path, itemsFS())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTest(t, path, itemsFS())
	var journal string
	if err := s.Reader().QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode after reopen = %q, want wal", journal)
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	fsys := files("0001_fk.sql", `
		CREATE TABLE parent (id INTEGER PRIMARY KEY);
		CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL REFERENCES parent(id));
	`)
	s := openTest(t, filepath.Join(t.TempDir(), FileName), fsys)
	_, err := s.Writer().Exec(`INSERT INTO child (parent_id) VALUES (99)`)
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("insert with a missing parent: got %v, want a foreign key error", err)
	}
	err = s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO child (parent_id) VALUES (99)`)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("insert in a transaction with a missing parent: got %v, want a foreign key error", err)
	}
	if _, err := s.Writer().Exec(`INSERT INTO parent (id) VALUES (1); INSERT INTO child (parent_id) VALUES (1)`); err != nil {
		t.Fatalf("valid insert: %v", err)
	}
}

func TestConcurrentWritersNeverSeeDatabaseLocked(t *testing.T) {
	s := openTest(t, filepath.Join(t.TempDir(), FileName), itemsFS())
	ctx := context.Background()

	const writers, perWriter, readers = 32, 40, 8

	var wg sync.WaitGroup
	errs := make(chan error, writers+readers)
	stop := make(chan struct{})

	// Readers run the whole time, some of them inside transactions that
	// hold a snapshot open while the writers work.
	var readerWG sync.WaitGroup
	for r := range readers {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				var n int
				var err error
				if r%2 == 0 {
					err = s.Reader().QueryRowContext(ctx, `SELECT count(*) FROM items`).Scan(&n)
				} else {
					var tx *sql.Tx
					tx, err = s.Reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
					if err == nil {
						err = tx.QueryRowContext(ctx, `SELECT count(*) FROM items`).Scan(&n)
						if err == nil {
							err = tx.QueryRowContext(ctx, `SELECT coalesce(sum(n), 0) FROM items`).Scan(&n)
						}
						tx.Rollback()
					}
				}
				if err != nil {
					errs <- fmt.Errorf("reader %d: %w", r, err)
					return
				}
			}
		}()
	}

	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWriter {
				var err error
				switch i % 3 {
				case 0: // a single statement
					_, err = s.Writer().ExecContext(ctx, `INSERT INTO items (worker, n) VALUES (?, ?)`, w, i)
				case 1: // a transaction that reads, then writes
					err = s.WriteTx(ctx, func(tx *sql.Tx) error {
						var n int
						if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM items WHERE worker = ?`, w).Scan(&n); err != nil {
							return err
						}
						_, err := tx.ExecContext(ctx, `INSERT INTO items (worker, n) VALUES (?, ?)`, w, i)
						return err
					})
				case 2: // a transaction begun by hand on the writer
					var tx *sql.Tx
					tx, err = s.Writer().BeginTx(ctx, nil)
					if err == nil {
						_, err = tx.ExecContext(ctx, `INSERT INTO items (worker, n) VALUES (?, ?)`, w, i)
						if err == nil {
							err = tx.Commit()
						} else {
							tx.Rollback()
						}
					}
				}
				if err != nil {
					errs <- fmt.Errorf("writer %d, write %d: %w", w, i, err)
					return
				}
			}
		}()
	}

	wg.Wait()
	close(stop)
	readerWG.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	var n int
	if err := s.Reader().QueryRow(`SELECT count(*) FROM items`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if want := writers * perWriter; n != want {
		t.Errorf("rows written = %d, want %d", n, want)
	}
}

func TestWriterIsASingleConnection(t *testing.T) {
	s := openTest(t, filepath.Join(t.TempDir(), FileName), itemsFS())
	if got := s.Writer().Stats().MaxOpenConnections; got != 1 {
		t.Errorf("writer MaxOpenConnections = %d, want 1", got)
	}
	if got := s.Reader().Stats().MaxOpenConnections; got < 2 {
		t.Errorf("reader MaxOpenConnections = %d, want several", got)
	}
}

func TestReaderRejectsWrites(t *testing.T) {
	s := openTest(t, filepath.Join(t.TempDir(), FileName), itemsFS())
	if _, err := s.Reader().Exec(`INSERT INTO items (worker, n) VALUES (1, 1)`); err == nil {
		t.Error("write through the reader succeeded, want an error")
	}
	var n int
	if err := s.Reader().QueryRow(`SELECT count(*) FROM items`).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows = %d, %v; want 0", n, err)
	}
}

func TestReaderSeesCommittedWrites(t *testing.T) {
	s := openTest(t, filepath.Join(t.TempDir(), FileName), itemsFS())
	for i := range 20 {
		if _, err := s.Writer().Exec(`INSERT INTO items (worker, n) VALUES (1, ?)`, i); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := s.Reader().QueryRow(`SELECT count(*) FROM items`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != i+1 {
			t.Fatalf("reader sees %d rows after %d writes", n, i+1)
		}
	}
}

func TestWriteTxCommitsAndRollsBack(t *testing.T) {
	s := openTest(t, filepath.Join(t.TempDir(), FileName), itemsFS())
	ctx := context.Background()
	count := func() int {
		t.Helper()
		var n int
		if err := s.Reader().QueryRow(`SELECT count(*) FROM items`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	insert := func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO items (worker, n) VALUES (1, 1)`)
		return err
	}

	if err := s.WriteTx(ctx, insert); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := count(); got != 1 {
		t.Errorf("after commit: %d rows, want 1", got)
	}

	boom := errors.New("boom")
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		if err := insert(tx); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want boom", err)
	}
	if got := count(); got != 1 {
		t.Errorf("after rollback: %d rows, want 1", got)
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic was swallowed")
			}
		}()
		s.WriteTx(ctx, func(tx *sql.Tx) error {
			if err := insert(tx); err != nil {
				return err
			}
			panic("boom")
		})
	}()
	if got := count(); got != 1 {
		t.Errorf("after panic: %d rows, want 1", got)
	}

	// The connection was released each time: the writer still works.
	if err := s.WriteTx(ctx, insert); err != nil {
		t.Fatalf("write after rollbacks: %v", err)
	}
	if got := count(); got != 2 {
		t.Errorf("at the end: %d rows, want 2", got)
	}
}
