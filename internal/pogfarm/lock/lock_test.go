//go:build unix

package lock

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAcquireExcludesSecondHolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	first, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir); !errors.Is(err, ErrHeld) {
		t.Fatalf("second Acquire: got %v, want ErrHeld", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(data)), strconv.Itoa(os.Getpid()); got != want {
		t.Errorf("lock file holds %q, want the pid %q", got, want)
	}

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Errorf("second Release: %v", err)
	}
	again, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	if err := again.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireEmptyDir(t *testing.T) {
	if _, err := Acquire(""); err == nil {
		t.Fatal("Acquire of an empty directory succeeded")
	}
}

func TestAcquireUnusableDir(t *testing.T) {
	// A regular file where the directory should be.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Acquire(file)
	if err == nil || errors.Is(err, ErrHeld) {
		t.Fatalf("got %v, want a non-ErrHeld error", err)
	}
}
