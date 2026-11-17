package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// migration is one numbered SQL file.
type migration struct {
	version int64
	name    string
	sql     string
}

var migrationFilePattern = regexp.MustCompile(`^([0-9]+)_([a-z0-9_]+)\.sql$`)

// loadMigrations reads the .sql files at the top level of fsys and returns
// them ordered by version. A .sql file with a malformed name, a version below
// 1 or a version another file already uses is an error, so a mistake is caught
// at startup instead of silently skipping a migration.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	var migrations []migration
	seen := map[int64]string{}
	for _, entry := range entries {
		file := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(file, ".sql") {
			continue
		}
		m := migrationFilePattern.FindStringSubmatch(file)
		if m == nil {
			return nil, fmt.Errorf("migration %q: name must look like 0001_short_name.sql", file)
		}
		version, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil || version < 1 {
			return nil, fmt.Errorf("migration %q: version must be a whole number from 1 up", file)
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("migrations %q and %q have the same version %d", other, file, version)
		}
		seen[version] = file
		body, err := fs.ReadFile(fsys, file)
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", file, err)
		}
		migrations = append(migrations, migration{version: version, name: m[2], sql: string(body)})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	return migrations, nil
}

// migrate brings the database up to date with the migration files in fsys.
// Each pending file runs in its own transaction together with the row that
// records it, so a migration is either fully applied and recorded or not
// applied at all.
func migrate(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	migrations, err := loadMigrations(fsys)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return err
	}
	known := map[int64]bool{}
	for _, m := range migrations {
		known[m.version] = true
	}
	var newest int64
	for version := range applied {
		if !known[version] {
			return fmt.Errorf("database has migration %d, which this binary does not know: it was written by a newer pogfarm", version)
		}
		newest = max(newest, version)
	}

	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		if m.version < newest {
			return fmt.Errorf("migration %d (%s) is older than migration %d, which is already applied: give it the next free number", m.version, m.name, newest)
		}
		err := writeTx(ctx, db, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, m.sql); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx,
				`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
				m.version, m.name, time.Now().UTC().Format(time.RFC3339))
			return err
		})
		if err != nil {
			return fmt.Errorf("apply migration %d (%s): %w", m.version, m.name, err)
		}
	}
	return nil
}

func appliedVersions(ctx context.Context, db *sql.DB) (map[int64]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	applied := map[int64]bool{}
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("read schema_migrations: %w", err)
		}
		applied[version] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	return applied, nil
}
