//go:build unix

package registry

import (
	"os"
	"path/filepath"
	"syscall"
)

// lockFile blocks until it holds an exclusive flock on path, creating the
// file and its directory when needed. The kernel releases the lock if the
// process dies, so a crashed doktunnel never leaves the registry locked.
func lockFile(path string) (unlock func() error, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	// Closing the descriptor releases the lock.
	return f.Close, nil
}
