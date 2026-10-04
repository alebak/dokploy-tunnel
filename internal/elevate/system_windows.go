//go:build windows

package elevate

// System returns the Elevator for this operating system: a UAC prompt.
func System() Elevator { return UAC{} }
