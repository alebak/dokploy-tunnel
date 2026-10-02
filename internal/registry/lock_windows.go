//go:build windows

package registry

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	errSharingViolation syscall.Errno = 32 // ERROR_SHARING_VIOLATION
	lockTimeout                       = 30 * time.Second
	lockRetry                         = 10 * time.Millisecond
)

// lockFile opens path with share mode 0, which Windows grants to one handle
// at a time, retrying while another process holds it. This uses only the
// standard library (LockFileEx would need golang.org/x/sys/windows). Windows
// closes the handle if the process dies, so a crash never leaves the
// registry locked.
func lockFile(path string) (unlock func() error, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(lockTimeout)
	for {
		h, err := syscall.CreateFile(name,
			syscall.GENERIC_READ|syscall.GENERIC_WRITE,
			0, // no sharing: this handle is the lock
			nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
		if err == nil {
			return func() error { return syscall.CloseHandle(h) }, nil
		}
		if !errors.Is(err, errSharingViolation) && !errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out after %s waiting for %s", lockTimeout, path)
		}
		time.Sleep(lockRetry)
	}
}
