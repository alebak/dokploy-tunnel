// Package version exposes build metadata injected at link time by GoReleaser.
package version

import "fmt"

// These values are overridden at build time via -ldflags "-X ...".
var (
	// Version is the semantic version of the build, without the "v" prefix.
	Version = "dev"
	// Commit is the git commit the build was produced from.
	Commit = "none"
	// Date is the build timestamp in RFC 3339 format.
	Date = "unknown"
)

// String returns a human-readable description of the build for the named binary.
func String(binary string) string {
	return fmt.Sprintf("%s %s (commit %s, built %s)", binary, Version, Commit, Date)
}
