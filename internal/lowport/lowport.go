// Package lowport lets an unprivileged forward listen on ports below the
// unprivileged port limit, such as 80 and 443, on Linux.
//
// Linux requires CAP_NET_BIND_SERVICE to bind any port below
// net.ipv4.ip_unprivileged_port_start (1024 by default), loopback addresses
// included. Instead of running doktunnel as root or changing a system
// setting, the privileged helper, which forward already runs through sudo
// for the hosts file, binds the requested addresses and hands the listening
// sockets back over a Unix socket with SCM_RIGHTS, then exits. The forward
// process keeps running as the user, and the sockets close with it, so there
// is nothing to restore on exit.
//
// The channel is a Linux abstract Unix socket with a random name, created by
// the forward process: no file is created, and root never opens a path its
// caller chose. Each side checks the other's credentials with SO_PEERCRED:
// the forward process only takes sockets from root, and the helper only
// hands them to the user who ran sudo.
//
// sudo only returns the helper's exit status, which cannot tell a refused
// elevation (wrong password, no terminal) from a helper that ran and
// failed. The channel can: a helper that fails once it knows the channel
// connects and reports the failure (see Broker.Report) before it exits, and
// Receive takes anything the helper queued even after the caller stopped
// waiting. So a connection from root proves the elevation worked, and
// Receive returns ErrNoHelper only when nothing connected.
//
// Outside Linux, Privileged always reports false and System's methods fail
// with ErrUnsupported. Windows lets users bind low ports. macOS lets a user
// other than root bind them only on the wildcard address, which forward
// never uses, so low ports on macOS still fail; the platform tests record
// it.
package lowport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/alebak/dokploy-tunnel/internal/registry"
)

// MaxAddrs bounds how many addresses one request may name.
const MaxAddrs = 64

// DefaultLimit is the first unprivileged port when the system's limit
// cannot be read: Linux's default.
const DefaultLimit = 1024

// limitPath holds Linux's first unprivileged port.
const limitPath = "/proc/sys/net/ipv4/ip_unprivileged_port_start"

// namePrefix starts every channel name; a random hex suffix follows.
const namePrefix = "doktunnel-"

// nameRandomBytes is the length of a channel name's random suffix.
const nameRandomBytes = 16

var (
	// ErrInvalid means a request names something the helper refuses.
	ErrInvalid = errors.New("invalid low-port request")
	// ErrPeer means the other end of the channel runs as the wrong user.
	ErrPeer = errors.New("unexpected peer on the low-port channel")
	// ErrUnsupported means this system has no low-port channel; it never
	// needs one.
	ErrUnsupported = errors.New("listening through the privileged helper is only supported on Linux")
	// ErrNoHelper means the helper never connected to the channel: the
	// elevation did not run it.
	ErrNoHelper = errors.New("the privileged helper never connected")
)

// maxFailure bounds the text of a reported failure, in bytes.
const maxFailure = 400

// BindError is a failure to listen on one requested address.
type BindError struct {
	Addr netip.AddrPort
	Err  error
}

func (e *BindError) Error() string { return fmt.Sprintf("listening on %s: %v", e.Addr, e.Err) }

func (e *BindError) Unwrap() error { return e.Err }

// HelperError is a failure the privileged helper reported over the
// channel: it ran, so the elevation worked, but it could not finish.
type HelperError struct {
	// Addr is the address the helper could not listen on, when that was
	// the failure; otherwise it is the zero value.
	Addr netip.AddrPort
	// Msg is the helper's error message, at most maxFailure bytes.
	Msg string
}

func (e *HelperError) Error() string { return e.Msg }

// Failure returns the HelperError the forward process receives when the
// helper reports err.
func Failure(err error) *HelperError {
	he := &HelperError{Msg: strings.ToValidUTF8(err.Error(), "?")}
	if len(he.Msg) > maxFailure {
		he.Msg = strings.ToValidUTF8(he.Msg[:maxFailure], "")
	}
	var be *BindError
	if errors.As(err, &be) {
		he.Addr = be.Addr
	}
	return he
}

// Bound is a listener the helper opened for a requested address.
type Bound struct {
	Addr     netip.AddrPort
	Listener net.Listener
}

// Receiver is the forward process's end of the channel.
type Receiver interface {
	// Name is the channel name to pass to the helper.
	Name() string
	// Receive waits for the helper to connect and send a listener for
	// every address in want, and returns them keyed by address. It fails,
	// returning no listener, if anything else arrives, if the helper hangs
	// up before sending them all, with a *HelperError if the helper
	// reported a failure, or when ctx ends. When ctx ends it still takes a
	// connection the helper already made; it wraps ErrNoHelper only when
	// there was none.
	Receive(ctx context.Context, want []netip.AddrPort) (map[netip.AddrPort]net.Listener, error)
	// Close stops receiving and removes the channel.
	Close() error
}

// Broker opens listeners below the unprivileged port limit in the
// privileged helper and passes them to the forward process.
type Broker interface {
	// Open creates the forward process's end of a new channel.
	Open() (Receiver, error)
	// Bind listens on every address. The privileged helper calls it before
	// changing anything else, so a port already in use changes nothing.
	Bind(addrs []netip.AddrPort) ([]Bound, error)
	// Send hands bound to the receiver called name, which must run as
	// peerUID, the user who invoked the helper.
	Send(name string, peerUID int, bound []Bound) error
	// Report tells the receiver called name, which must run as peerUID,
	// that the helper failed with failure; Receive returns it as a
	// *HelperError.
	Report(name string, peerUID int, failure error) error
}

// ParseAddrs parses a comma-separated "<ip>:<port>" list, as FormatAddrs
// writes it, and validates it strictly: one to MaxAddrs addresses, each in
// registry.DefaultRange with a port from 1 to limit-1, none repeated; with a
// limit of 1 or less no port is valid. The
// privileged helper uses it, so errors name the position of a bad element,
// never its contents.
func ParseAddrs(list string, limit int) ([]netip.AddrPort, error) {
	if list == "" {
		return nil, fmt.Errorf("%w: no address", ErrInvalid)
	}
	parts := strings.Split(list, ",")
	if limit <= 1 {
		return nil, fmt.Errorf("%w: no port needs privileges on this system", ErrInvalid)
	}
	if len(parts) > MaxAddrs {
		return nil, fmt.Errorf("%w: more than %d addresses", ErrInvalid, MaxAddrs)
	}
	out := make([]netip.AddrPort, 0, len(parts))
	seen := make(map[netip.AddrPort]bool, len(parts))
	for i, p := range parts {
		bad := func(reason string) error {
			return fmt.Errorf("%w: address %d: %s", ErrInvalid, i+1, reason)
		}
		ap, err := netip.ParseAddrPort(p)
		if err != nil || ap.Addr().Zone() != "" || !registry.DefaultRange.Contains(ap.Addr()) {
			return nil, bad(fmt.Sprintf("not an address in %v with a port", registry.DefaultRange))
		}
		if ap.Port() < 1 || int(ap.Port()) >= limit {
			return nil, bad(fmt.Sprintf("the port is not from 1 to %d", limit-1))
		}
		if seen[ap] {
			return nil, bad("repeats an earlier address")
		}
		seen[ap] = true
		out = append(out, ap)
	}
	return out, nil
}

// FormatAddrs renders addrs as the comma-separated list ParseAddrs reads.
func FormatAddrs(addrs []netip.AddrPort) string {
	s := make([]string, len(addrs))
	for i, a := range addrs {
		s[i] = a.String()
	}
	return strings.Join(s, ",")
}

// NewName returns a random channel name.
func NewName() (string, error) {
	b := make([]byte, nameRandomBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating a channel name: %w", err)
	}
	return namePrefix + hex.EncodeToString(b), nil
}

// ValidName reports whether name has the form NewName returns, so the
// helper never connects anywhere else.
func ValidName(name string) bool {
	suffix, ok := strings.CutPrefix(name, namePrefix)
	if !ok || len(suffix) != 2*nameRandomBytes {
		return false
	}
	return strings.Trim(suffix, "0123456789abcdef") == ""
}

// Limit returns the first port an unprivileged process may bind: Linux's
// net.ipv4.ip_unprivileged_port_start, or DefaultLimit when it cannot be
// read.
func Limit() int {
	return readLimit(limitPath)
}

func readLimit(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return DefaultLimit
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n < 0 || n > 65535 {
		return DefaultLimit
	}
	return n
}
