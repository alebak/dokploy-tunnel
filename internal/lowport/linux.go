//go:build linux

package lowport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"syscall"
	"time"
)

// maxMessage bounds one channel message: an address with its port.
const maxMessage = 64

// Privileged returns whether binding a port needs administrator privileges
// for this process: on Linux, every port below Limit unless the process
// already runs as root. The limit is read once, when Privileged is called.
func Privileged() func(port int) bool {
	if os.Geteuid() == 0 {
		return func(int) bool { return false }
	}
	limit := Limit()
	return func(port int) bool { return port < limit }
}

// System returns the Broker for Linux: the forward process takes listeners
// only from root, and the helper hands them only to the user who ran sudo.
func System() Broker { return system{} }

type system struct{}

func (system) Open() (Receiver, error) { return openReceiver(0) }

func (system) Bind(addrs []netip.AddrPort) ([]Bound, error) { return bind(addrs) }

func (system) Send(name string, wantUID int, bound []Bound) error {
	return send(name, wantUID, bound)
}

// receiver listens on an abstract SOCK_SEQPACKET socket, which keeps every
// address together with its descriptor in one message.
type receiver struct {
	ln      *net.UnixListener
	name    string
	wantUID int
}

// openReceiver creates a channel whose only accepted peer runs as wantUID.
func openReceiver(wantUID int) (*receiver, error) {
	name, err := NewName()
	if err != nil {
		return nil, err
	}
	ln, err := net.ListenUnix("unixpacket", abstractAddr(name))
	if err != nil {
		return nil, fmt.Errorf("opening the low-port channel: %w", err)
	}
	return &receiver{ln: ln, name: name, wantUID: wantUID}, nil
}

// abstractAddr is the abstract socket called name; Go maps the leading "@"
// to the abstract namespace, so no file is created.
func abstractAddr(name string) *net.UnixAddr {
	return &net.UnixAddr{Name: "@" + name, Net: "unixpacket"}
}

func (r *receiver) Name() string { return r.name }

func (r *receiver) Close() error { return r.ln.Close() }

// Receive implements Receiver. A peer with another uid is dropped and the
// receiver keeps waiting, so a local user who finds the name cannot keep
// the helper's connection out.
func (r *receiver) Receive(ctx context.Context, want []netip.AddrPort) (map[netip.AddrPort]net.Listener, error) {
	var mu sync.Mutex
	var current *net.UnixConn
	stop := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		past := time.Unix(1, 0)
		_ = r.ln.SetDeadline(past)
		if current != nil {
			_ = current.SetDeadline(past)
		}
	})
	defer stop()

	for {
		c, err := r.ln.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("waiting for the privileged helper: %w", ctx.Err())
			}
			return nil, fmt.Errorf("waiting for the privileged helper: %w", err)
		}
		if uid, err := peerUID(c); err != nil || uid != r.wantUID {
			_ = c.Close()
			continue
		}
		mu.Lock()
		current = c
		if ctx.Err() != nil {
			_ = c.SetDeadline(time.Unix(1, 0))
		}
		mu.Unlock()
		got, err := readListeners(c, want)
		_ = c.Close()
		if err != nil && ctx.Err() != nil {
			err = fmt.Errorf("%w: %w", err, ctx.Err())
		}
		return got, err
	}
}

// readListeners reads one message per address in want, each holding the
// address and exactly one descriptor listening on it. On any error it
// closes every descriptor it received.
func readListeners(c *net.UnixConn, want []netip.AddrPort) (map[netip.AddrPort]net.Listener, error) {
	wanted := make(map[netip.AddrPort]bool, len(want))
	for _, a := range want {
		wanted[a] = true
	}
	got := make(map[netip.AddrPort]net.Listener, len(want))
	fail := func(err error) (map[netip.AddrPort]net.Listener, error) {
		for _, ln := range got {
			_ = ln.Close()
		}
		return nil, err
	}
	buf := make([]byte, maxMessage)
	// Room for more than one descriptor, so extra ones are seen and closed
	// rather than silently truncated.
	oob := make([]byte, syscall.CmsgSpace(4*4))
	for len(got) < len(want) {
		n, oobn, flags, _, err := c.ReadMsgUnix(buf, oob)
		if err != nil && !errors.Is(err, io.EOF) {
			return fail(fmt.Errorf("reading from the privileged helper: %w", err))
		}
		fds, rightsErr := parseRights(oob[:oobn])
		if n == 0 && oobn == 0 {
			return fail(fmt.Errorf("the privileged helper sent %d of %d listeners", len(got), len(want)))
		}
		ap, addrErr := netip.ParseAddrPort(string(buf[:n]))
		switch {
		case rightsErr != nil || flags&(syscall.MSG_TRUNC|syscall.MSG_CTRUNC) != 0 || len(fds) != 1:
			closeFDs(fds)
			return fail(errors.New("the privileged helper sent a malformed message"))
		case addrErr != nil || !wanted[ap] || got[ap] != nil:
			closeFDs(fds)
			return fail(errors.New("the privileged helper sent a listener that was not requested"))
		}
		ln, err := fileListener(fds[0], ap)
		if err != nil {
			return fail(err)
		}
		got[ap] = ln
	}
	return got, nil
}

// fileListener turns fd into a listener, checking that it listens on ap.
// It always takes ownership of fd.
func fileListener(fd int, ap netip.AddrPort) (net.Listener, error) {
	f := os.NewFile(uintptr(fd), "low-port listener")
	ln, err := net.FileListener(f)
	_ = f.Close()
	if err != nil {
		return nil, fmt.Errorf("using the listener for %s: %w", ap, err)
	}
	if got, ok := ln.Addr().(*net.TCPAddr); !ok || got.AddrPort() != ap {
		_ = ln.Close()
		return nil, fmt.Errorf("the listener for %s listens on %s", ap, ln.Addr())
	}
	return ln, nil
}

func parseRights(oob []byte) ([]int, error) {
	if len(oob) == 0 {
		return nil, nil
	}
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	var fds []int
	var errs []error
	for i := range msgs {
		got, err := syscall.ParseUnixRights(&msgs[i])
		fds = append(fds, got...)
		errs = append(errs, err)
	}
	return fds, errors.Join(errs...)
}

func closeFDs(fds []int) {
	for _, fd := range fds {
		_ = syscall.Close(fd)
	}
}

// peerUID returns the uid of the process at the other end of c, as the
// kernel recorded it when the connection was made.
func peerUID(c *net.UnixConn) (int, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *syscall.Ucred
	var credErr error
	if err := rc.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, fmt.Errorf("reading the peer's credentials: %w", credErr)
	}
	return int(cred.Uid), nil
}

// bind listens on every address, closing them all if one fails.
func bind(addrs []netip.AddrPort) ([]Bound, error) {
	out := make([]Bound, 0, len(addrs))
	for _, a := range addrs {
		ln, err := net.Listen("tcp", a.String())
		if err != nil {
			for _, b := range out {
				_ = b.Listener.Close()
			}
			return nil, fmt.Errorf("listening on %s: %w", a, err)
		}
		out = append(out, Bound{Addr: a, Listener: ln})
	}
	return out, nil
}

// send connects to the channel called name and, once its owner proves to
// run as wantUID, sends one message per listener: the address, with the
// descriptor attached.
func send(name string, wantUID int, bound []Bound) error {
	if !ValidName(name) {
		return fmt.Errorf("%w: not a doktunnel channel name", ErrInvalid)
	}
	c, err := net.DialUnix("unixpacket", nil, abstractAddr(name))
	if err != nil {
		return fmt.Errorf("connecting to the forward process: %w", err)
	}
	defer c.Close()
	uid, err := peerUID(c)
	if err != nil {
		return fmt.Errorf("checking the forward process: %w", err)
	}
	if uid != wantUID {
		return fmt.Errorf("%w: the channel belongs to uid %d, not %d", ErrPeer, uid, wantUID)
	}
	for _, b := range bound {
		sc, ok := b.Listener.(syscall.Conn)
		if !ok {
			return fmt.Errorf("the listener for %s has no descriptor", b.Addr)
		}
		rc, err := sc.SyscallConn()
		if err != nil {
			return fmt.Errorf("sending the listener for %s: %w", b.Addr, err)
		}
		var writeErr error
		if err := rc.Control(func(fd uintptr) {
			_, _, writeErr = c.WriteMsgUnix([]byte(b.Addr.String()), syscall.UnixRights(int(fd)), nil)
		}); err != nil {
			return fmt.Errorf("sending the listener for %s: %w", b.Addr, err)
		}
		if writeErr != nil {
			return fmt.Errorf("sending the listener for %s: %w", b.Addr, writeErr)
		}
	}
	return nil
}
