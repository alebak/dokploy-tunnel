//go:build !linux

package lowport

import "net/netip"

// Privileged returns whether binding a port needs administrator privileges
// for this process. Outside Linux it always reports false: Windows needs
// none, and on macOS the helper cannot open them (see the package comment).
func Privileged() func(port int) bool {
	return func(int) bool { return false }
}

// System returns the Broker for this operating system; outside Linux, it
// fails every call with ErrUnsupported.
func System() Broker { return unsupported{} }

type unsupported struct{}

func (unsupported) Open() (Receiver, error) { return nil, ErrUnsupported }

func (unsupported) Bind([]netip.AddrPort) ([]Bound, error) { return nil, ErrUnsupported }

func (unsupported) Send(string, int, []Bound) error { return ErrUnsupported }

func (unsupported) Report(string, int, error) error { return ErrUnsupported }
