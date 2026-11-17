//go:build !unix

package lock

import (
	"errors"
	"os"
)

// File locks are only implemented with flock, on Unix. pogfarm is deployed
// on Linux; on other systems it refuses to start rather than run unguarded.

func lockFile(*os.File) error {
	return errors.New("locking the data directory is not supported on this system")
}

func unlockFile(*os.File) error { return nil }
