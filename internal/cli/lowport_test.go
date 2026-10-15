package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
	"github.com/alebak/dokploy-tunnel/internal/lowport"
)

// fakeBroker stands in for lowport.System on every OS: Bind opens a
// listener on a free port of 127.0.0.1 for each requested address, and Send
// and Report deliver, in-process, to the receiver with that name.
type fakeBroker struct {
	mu        sync.Mutex
	receivers map[string]*fakeReceiver
	// bound are the addresses the helper was asked to bind, in order.
	bound []netip.AddrPort
	// listeners are every listener Bind opened, keyed by requested address.
	listeners map[netip.AddrPort]net.Listener
	// sendUIDs are the peer uids the helper required.
	sendUIDs []int
	// failBind makes Bind fail like a port already in use.
	failBind bool
	// failSend makes Send connect but hang up before handing anything over.
	failSend bool
}

func newFakeBroker() *fakeBroker {
	return &fakeBroker{receivers: map[string]*fakeReceiver{}, listeners: map[netip.AddrPort]net.Listener{}}
}

func (b *fakeBroker) Open() (lowport.Receiver, error) {
	name, err := lowport.NewName()
	if err != nil {
		return nil, err
	}
	r := &fakeReceiver{name: name, ch: make(chan delivery, 1)}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.receivers[name] = r
	return r, nil
}

func (b *fakeBroker) Bind(addrs []netip.AddrPort) ([]lowport.Bound, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bound = append(b.bound, addrs...)
	if b.failBind {
		return nil, &lowport.BindError{Addr: addrs[0], Err: errors.New("address already in use")}
	}
	var out []lowport.Bound
	for _, a := range addrs {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		b.listeners[a] = ln
		out = append(out, lowport.Bound{Addr: a, Listener: helperCopy{ln}})
	}
	return out, nil
}

func (b *fakeBroker) Send(name string, peerUID int, bound []lowport.Bound) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sendUIDs = append(b.sendUIDs, peerUID)
	r := b.receivers[name]
	if r == nil {
		return errors.New("no receiver with that name")
	}
	if b.failSend {
		r.ch <- delivery{}
		return errors.New("sending the listener: broken pipe")
	}
	// The receiver gets the listeners themselves, as SCM_RIGHTS hands over
	// duplicates of the helper's descriptors.
	var sent []lowport.Bound
	for _, bd := range bound {
		sent = append(sent, lowport.Bound{Addr: bd.Addr, Listener: b.listeners[bd.Addr]})
	}
	r.ch <- delivery{bound: sent}
	return nil
}

func (b *fakeBroker) Report(name string, peerUID int, failure error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.receivers[name]
	if r == nil {
		return errors.New("no receiver with that name")
	}
	r.ch <- delivery{failure: lowport.Failure(failure)}
	return nil
}

// delivery is what one helper connection carries: listeners or a failure.
type delivery struct {
	bound   []lowport.Bound
	failure *lowport.HelperError
}

// helperCopy is the helper's own descriptor of a listener: closing it, as
// the helper does when it exits, leaves the copy sent to forward open.
type helperCopy struct{ net.Listener }

func (helperCopy) Close() error { return nil }

// listener returns the listener Bind opened for addr.
func (b *fakeBroker) listener(addr string) net.Listener {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.listeners[netip.MustParseAddrPort(addr)]
}

type fakeReceiver struct {
	name string
	ch   chan delivery
}

func (r *fakeReceiver) Name() string { return r.name }

func (r *fakeReceiver) Close() error { return nil }

// Receive, like the real one, still takes what the helper sent before the
// caller stopped waiting.
func (r *fakeReceiver) Receive(ctx context.Context, want []netip.AddrPort) (map[netip.AddrPort]net.Listener, error) {
	var d delivery
	select {
	case d = <-r.ch:
	case <-ctx.Done():
		select {
		case d = <-r.ch:
		default:
			return nil, fmt.Errorf("%w: %w", lowport.ErrNoHelper, ctx.Err())
		}
	}
	if d.failure != nil {
		return nil, d.failure
	}
	got := map[netip.AddrPort]net.Listener{}
	for _, b := range d.bound {
		got[b.Addr] = b.Listener
	}
	if len(got) != len(want) {
		for _, ln := range got {
			_ = ln.Close()
		}
		return nil, fmt.Errorf("the privileged helper sent %d of %d listeners", len(got), len(want))
	}
	return got, nil
}

// isClosed reports whether ln no longer accepts; an open one times out.
func isClosed(ln net.Listener) bool {
	if tl, ok := ln.(*net.TCPListener); ok {
		_ = tl.SetDeadline(time.Now().Add(time.Second))
	}
	c, err := ln.Accept()
	if c != nil {
		_ = c.Close()
	}
	return errors.Is(err, net.ErrClosed)
}

// lowPortHarness is a forward harness where, as on Linux, ports below 1024
// need administrator privileges.
func lowPortHarness(t *testing.T) *forwardHarness {
	t.Helper()
	h := newForwardHarness(t)
	h.hosts.privilegedPorts = true
	h.companion.ports["cmp_myapp/pgadmin"] = []int{80}
	return h
}

// helperArgs returns the flags of the helper call, without the executable
// and the "hosts privileged-apply" words.
func helperArgs(t *testing.T, call []string) []string {
	t.Helper()
	if len(call) < 3 || !slices.Equal(call[:3], []string{fakeExe, "hosts", "privileged-apply"}) {
		t.Fatalf("elevated %q, want the privileged helper", call)
	}
	return call[3:]
}

// flagValue returns the value after name in args.
func flagValue(args []string, name string) string {
	if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func TestForward_LowPortWithUnchangedHostsElevatesOnce(t *testing.T) {
	h := lowPortHarness(t)
	// The hosts file is writable, so only the port needs privileges.
	r := h.forward("", true, "myapp/pgadmin", "--json")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stdout %q, stderr %q)", r.exit, r.stdout, r.stderr)
	}
	calls := h.hosts.elevator.calls
	if len(calls) != 1 {
		t.Fatalf("elevated %d times, want once: %q", len(calls), calls)
	}
	args := helperArgs(t, calls[0])
	if got := flagValue(args, "--ports"); got != "127.77.0.1:80" {
		t.Errorf("--ports = %q, want 127.77.0.1:80 (args %q)", got, args)
	}
	if !lowport.ValidName(flagValue(args, "--socket")) {
		t.Errorf("--socket = %q, want a channel name", flagValue(args, "--socket"))
	}
	if slices.Contains(args, "--entries-file") {
		t.Errorf("the helper was handed entries although the hosts file was written directly: %q", args)
	}
	if got := h.requested(); slices.Contains(got, "127.77.0.1:80") {
		t.Errorf("forward listened on 127.77.0.1:80 itself (%q), want the helper's listener", got)
	}
	ln := h.hosts.broker.listener("127.77.0.1:80")
	if ln == nil || !isClosed(ln) {
		t.Error("the helper's listener was not used and closed on exit")
	}
}

func TestForward_LowPortAndHostsChangeElevateOnce(t *testing.T) {
	h := lowPortHarness(t)
	h.hosts.denyWrite = true
	r := h.forward("", true, "myapp/pgadmin")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stderr %q)", r.exit, r.stderr)
	}
	// Once on start for both, once on exit to remove the entry.
	calls := h.hosts.elevator.calls
	if len(calls) != 2 {
		t.Fatalf("elevated %d times, want twice (start and exit): %q", len(calls), calls)
	}
	start := helperArgs(t, calls[0])
	if flagValue(start, "--entries-file") != "-" || flagValue(start, "--ports") != "127.77.0.1:80" {
		t.Errorf("start helper args = %q, want the entries and the port together", start)
	}
	if want := "127.77.0.1\tpgadmin.shop-myapp-x1y2z3.internal\n"; h.hosts.elevator.stdins[0] != want {
		t.Errorf("start helper stdin = %q, want %q", h.hosts.elevator.stdins[0], want)
	}
	if exit := helperArgs(t, calls[1]); slices.Contains(exit, "--ports") {
		t.Errorf("exit helper args = %q, want no ports", exit)
	}
	if !strings.Contains(r.stderr, "listening on 127.77.0.1:80") {
		t.Errorf("stderr %q does not say the port needs privileges", r.stderr)
	}
	if got := h.hosts.readHosts(); got != fixtureHosts {
		t.Errorf("hosts file after exit =\n%q\nwant\n%q", got, fixtureHosts)
	}
}

func TestForward_HighPortsNeverElevate(t *testing.T) {
	h := lowPortHarness(t)
	r := h.forward("", true, "myapp/postgres", "--json")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stdout %q)", r.exit, r.stdout)
	}
	if len(h.hosts.elevator.calls) != 0 || len(h.hosts.broker.bound) != 0 {
		t.Errorf("elevated %q and bound %v for port 5432, want neither", h.hosts.elevator.calls, h.hosts.broker.bound)
	}
	if !slices.Contains(h.requested(), "127.77.0.1:5432") {
		t.Errorf("forward did not listen on 127.77.0.1:5432 itself: %q", h.requested())
	}
}

func TestForward_LowPortWithoutPrompting(t *testing.T) {
	tests := []struct {
		name     string
		terminal bool
		args     []string
		setup    func(h *forwardHarness)
		calls    int
	}{
		{name: "--no-input", terminal: true, args: []string{"--no-input"}},
		{name: "no terminal", terminal: false},
		{name: "--no-input with a hosts change", terminal: true, args: []string{"--no-input"}, setup: func(h *forwardHarness) { h.hosts.denyWrite = true }},
		{name: "elevation refused", terminal: true, setup: func(h *forwardHarness) { h.hosts.elevator.fail = true }, calls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := lowPortHarness(t)
			if tt.setup != nil {
				tt.setup(h)
			}
			r := h.forward("", tt.terminal, append([]string{"myapp/pgadmin", "--json"}, tt.args...)...)
			if want := clierr.ElevationRequired.ExitCode(); r.exit != want {
				t.Fatalf("exit = %d, want %d (stdout %q)", r.exit, want, r.stdout)
			}
			e := decodeError(t, r.stdout)
			if e.Code != clierr.ElevationRequired || !strings.Contains(e.Message, "127.77.0.1:80") {
				t.Errorf("error = %+v, want elevation_required naming 127.77.0.1:80", e)
			}
			if !strings.Contains(e.Hint, "terminal") || strings.Contains(e.Hint, "privileged-apply") {
				t.Errorf("hint %q, want one that says to run from a terminal, not a command to copy", e.Hint)
			}
			if len(h.hosts.elevator.calls) != tt.calls || len(h.requested()) != 0 {
				t.Errorf("elevated %d times and listened on %q, want %d and nothing", len(h.hosts.elevator.calls), h.requested(), tt.calls)
			}
			if got := h.hosts.readHosts(); got != fixtureHosts {
				t.Errorf("hosts file after the failure =\n%q\nwant it unchanged", got)
			}
		})
	}
}

// TestForward_LowPortHelperFailsAfterElevation checks a helper that ran, so
// the elevation worked, but could not open or hand over a port is not
// reported as elevation_required: asking for the password again would not
// help.
func TestForward_LowPortHelperFailsAfterElevation(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(b *fakeBroker)
		message  string
		wantHint string
	}{
		{
			name:     "the helper cannot bind",
			setup:    func(b *fakeBroker) { b.failBind = true },
			message:  "listening on 127.77.0.1:80: address already in use",
			wantHint: "another process may already listen on 127.77.0.1:80; run 'doktunnel status'",
		},
		{
			name:    "the helper cannot hand the listener over",
			setup:   func(b *fakeBroker) { b.failSend = true },
			message: "receiving the listeners from the privileged helper",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := lowPortHarness(t)
			tt.setup(h.hosts.broker)
			r := h.forward("", true, "myapp/pgadmin", "--json")
			if want := clierr.Internal.ExitCode(); r.exit != want {
				t.Fatalf("exit = %d, want %d (stdout %q)", r.exit, want, r.stdout)
			}
			e := decodeError(t, r.stdout)
			if e.Code != clierr.Internal || !strings.Contains(e.Message, "127.77.0.1:80") || !strings.Contains(e.Message, tt.message) {
				t.Errorf("error = %+v, want internal naming 127.77.0.1:80 and %q", e, tt.message)
			}
			if tt.wantHint != "" && !strings.Contains(e.Hint, tt.wantHint) {
				t.Errorf("hint %q, want it to contain %q", e.Hint, tt.wantHint)
			}
			if strings.Contains(e.Hint, "terminal") || strings.Contains(e.Hint, "password") {
				t.Errorf("hint %q asks to elevate again", e.Hint)
			}
			if len(h.hosts.elevator.calls) != 1 || len(h.requested()) != 0 {
				t.Errorf("elevated %d times and listened on %q, want once and nothing", len(h.hosts.elevator.calls), h.requested())
			}
			if got := h.hosts.readHosts(); got != fixtureHosts {
				t.Errorf("hosts file after the failure =\n%q\nwant it unchanged", got)
			}
		})
	}
}

// TestForward_LaterBindFailureClosesReceivedListeners checks the
// all-or-nothing rule with a listener the helper opened but forward never
// reached.
func TestForward_LaterBindFailureClosesReceivedListeners(t *testing.T) {
	h := lowPortHarness(t)
	// postgres comes first and fails, so pgadmin's port 80 is never used.
	h.busy = map[string]bool{"127.77.0.1:5432": true}
	r := h.forward("", true, "myapp/postgres", "myapp/pgadmin", "--json")
	if want := clierr.Internal.ExitCode(); r.exit != want {
		t.Fatalf("exit = %d, want %d (stdout %q)", r.exit, want, r.stdout)
	}
	ln := h.hosts.broker.listener("127.77.0.2:80")
	if ln == nil {
		t.Fatalf("the helper did not bind 127.77.0.2:80 (bound %v)", h.hosts.broker.bound)
	}
	if !isClosed(ln) {
		t.Error("the received listener for 127.77.0.2:80 still accepts after the forward failed")
	}
}

// TestForward_LowPortsOfSeveralServicesInOneElevation checks every low port
// of every service travels in the one elevation, and high ports in none.
func TestForward_LowPortsOfSeveralServicesInOneElevation(t *testing.T) {
	h := lowPortHarness(t)
	h.companion.ports["cmp_myapp/pgadmin"] = []int{80, 443}
	r := h.forward("", true, "myapp/pgadmin", "myapp/postgres", "--all-ports", "--json")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stdout %q)", r.exit, r.stdout)
	}
	if len(h.hosts.elevator.calls) != 1 {
		t.Fatalf("elevated %d times, want once", len(h.hosts.elevator.calls))
	}
	if got := flagValue(helperArgs(t, h.hosts.elevator.calls[0]), "--ports"); got != "127.77.0.1:80,127.77.0.1:443" {
		t.Errorf("--ports = %q, want both low ports and not 5432", got)
	}
}

func TestHostsPrivilegedApply_Ports(t *testing.T) {
	h := newHostsHarness(t)
	h.privilegedPorts = true
	h.env["SUDO_UID"] = "1000"
	recv, err := h.broker.Open()
	if err != nil {
		t.Fatal(err)
	}
	r := h.runAs(true, false, "hosts", "privileged-apply", "--ports", "127.77.0.5:80,127.77.0.5:443", "--socket", recv.Name())
	if r.exit != 0 {
		t.Fatalf("exit = %d (stderr %q)", r.exit, r.stderr)
	}
	got, err := recv.Receive(context.Background(), []netip.AddrPort{netip.MustParseAddrPort("127.77.0.5:80"), netip.MustParseAddrPort("127.77.0.5:443")})
	if err != nil || len(got) != 2 {
		t.Fatalf("received %v, %v; want two listeners", got, err)
	}
	if !slices.Equal(h.broker.sendUIDs, []int{1000}) {
		t.Errorf("the helper required peer uids %v, want SUDO_UID 1000", h.broker.sendUIDs)
	}
	if h.readHosts() != fixtureHosts {
		t.Error("a ports-only request changed the hosts file")
	}
}

func TestHostsPrivilegedApply_RejectsInvalidPorts(t *testing.T) {
	name, err := lowport.NewName()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args []string
		env  map[string]string
	}{
		{name: "address outside 127.77.0.0/16", args: []string{"--ports", "127.0.0.1:80", "--socket", name}},
		{name: "port at the limit", args: []string{"--ports", "127.77.0.5:1024", "--socket", name}},
		{name: "high port", args: []string{"--ports", "127.77.0.5:5432", "--socket", name}},
		{name: "port zero", args: []string{"--ports", "127.77.0.5:0", "--socket", name}},
		{name: "duplicate", args: []string{"--ports", "127.77.0.5:80,127.77.0.5:80", "--socket", name}},
		{name: "ports without a socket", args: []string{"--ports", "127.77.0.5:80"}},
		{name: "socket without ports", args: []string{"--entries-file", "-", "--socket", name}},
		{name: "a path instead of a socket name", args: []string{"--ports", "127.77.0.5:80", "--socket", "/tmp/evil.sock"}},
		{name: "ports with --clean", args: []string{"--clean", "--ports", "127.77.0.5:80", "--socket", name}},
		{name: "invalid SUDO_UID", args: []string{"--ports", "127.77.0.5:80", "--socket", name}, env: map[string]string{"SUDO_UID": "root"}},
		{name: "invalid entries with valid ports", args: []string{"--entries-file", "-", "--ports", "127.77.0.5:80", "--socket", name}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHostsHarness(t)
			h.privilegedPorts = true
			for k, v := range tt.env {
				h.env[k] = v
			}
			r := h.runWithStdin(true, false, "10.0.0.1\tevil.example.com\n", append([]string{"hosts", "privileged-apply"}, tt.args...)...)
			if r.exit != clierr.InvalidArgument.ExitCode() {
				t.Errorf("exit = %d, want %d (stderr %q)", r.exit, clierr.InvalidArgument.ExitCode(), r.stderr)
			}
			if len(h.broker.bound) != 0 {
				t.Errorf("bound %v, want nothing", h.broker.bound)
			}
			if h.readHosts() != fixtureHosts {
				t.Error("hosts file changed")
			}
		})
	}
}

// TestHostsPrivilegedApply_BindFailureChangesNothing checks the helper binds
// before it writes the hosts file.
func TestHostsPrivilegedApply_BindFailureChangesNothing(t *testing.T) {
	h := newHostsHarness(t)
	h.privilegedPorts = true
	h.broker.failBind = true
	recv, err := h.broker.Open()
	if err != nil {
		t.Fatal(err)
	}
	r := h.runWithStdin(true, false, fixtureEntries, "hosts", "privileged-apply", "--entries-file", "-", "--ports", "127.77.0.1:80", "--socket", recv.Name())
	if r.exit == 0 || !strings.Contains(r.stderr, "already in use") {
		t.Fatalf("exit = %d, stderr %q; want the bind failure", r.exit, r.stderr)
	}
	if h.readHosts() != fixtureHosts {
		t.Error("hosts file changed although the port could not be bound")
	}
}

// TestHostsSync_UnchangedByLowPorts checks "hosts sync" never asks for
// ports, whatever the forwards listen on.
func TestHostsSync_UnchangedByLowPorts(t *testing.T) {
	h := newHostsHarness(t)
	h.privilegedPorts = true
	h.leaseFixture()
	h.writeHosts(fixtureHosts + fixtureBlock)
	if r := h.run(true, "hosts", "sync"); r.exit != 0 || len(h.elevator.calls) != 0 {
		t.Fatalf("exit = %d, elevations %q; want 0 and none", r.exit, h.elevator.calls)
	}
}
