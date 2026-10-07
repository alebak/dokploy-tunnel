package runstate

// Alive reports whether a process with the given PID is running, including
// one owned by another user.
//
// It cannot tell a forward process from an unrelated one that reused its
// PID after the forward process was killed: the process start time is not
// readable portably. Forward processes remove their own files when they
// exit normally, so this only matters for files left by a killed process
// whose PID the system has handed out again.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return alive(pid)
}
