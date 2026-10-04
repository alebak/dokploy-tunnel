// Package platformtest verifies doktunnel's platform assumptions on real
// operating systems: that leased addresses in 127.77.0.0/16 can be bound
// (natively on Linux and Windows, after a lo0 alias on macOS), that the
// hosts file round trip works with real administrator privileges, and that
// the result resolves through the system resolver.
//
// These tests change the machine they run on: they run sudo, add and remove
// macOS loopback aliases and rewrite the system hosts file. They are only
// built with the "platform" build tag and, even then, skip unless Enabled
// reports a disposable GitHub Actions runner that opted in. The platform
// workflow (.github/workflows/platform.yml) is the only place that runs
// them; never run them on a developer machine.
package platformtest

// EnvVar opts in to the platform tests when set to "1". Only the platform
// workflow sets it.
const EnvVar = "DOKTUNNEL_PLATFORM_TESTS"

// Enabled reports whether the platform tests may run: EnvVar must be "1"
// and the process must run on GitHub Actions, whose hosted runners are
// disposable machines with passwordless sudo (Linux, macOS) or an
// Administrator account (Windows). Both are required, so neither a stray
// variable in a developer's shell nor a CI checkout alone enables them.
func Enabled(getenv func(string) string) bool {
	return getenv(EnvVar) == "1" && getenv("GITHUB_ACTIONS") == "true"
}
