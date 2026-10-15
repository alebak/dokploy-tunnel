//go:build !linux

package lowport

import "net/netip"

// Privileged returns whether binding a port needs administrator privileges
// for this process. Outside Linux, loopback ports never do.
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
