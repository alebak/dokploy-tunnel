//go:build linux

package lowport

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

// listenAny opens a TCP listener on a free port of 127.0.0.1, standing in for
// one the helper bound below the unprivileged limit.
func listenAny(t *testing.T) (net.Listener, netip.AddrPort) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln, netip.MustParseAddrPort(ln.Addr().String())
}

// TestChannel_RoundTrip passes two listeners through a real abstract socket
// with SCM_RIGHTS and proves the received ones accept connections. Both
// sides expect the test's own uid as their peer.
func TestChannel_RoundTrip(t *testing.T) {
	uid := os.Getuid()
	recv, err := openReceiver(uid)
	if err != nil {
		t.Fatal(err)
	}
	defer recv.Close()

	ln1, a1 := listenAny(t)
	ln2, a2 := listenAny(t)
	want := []netip.AddrPort{a1, a2}

	type result struct {
		got map[netip.AddrPort]net.Listener
		err error
	}
	done := make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		got, err := recv.Receive(ctx, want)
		done <- result{got, err}
	}()

	if err := send(recv.Name(), uid, []Bound{{Addr: a1, Listener: ln1}, {Addr: a2, Listener: ln2}}); err != nil {
		t.Fatalf("send: %v", err)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("Receive: %v", r.err)
	}
	if len(r.got) != 2 {
		t.Fatalf("received %d listeners, want 2", len(r.got))
	}
	// The sender's copies close, as they do when the helper exits; the
	// received ones must keep working.
	_ = ln1.Close()
	_ = ln2.Close()
	for _, a := range want {
		ln := r.got[a]
		if ln == nil {
			t.Fatalf("no listener received for %s", a)
		}
		if got := ln.Addr().String(); got != a.String() {
			t.Fatalf("received listener for %s listens on %s", a, got)
		}
		echoThrough(t, ln, a.String())
		_ = ln.Close()
	}
}

func echoThrough(t *testing.T, ln net.Listener, addr string) {
	t.Helper()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, io.LimitReader(c, 1))
	}()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dialing %s: %v", addr, err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte{'x'}); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 1)
	if _, err := io.ReadFull(c, b); err != nil || b[0] != 'x' {
		t.Fatalf("echo through %s: %q, %v", addr, b, err)
	}
}

// TestReceive_RejectsWrongPeer shows the forward side ignores a peer whose
// uid is not the one it expects (root, outside tests): its listeners never
// arrive, and Receive ends when its context does.
func TestReceive_RejectsWrongPeer(t *testing.T) {
	recv, err := openReceiver(os.Getuid() + 1)
	if err != nil {
		t.Fatal(err)
	}
	defer recv.Close()
	ln, a := listenAny(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		got, err := recv.Receive(ctx, []netip.AddrPort{a})
		for _, l := range got {
			_ = l.Close()
		}
		done <- err
	}()
	// The sender itself accepts the receiver (same uid), so only the
	// receiver's check stands between it and the listeners.
	_ = send(recv.Name(), os.Getuid(), []Bound{{Addr: a, Listener: ln}})
	err = <-done
	if err == nil {
		t.Fatal("Receive accepted listeners from a peer with the wrong uid")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Receive error = %v, want the context's deadline", err)
	}
}

// TestSend_RejectsWrongPeer shows the helper side refuses to hand listeners
// to a receiver that does not run as the user who invoked sudo.
func TestSend_RejectsWrongPeer(t *testing.T) {
	recv, err := openReceiver(os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	defer recv.Close()
	ln, a := listenAny(t)
	err = send(recv.Name(), os.Getuid()+1, []Bound{{Addr: a, Listener: ln}})
	if err == nil || !errors.Is(err, ErrPeer) {
		t.Fatalf("send to a receiver with the wrong uid: %v, want ErrPeer", err)
	}
}

// TestReceive_RefusesUnrequestedAddress shows an address that was not
// requested is refused, and nothing received is kept.
func TestReceive_RefusesUnrequestedAddress(t *testing.T) {
	uid := os.Getuid()
	recv, err := openReceiver(uid)
	if err != nil {
		t.Fatal(err)
	}
	defer recv.Close()
	ln, a := listenAny(t)
	_, other := listenAny(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		got, err := recv.Receive(ctx, []netip.AddrPort{other})
		if got != nil {
			err = errors.New("listeners returned with the error")
		}
		done <- err
	}()
	if err := send(recv.Name(), uid, []Bound{{Addr: a, Listener: ln}}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := <-done; err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Receive error = %v, want a refusal of the unrequested address", err)
	}
}

// TestReceive_MissingAddress shows a sender that hangs up early fails the
// receive instead of leaving a forward without its listener.
func TestReceive_MissingAddress(t *testing.T) {
	uid := os.Getuid()
	recv, err := openReceiver(uid)
	if err != nil {
		t.Fatal(err)
	}
	defer recv.Close()
	ln, a := listenAny(t)
	_, b := listenAny(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		got, err := recv.Receive(ctx, []netip.AddrPort{a, b})
		if got != nil {
			err = errors.New("listeners returned with the error")
		}
		done <- err
	}()
	if err := send(recv.Name(), uid, []Bound{{Addr: a, Listener: ln}}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := <-done; err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Receive error = %v, want a missing-address failure", err)
	}
}

func TestSend_RefusesInvalidName(t *testing.T) {
	ln, a := listenAny(t)
	if err := send("../../etc/passwd", os.Getuid(), []Bound{{Addr: a, Listener: ln}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("send with an invalid name: %v, want ErrInvalid", err)
	}
}

// TestChannel_ReportsFailure shows a helper failure travels to the forward
// process as a HelperError naming the address it could not bind.
func TestChannel_ReportsFailure(t *testing.T) {
	uid := os.Getuid()
	recv, err := openReceiver(uid)
	if err != nil {
		t.Fatal(err)
	}
	defer recv.Close()
	a := netip.MustParseAddrPort("127.77.0.1:80")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		got, err := recv.Receive(ctx, []netip.AddrPort{a})
		if got != nil {
			err = errors.New("listeners returned with the error")
		}
		done <- err
	}()
	failure := &BindError{Addr: a, Err: errors.New("address already in use")}
	if err := report(recv.Name(), uid, failure); err != nil {
		t.Fatalf("report: %v", err)
	}
	err = <-done
	var he *HelperError
	if !errors.As(err, &he) {
		t.Fatalf("Receive error = %v, want a HelperError", err)
	}
	if he.Addr != a || he.Error() != failure.Error() {
		t.Errorf("HelperError = {%v, %q}, want {%v, %q}", he.Addr, he.Error(), a, failure.Error())
	}
}

// TestReceive_TakesReportQueuedBeforeTheEnd shows a report the helper sent
// before it exited is read even when the caller stops waiting before
// Receive first looks: the caller only stops once the helper has exited.
func TestReceive_TakesReportQueuedBeforeTheEnd(t *testing.T) {
	uid := os.Getuid()
	recv, err := openReceiver(uid)
	if err != nil {
		t.Fatal(err)
	}
	defer recv.Close()
	if err := report(recv.Name(), uid, errors.New("writing the hosts file: read-only file system")); err != nil {
		t.Fatalf("report: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = recv.Receive(ctx, []netip.AddrPort{netip.MustParseAddrPort("127.77.0.1:80")})
	var he *HelperError
	if !errors.As(err, &he) || he.Addr.IsValid() || !strings.Contains(he.Error(), "read-only file system") {
		t.Fatalf("Receive error = %v, want the helper's report without an address", err)
	}
	if errors.Is(err, ErrNoHelper) {
		t.Errorf("Receive error %v says the helper never connected", err)
	}
}

// TestReceive_NoHelper shows a channel nobody connected to ends with
// ErrNoHelper, which is how forward tells a refused elevation apart.
func TestReceive_NoHelper(t *testing.T) {
	recv, err := openReceiver(os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	defer recv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = recv.Receive(ctx, []netip.AddrPort{netip.MustParseAddrPort("127.77.0.1:80")})
	if !errors.Is(err, ErrNoHelper) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Receive error = %v, want ErrNoHelper with the context's error", err)
	}
}

// TestReport_RejectsWrongPeer shows the helper reports nothing to a receiver
// that does not run as the user who invoked sudo.
func TestReport_RejectsWrongPeer(t *testing.T) {
	recv, err := openReceiver(os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	defer recv.Close()
	if err := report(recv.Name(), os.Getuid()+1, errors.New("boom")); !errors.Is(err, ErrPeer) {
		t.Fatalf("report to a receiver with the wrong uid: %v, want ErrPeer", err)
	}
}
