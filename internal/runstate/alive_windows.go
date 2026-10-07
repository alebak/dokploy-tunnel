//go:build windows

package runstate

import (
	"errors"
	"syscall"
)

const (
	// processQueryLimitedInformation is the least access right that allows
	// GetExitCodeProcess; the syscall package does not define it.
	processQueryLimitedInformation = 0x1000
	// stillActive is the exit code of a process that has not exited.
	stillActive = 259
)

// alive opens the process and checks that it has no exit code yet. A
// process that cannot be opened for lack of rights exists; any other
// failure, usually ERROR_INVALID_PARAMETER, means there is no such process.
// A process that exited with code 259 reads as alive, a documented
// limitation of GetExitCodeProcess.
func alive(pid int) bool {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return errors.Is(err, syscall.ERROR_ACCESS_DENIED)
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		// The process exists, since it could be opened.
		return true
	}
	return code == stillActive
}
