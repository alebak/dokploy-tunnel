package registry

import (
	"errors"
	"fmt"
	"maps"
	"slices"
)

const (
	// FirstUnprivilegedPort is the lowest port every OS lets an
	// unprivileged process listen on. Service ports from it up are
	// listened on as they are.
	FirstUnprivilegedPort = 1024
	// LowPortOffset is added to a service port below FirstUnprivilegedPort
	// to get its preferred local port: 80 becomes 8080 and 443 becomes 8443.
	LowPortOffset = 8000
	maxPort       = 65535
)

// ErrNoLocalPort means a service port below FirstUnprivilegedPort found no
// free local port up to 65535.
var ErrNoLocalPort = errors.New("no free local port")

// PreferredLocalPort returns the local port service port p is listened on
// unless the service exposes that number itself: p from
// FirstUnprivilegedPort up, which needs no privileges, and p+LowPortOffset
// below it.
func PreferredLocalPort(p int) int {
	if p >= FirstUnprivilegedPort {
		return p
	}
	return p + LowPortOffset
}

// PortMapping is the local port a service port is listened on.
type PortMapping struct {
	ServicePort int
	LocalPort   int
	// Replaced is the stored local port this mapping replaces because the
	// service now exposes that number itself, or 0.
	Replaced int
}

// PortCollisionError is returned when a fixed local port is already used by
// another port of the same service: natively, or as its stored local port.
type PortCollisionError struct {
	ServicePort int
	LocalPort   int
	// OtherPort is the service port that already uses LocalPort.
	OtherPort int
}

func (e *PortCollisionError) Error() string {
	if e.OtherPort == e.LocalPort {
		return fmt.Sprintf("local port %d for port %d collides with port %d, which the service exposes itself", e.LocalPort, e.ServicePort, e.OtherPort)
	}
	return fmt.Sprintf("local port %d for port %d collides with port %d, which is listened on at %d", e.LocalPort, e.ServicePort, e.OtherPort, e.LocalPort)
}

// MapPorts returns the local port of every service port in ports, in that
// order, for k's lease, and stores what it chose in the lease so later
// calls return the same ports.
//
// A port from FirstUnprivilegedPort up is listened on as it is. A lower
// port keeps the local port stored for it, unless the service now exposes
// that number itself; otherwise it gets PreferredLocalPort, or the next
// number up that the service neither exposes nor uses for another port.
// exposed are other ports the service exposes, such as those the companion
// reports but that are not forwarded. fixed maps service ports to explicit
// local ports, which are used as they are, never stored, and fail with a
// PortCollisionError when another port of the service uses them.
//
// It returns ErrNotLeased when k has no lease.
func (r *Registry) MapPorts(k Key, ports, exposed []int, fixed map[int]int) ([]PortMapping, error) {
	k, err := k.normalize()
	if err != nil {
		return nil, err
	}
	var out []PortMapping
	err = r.update(func(st *state) (bool, error) {
		i := st.find(k)
		if i < 0 {
			return false, fmt.Errorf("%w: %+v", ErrNotLeased, k)
		}
		stored := st.Leases[i].LocalPorts
		var next map[int]int
		out, next, err = assignLocalPorts(ports, exposed, stored, fixed)
		if err != nil {
			return false, err
		}
		if maps.Equal(stored, next) {
			return false, nil
		}
		st.Leases[i].LocalPorts = next
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// assignLocalPorts implements MapPorts on the stored local ports of a
// lease, and returns the local ports to store next.
func assignLocalPorts(ports, exposed []int, stored, fixed map[int]int) ([]PortMapping, map[int]int, error) {
	// owner maps every local port in use to the service port that uses it.
	owner := map[int]int{}
	for _, p := range slices.Concat(ports, exposed) {
		if p >= FirstUnprivilegedPort && p <= maxPort {
			owner[p] = p
		}
	}

	// Stored ports are kept while still valid, lowest service port first
	// so the outcome does not depend on map order.
	next := map[int]int{}
	replaced := map[int]int{}
	for _, p := range slices.Sorted(maps.Keys(stored)) {
		l := stored[p]
		if _, ok := fixed[p]; ok {
			next[p] = l
			continue
		}
		if p < 1 || p >= FirstUnprivilegedPort || l < FirstUnprivilegedPort || l > maxPort {
			continue
		}
		if o, taken := owner[l]; taken {
			if o == l {
				replaced[p] = l
			}
			continue
		}
		owner[l], next[p] = p, l
	}

	for _, p := range slices.Sorted(maps.Keys(fixed)) {
		l := fixed[p]
		if o, taken := owner[l]; taken && o != p {
			return nil, nil, &PortCollisionError{ServicePort: p, LocalPort: l, OtherPort: o}
		}
		owner[l] = p
	}

	local := map[int]int{}
	for _, p := range slices.Sorted(slices.Values(ports)) {
		if _, done := local[p]; done {
			continue
		}
		switch l, ok := fixed[p]; {
		case ok:
			local[p] = l
			continue
		case p >= FirstUnprivilegedPort:
			local[p] = p
			continue
		}
		if l, ok := next[p]; ok {
			local[p] = l
			continue
		}
		l := PreferredLocalPort(p)
		for ; l <= maxPort; l++ {
			if _, taken := owner[l]; !taken {
				break
			}
		}
		if l > maxPort {
			return nil, nil, fmt.Errorf("%w for port %d between %d and %d", ErrNoLocalPort, p, PreferredLocalPort(p), maxPort)
		}
		owner[l], next[p], local[p] = p, l, l
	}

	out := make([]PortMapping, len(ports))
	for i, p := range ports {
		out[i] = PortMapping{ServicePort: p, LocalPort: local[p]}
		if _, ok := fixed[p]; !ok {
			out[i].Replaced = replaced[p]
		}
	}
	if len(next) == 0 {
		next = nil
	}
	return out, next, nil
}
