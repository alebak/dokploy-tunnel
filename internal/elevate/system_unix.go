//go:build !windows

package elevate

// System returns the Elevator for this operating system: sudo.
func System() Elevator { return Sudo{} }
