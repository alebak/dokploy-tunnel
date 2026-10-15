//go:build platform

package platformtest

import (
	"net"
	"net/netip"
	"os"
	"runtime"
	"testing"

	"github.com/alebak/dokploy-tunnel/internal/registry"
)

// probeAddrs are addresses of the lease range the probe binds. They sit far
// from the first leases, which the hosts round trip uses.
var probeAddrs = []string{"127.77.250.1", "127.77.250.2", "127.77.251.254"}

// TestLoopback_BindsLeasedRange proves the addressing assumption every
// forward relies on: Linux and Windows accept a listener on any address in
// 127.77.0.0/16 without configuration, and macOS accepts one only after the
// address is added to lo0 as an alias, which doktunnel does with sudo.
func TestLoopback_BindsLeasedRange(t *testing.T) {
	requirePlatform(t)
	for _, s := range probeAddrs {
		ip := netip.MustParseAddr(s)
		if !registry.DefaultRange.Contains(ip) {
			t.Fatalf("probe address %s is outside %s", ip, registry.DefaultRange)
		}
		t.Run(s, func(t *testing.T) {
			if runtime.GOOS == "darwin" {
				probeWithAlias(t, ip)
				return
			}
			if err := bindAndConnect(ip); err != nil {
				finding(t, "%s did not bind %s natively (%v); the design assumes every 127.0.0.0/8 address works without configuration, so addressing needs a %s Loopback implementation", runtime.GOOS, ip, err, runtime.GOOS)
				t.Fatalf("binding %s natively: %v", ip, err)
			}
			report(t, "bound and connected to %s natively", ip)
		})
	}
}

// probeWithAlias checks that macOS refuses ip without a lo0 alias and
// accepts it with one, then removes the alias again.
func probeWithAlias(t *testing.T, ip netip.Addr) {
	present, err := hasLo0Alias(ip)
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatalf("lo0 already has %s; the runner is not in the expected clean state", ip)
	}

	if err := bindAndConnect(ip); err == nil {
		finding(t, "macOS bound %s without a lo0 alias; the alias step in 'hosts sync' may be unnecessary", ip)
	} else {
		report(t, "refused %s without a lo0 alias, as expected (%v)", ip, err)
	}

	t.Cleanup(func() { removeLo0Alias(t, ip) })
	if _, err := sudo(nil, ifconfigPath, "lo0", "alias", ip.String(), "up"); err != nil {
		finding(t, "adding a lo0 alias for %s with sudo failed: %v", ip, err)
		t.Fatal(err)
	}
	if err := bindAndConnect(ip); err != nil {
		finding(t, "macOS still refused %s after 'ifconfig lo0 alias %s up' (%v)", ip, ip, err)
		t.Fatalf("binding %s with a lo0 alias: %v", ip, err)
	}
	report(t, "bound and connected to %s after adding a lo0 alias", ip)

	removeLo0Alias(t, ip)
	if present, err := hasLo0Alias(ip); err != nil || present {
		t.Fatalf("lo0 alias %s still present after removal (err %v)", ip, err)
	}
	report(t, "removed the lo0 alias %s", ip)
}

// remapAddr is the address the remapped low port probe listens on, apart
// from probeAddrs and the hosts round trip.
const remapAddr = "127.77.249.1"

// TestLoopback_RemappedLowPort proves that forwarding a service on port 80
// needs no privileges for the port: the local port doktunnel picks for it
// is one an unprivileged process can listen on, on a leased address, and it
// accepts connections. Whether port 80 itself could be bound is only
// reported, since doktunnel never tries it. On macOS the address needs a lo0
// alias first, which is the only step that uses sudo.
func TestLoopback_RemappedLowPort(t *testing.T) {
	requirePlatform(t)
	ip := netip.MustParseAddr(remapAddr)
	if !registry.DefaultRange.Contains(ip) {
		t.Fatalf("probe address %s is outside %s", ip, registry.DefaultRange)
	}
	if os.Geteuid() == 0 {
		t.Fatal("the probe runs as root, so it cannot show that the remapped port needs no privileges")
	}
	if runtime.GOOS == "darwin" {
		present, err := hasLo0Alias(ip)
		if err != nil {
			t.Fatal(err)
		}
		if present {
			t.Fatalf("lo0 already has %s; the runner is not in the expected clean state", ip)
		}
		t.Cleanup(func() { removeLo0Alias(t, ip) })
		if _, err := sudo(nil, ifconfigPath, "lo0", "alias", ip.String(), "up"); err != nil {
			t.Fatalf("adding a lo0 alias for %s: %v", ip, err)
		}
	}

	if ln, err := net.Listen("tcp", netip.AddrPortFrom(ip, 80).String()); err == nil {
		ln.Close()
		report(t, "%s:80 could also be bound directly by this process", ip)
	} else {
		report(t, "%s:80 cannot be bound without privileges, as expected (%v)", ip, err)
	}

	local := registry.PreferredLocalPort(80)
	if local != 8080 {
		t.Fatalf("PreferredLocalPort(80) = %d, want 8080", local)
	}
	addr := netip.AddrPortFrom(ip, uint16(local)).String()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		finding(t, "%s refused the remapped port %s to an unprivileged process (%v); forwarding port 80 would fail", runtime.GOOS, addr, err)
		t.Fatalf("listening on %s: %v", addr, err)
	}
	defer ln.Close()
	if err := echo(ln, addr); err != nil {
		t.Fatalf("connecting to %s: %v", addr, err)
	}
	report(t, "an unprivileged process listened on %s for port 80 and accepted a connection", addr)
}
