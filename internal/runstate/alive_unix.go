//go:build unix

package runstate

import (
	"errors"
	"os"
	"syscall"
)

// alive sends signal 0, which checks that the process exists without
// affecting it. EPERM means it exists but belongs to another user.
func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	defer p.Release()
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
