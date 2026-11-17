package store

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func fsSub() (fs.FS, error) { return fs.Sub(embedded, "migrations") }

func TestLoadMigrationsOrdersByVersion(t *testing.T) {
	got, err := loadMigrations(files(
		"0010_ten.sql", "SELECT 10;",
		"0002_two.sql", "SELECT 2;",
		"1_one.sql", "SELECT 1;",
		"README.md", "ignored",
		"notes.txt", "ignored",
	))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range got {
		names = append(names, m.name)
	}
	if s := strings.Join(names, ","); s != "one,two,ten" {
		t.Errorf("order = %s, want one,two,ten", s)
	}
	if got[2].version != 10 || got[2].sql != "SELECT 10;" {
		t.Errorf("last migration = %+v", got[2])
	}
}

func TestLoadMigrationsIgnoresSubdirectories(t *testing.T) {
	got, err := loadMigrations(files("old/0001_a.sql", "SELECT 1;"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d migrations, want 0", len(got))
	}
}

func TestLoadMigrationsRejectsBadFiles(t *testing.T) {
	cases := []struct {
		name string
		fsys fstest.MapFS
		want string
	}{
		{"no version", files("init.sql", ""), "name must look like"},
		{"no name", files("0001.sql", ""), "name must look like"},
		{"upper case", files("0001_Init.sql", ""), "name must look like"},
		{"dash", files("0001-init.sql", ""), "name must look like"},
		{"version zero", files("0000_init.sql", ""), "from 1 up"},
		{"version too large", files("99999999999999999999_init.sql", ""), "from 1 up"},
		{"duplicate version", files("0001_a.sql", "", "01_b.sql", ""), "same version 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadMigrations(tc.fsys)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestBadMigrationFileFailsOpen(t *testing.T) {
	_, err := open(context.Background(), filepath.Join(t.TempDir(), FileName), files("init.sql", "SELECT 1;"))
	if err == nil {
		t.Fatal("open succeeded with a malformed migration name")
	}
}

func TestFailedMigrationRollsBackCompletely(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	fsys := files(
		"0001_a.sql", `CREATE TABLE a (id INTEGER PRIMARY KEY);`,
		"0002_broken.sql", `CREATE TABLE b (id INTEGER PRIMARY KEY); INSERT INTO a (id) VALUES (7); INSERT INTO missing (id) VALUES (1);`,
		"0003_c.sql", `CREATE TABLE c (id INTEGER PRIMARY KEY);`,
	)
	_, err := open(context.Background(), path, fsys)
	if err == nil {
		t.Fatal("open succeeded with a broken migration")
	}
	if !strings.Contains(err.Error(), "migration 2 (broken)") {
		t.Errorf("error does not name the migration: %v", err)
	}

	// Look at what is on disk using only the good first migration.
	s, err := open(context.Background(), path, files("0001_a.sql", `CREATE TABLE a (id INTEGER PRIMARY KEY);`))
	if err != nil {
		t.Fatalf("open after failure: %v", err)
	}
	if got := strings.Join(appliedList(t, s.Reader()), ","); got != "1:a" {
		t.Errorf("applied = %q, want only 1:a", got)
	}
	if tableExists(t, s.Reader(), "b") {
		t.Error("table b exists: the broken migration was partly applied")
	}
	if tableExists(t, s.Reader(), "c") {
		t.Error("table c exists: a migration ran after the broken one")
	}
	var n int
	if err := s.Reader().QueryRow(`SELECT count(*) FROM a`).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows in a = %d, %v; want 0", n, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// With the migration fixed, the rest applies.
	fsys["0002_broken.sql"] = &fstest.MapFile{Data: []byte(`CREATE TABLE b (id INTEGER PRIMARY KEY);`)}
	s = openTest(t, path, fsys)
	if got := strings.Join(appliedList(t, s.Reader()), ","); got != "1:a,2:broken,3:c" {
		t.Errorf("applied after the fix = %q", got)
	}
}

func TestDatabaseNewerThanBinaryIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s, err := open(context.Background(), path, files(
		"0001_a.sql", `CREATE TABLE a (id INTEGER PRIMARY KEY);`,
		"0002_b.sql", `CREATE TABLE b (id INTEGER PRIMARY KEY);`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = open(context.Background(), path, files("0001_a.sql", `CREATE TABLE a (id INTEGER PRIMARY KEY);`))
	if err == nil || !strings.Contains(err.Error(), "newer pogfarm") {
		t.Errorf("error = %v, want one about a newer pogfarm", err)
	}
}

func TestOutOfOrderMigrationIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	fsys := files(
		"0001_a.sql", `CREATE TABLE a (id INTEGER PRIMARY KEY);`,
		"0003_c.sql", `CREATE TABLE c (id INTEGER PRIMARY KEY);`,
	)
	s, err := open(context.Background(), path, fsys)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	fsys["0002_b.sql"] = &fstest.MapFile{Data: []byte(`CREATE TABLE b (id INTEGER PRIMARY KEY);`)}
	_, err = open(context.Background(), path, fsys)
	if err == nil || !strings.Contains(err.Error(), "next free number") {
		t.Errorf("error = %v, want one about taking the next free number", err)
	}
}
