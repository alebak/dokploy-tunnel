//go:build platform

package platformtest

import (
	"net/netip"
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
