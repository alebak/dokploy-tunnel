//go:build !unix && !windows

package runstate

// alive cannot check other systems, so it assumes the process is running:
// a stale file is then listed instead of being removed.
func alive(int) bool { return true }
