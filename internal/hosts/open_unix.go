//go:build !windows

package hosts

import (
	"os"
	"syscall"
)

// openNoFollow opens path for reading without following a symbolic link in
// its last element, and without blocking on a FIFO swapped in after the
// caller checked it.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
