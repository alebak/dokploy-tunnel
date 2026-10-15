//go:build platform

package platformtest

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
	"github.com/alebak/dokploy-tunnel/internal/lowport"
	"github.com/alebak/dokploy-tunnel/internal/registry"
)

// lowProbePort is a port below 1024 that nothing on the runners listens on.
const lowProbePort = 81

// lowProbeAddr is a leased-range address no other platform test uses.
var lowProbeAddr = netip.MustParseAddr("127.77.252.1")

// TestLowPort_ListensBelowTheUnprivilegedLimit proves how forward gets a
// port below 1024. On Linux an unprivileged process cannot listen there, so
// the real doktunnel binary's privileged helper runs through real sudo,
// listens on a leased address and hands the socket to this unprivileged
// process, which then accepts a connection on it. On macOS and Windows, the
// design assumes a normal process may listen on such a port on loopback
// itself, so nothing is elevated there; this checks that assumption, which
// holds on Windows and does not on macOS (see lowPortOnMacOS).
func TestLowPort_ListensBelowTheUnprivilegedLimit(t *testing.T) {
	requirePlatform(t)
	if runtime.GOOS == "linux" {
		lowPortThroughHelper(t)
		return
	}
	lowPortDirectly(t)
}

func lowPortThroughHelper(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("the test process runs as root, so it cannot show what an unprivileged forward needs")
	}
	limit := lowport.Limit()
	report(t, "net.ipv4.ip_unprivileged_port_start is %d", limit)
	if lowProbePort >= limit {
		t.Fatalf("port %d is not below the unprivileged limit %d on this runner", lowProbePort, limit)
	}
	ap := netip.AddrPortFrom(lowProbeAddr, lowProbePort)
	if !registry.DefaultRange.Contains(ap.Addr()) {
		t.Fatalf("probe address %s is outside %s", ap.Addr(), registry.DefaultRange)
	}
	if !lowport.Privileged()(lowProbePort) {
		t.Fatalf("lowport.Privileged reports port %d as unprivileged for this process", lowProbePort)
	}

	// The reason the helper exists: the user cannot listen there.
	if ln, err := net.Listen("tcp", ap.String()); err == nil {
		_ = ln.Close()
		finding(t, "an unprivileged process listened on %s directly; low ports may not need the helper here", ap)
	} else if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("listening on %s unprivileged: %v, want permission denied", ap, err)
	} else {
		report(t, "an unprivileged process cannot listen on %s (%v)", ap, err)
	}

	exe := buildDoktunnel(t)

	// The helper refuses an address outside the leased range, as root.
	refusedName, err := lowport.NewName()
	if err != nil {
		t.Fatal(err)
	}
	out, err := sudo(nil, exe, "hosts", "privileged-apply", "--ports", "127.0.0.1:81", "--socket", refusedName)
	if code := exitCode(err); err == nil || code != clierr.InvalidArgument.ExitCode() {
		t.Fatalf("the helper accepted 127.0.0.1:81 or failed otherwise: exit %d, %v\n%s", code, err, out)
	}
	report(t, "the privileged helper, run as root, refused 127.0.0.1:81 outside 127.77.0.0/16")

	recv, err := lowport.System().Open()
	if err != nil {
		t.Fatal(err)
	}
	defer recv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	type received struct {
		listeners map[netip.AddrPort]net.Listener
		err       error
	}
	done := make(chan received, 1)
	go func() {
		ls, err := recv.Receive(ctx, []netip.AddrPort{ap})
		done <- received{ls, err}
	}()

	// sudo records this process's user in SUDO_UID, which the helper
	// requires of the channel's owner.
	if out, err := sudo(nil, exe, "hosts", "privileged-apply", "--ports", ap.String(), "--socket", recv.Name()); err != nil {
		cancel()
		<-done
		t.Fatalf("privileged helper: %v\n%s", err, out)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("receiving the listener from the helper: %v", r.err)
	}
	ln := r.listeners[ap]
	if ln == nil {
		t.Fatalf("received %v, want a listener for %s", r.listeners, ap)
	}
	defer ln.Close()
	if err := echo(ln, ap.String()); err != nil {
		t.Fatalf("connecting through the received listener: %v", err)
	}
	report(t, "the privileged helper listened on %s through sudo and handed the socket to the unprivileged process, which accepted a connection on it", ap)
}

func lowPortDirectly(t *testing.T) {
	if lowport.Privileged()(lowProbePort) {
		t.Fatalf("lowport.Privileged reports port %d as privileged on %s", lowProbePort, runtime.GOOS)
	}
	if runtime.GOOS == "darwin" {
		lowPortOnMacOS(t)
		return
	}
	// Windows answers the whole leased range natively.
	for _, ap := range []netip.AddrPort{
		netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), lowProbePort),
		netip.AddrPortFrom(lowProbeAddr, lowProbePort),
	} {
		if err := listenAndEcho(ap); err != nil {
			finding(t, "%s refused to listen on %s without elevation (%v); low ports need the privileged helper here too", runtime.GOOS, ap, err)
			t.Fatal(err)
		}
		report(t, "the test process listened on %s and accepted a connection without elevation (the runner is an elevated Administrator, "+
			"so this does not prove what a standard user may do; Windows has no privileged port range)", ap)
	}
}

// lowPortOnMacOS records what the first run of this test found: since
// macOS 10.14 a user other than root may listen on a port below 1024 only on
// the wildcard address, not on a specific one such as 127.0.0.1 or a lo0
// alias. forward listens on a specific loopback address, so on macOS a port
// below 1024 still fails with "permission denied"; the privileged helper
// only opens low ports on Linux. The test fails if that changes, so the
// README and the helper can follow.
func lowPortOnMacOS(t *testing.T) {
	specific := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), lowProbePort)
	if err := listenAndEcho(specific); err == nil {
		t.Fatalf("macOS now lets the runner user listen on %s; update the README and consider dropping this check", specific)
	} else if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("listening on %s: %v, want permission denied", specific, err)
	}
	finding(t, "macOS refused to listen on %s as the runner user (not root); forward cannot mirror ports below 1024 on macOS, where the privileged helper does not open them", specific)

	wildcard := netip.AddrPortFrom(netip.IPv4Unspecified(), lowProbePort)
	if err := listenAndEcho(wildcard); err != nil {
		report(t, "the runner user cannot listen on %s either (%v)", wildcard, err)
		return
	}
	report(t, "the runner user can listen on %s, the wildcard address forward never uses", wildcard)
}

// listenAndEcho listens on ap and proves a client can connect through it.
func listenAndEcho(ap netip.AddrPort) error {
	ln, err := net.Listen("tcp", ap.String())
	if err != nil {
		return err
	}
	defer ln.Close()
	dial := ap
	if ap.Addr().IsUnspecified() {
		dial = netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), ap.Port())
	}
	return echo(ln, dial.String())
}
