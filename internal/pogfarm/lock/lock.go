// Package lock makes sure only one pogfarm uses a data directory.
//
// pogfarm must run exactly once: two instances on the same data directory
// would hand out the same jobs and write the same database. At startup
// pogfarm takes an exclusive lock on a file in the data directory and gives
// up if another process holds it. The operating system releases the lock
// when the process ends, however it ends, so a crash never leaves a stale
// lock behind.
package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// FileName is the name of the lock file inside the data directory.
const FileName = "pogfarm.lock"

// ErrHeld is returned by Acquire when another holder has the lock.
var ErrHeld = errors.New("lock: another pogfarm is using this data directory")

// Lock is a held lock on a data directory.
type Lock struct {
	f *os.File
}

// Acquire takes the lock on dataDir, creating the directory and the lock
// file when they do not exist. It does not wait: when another process (or
// another Acquire in this process) holds the lock it fails with an error
// matching ErrHeld. The caller must Release the lock.
//
// The lock file records the process id of the holder, for people. Its
// content is never read back; only the operating system lock counts.
func Acquire(dataDir string) (*Lock, error) {
	if dataDir == "" {
		return nil, errors.New("lock: data directory is empty")
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("lock: create data directory: %w", err)
	}
	path := filepath.Join(dataDir, FileName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o640)
	if err != nil {
		return nil, fmt.Errorf("lock: open %s: %w", path, err)
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		if errors.Is(err, ErrHeld) {
			return nil, fmt.Errorf("%w (%s)", ErrHeld, path)
		}
		return nil, fmt.Errorf("lock: %s: %w", path, err)
	}
	// Best effort: the pid is informational.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &Lock{f: f}, nil
}

// Release gives the lock up. The file stays in place: removing it could let
// a second process lock a file the first one is about to recreate.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := errors.Join(unlockFile(l.f), l.f.Close())
	l.f = nil
	if err != nil {
		return fmt.Errorf("lock: release: %w", err)
	}
	return nil
}
