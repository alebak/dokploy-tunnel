package hosts

import (
	"context"
	"fmt"
	"net/netip"
	"os/exec"
	"runtime"
	"strings"
)

// Loopback manages the loopback addresses leased services listen on.
//
// Linux routes all of 127.0.0.0/8 to the loopback interface, so nothing has
// to be configured there. macOS only answers 127.0.0.1 until each further
// address is added to lo0 as an alias, which needs root and does not survive
// a reboot. Windows is expected to behave like Linux; that is verified
// separately, and this interface is the seam for any change it needs.
type Loopback interface {
	// Missing returns the addresses in ips that are not configured yet,
	// in the given order. It needs no privileges.
	Missing(ctx context.Context, ips []netip.Addr) ([]netip.Addr, error)
	// Add configures ips. It needs administrator privileges.
	Add(ctx context.Context, ips []netip.Addr) error
}

// Runner runs a command and returns its combined output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// SystemLoopback returns the Loopback for this operating system.
func SystemLoopback() Loopback {
	return loopbackFor(runtime.GOOS, execRunner)
}

func loopbackFor(goos string, run Runner) Loopback {
	if goos == "darwin" {
		return ifconfigLoopback{run: run}
	}
	return noLoopback{}
}

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// noLoopback is the Loopback of systems that route all of 127.0.0.0/8.
type noLoopback struct{}

func (noLoopback) Missing(context.Context, []netip.Addr) ([]netip.Addr, error) { return nil, nil }

func (noLoopback) Add(context.Context, []netip.Addr) error { return nil }

// ifconfigPath is macOS's ifconfig. The privileged helper runs it as root,
// and sudo on macOS keeps the caller's PATH, so it is never looked up there.
const ifconfigPath = "/sbin/ifconfig"

// ifconfigLoopback manages lo0 aliases with ifconfig, as macOS needs.
type ifconfigLoopback struct {
	run Runner
}

func (l ifconfigLoopback) Missing(ctx context.Context, ips []netip.Addr) ([]netip.Addr, error) {
	if len(ips) == 0 {
		return nil, nil
	}
	out, err := l.run(ctx, ifconfigPath, "lo0")
	if err != nil {
		return nil, fmt.Errorf("reading lo0 addresses: %w: %s", err, strings.TrimSpace(string(out)))
	}
	present := map[netip.Addr]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "inet" {
			if ip, err := netip.ParseAddr(fields[1]); err == nil {
				present[ip] = true
			}
		}
	}
	var missing []netip.Addr
	for _, ip := range ips {
		if !present[ip] {
			missing = append(missing, ip)
		}
	}
	return missing, nil
}

func (l ifconfigLoopback) Add(ctx context.Context, ips []netip.Addr) error {
	for _, ip := range ips {
		if out, err := l.run(ctx, ifconfigPath, "lo0", "alias", ip.String(), "up"); err != nil {
			return fmt.Errorf("adding lo0 alias %s: %w: %s", ip, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
