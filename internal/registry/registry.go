// Package registry assigns every forwarded Dokploy service a stable loopback
// IP address on the client machine.
//
// A lease binds a Key (Dokploy instance, organization and service IDs) to one
// address from a dedicated loopback range. Each service then listens on its
// real port on its own address, so two services that both use port 5432 never
// collide. Leases persist in a JSON file shared by every doktunnel process and
// are never handed to a different key, not even after Forget.
package registry

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/hostname"
)

// DefaultRange is the loopback range leases are allocated from.
//
// 127.77.0.0/16 sits inside 127.0.0.0/8 (loopback on every supported OS) and
// stays clear of the addresses other software already claims:
// 127.0.0.0/24 (127.0.0.1 and systemd-resolved's 127.0.0.53),
// 127.0.1.0/24 (the hostname entry Debian writes to /etc/hosts) and
// 127.24.0.0/16 (Northflank's forwarding range). Host addresses whose last
// octet is 0 or 255 are never leased, leaving 65,024 usable addresses.
var DefaultRange = netip.MustParsePrefix("127.77.0.0/16")

var loopback = netip.MustParsePrefix("127.0.0.0/8")

// Lease is a persisted binding of a Key to an address.
type Lease struct {
	Key       Key
	IP        netip.Addr
	CreatedAt time.Time
	// Names are what the lease's hostname is built from, last recorded
	// with SetNames; they are zero until the lease is named.
	Names hostname.Names
}

// ExhaustedError is returned when every address in the range is leased or
// retired.
type ExhaustedError struct {
	Range netip.Prefix
}

func (e *ExhaustedError) Error() string {
	return fmt.Sprintf("no free loopback address left in %s", e.Range)
}

// Registry reads and writes the lease file at a fixed path. A Registry holds
// no cached state: every call locks the file, so separate Registry values and
// separate processes sharing one file stay consistent.
type Registry struct {
	path string
	rng  netip.Prefix
}

// Option configures a Registry.
type Option func(*Registry)

// WithRange allocates new leases from p instead of DefaultRange. It exists
// for tests that need a tiny range.
func WithRange(p netip.Prefix) Option {
	return func(r *Registry) { r.rng = p }
}

// Open returns a Registry backed by the file at path. The file is created on
// the first lease; a missing file is an empty registry.
func Open(path string, opts ...Option) (*Registry, error) {
	r := &Registry{path: path, rng: DefaultRange}
	for _, opt := range opts {
		opt(r)
	}
	// /30 is the smallest range with a usable address: a /31 or /32 inside
	// 127.0.0.0/8 holds only a .0 octet, or one address at the range edge.
	if !r.rng.IsValid() || !r.rng.Addr().Is4() || r.rng.Bits() < loopback.Bits() ||
		r.rng.Bits() > 30 || !loopback.Contains(r.rng.Addr()) {
		return nil, fmt.Errorf("invalid lease range %s: must be an IPv4 prefix between /8 and /30 inside %s", r.rng, loopback)
	}
	r.rng = r.rng.Masked()
	return r, nil
}

// Lease returns the address bound to k, allocating and persisting the next
// free address when k has none yet.
func (r *Registry) Lease(k Key) (netip.Addr, error) {
	k, err := k.normalize()
	if err != nil {
		return netip.Addr{}, err
	}
	var ip netip.Addr
	err = r.update(func(st *state) (bool, error) {
		if i := st.find(k); i >= 0 {
			ip = st.Leases[i].IP
			return false, nil
		}
		next, ok := st.nextFree(r.rng)
		if !ok {
			return false, &ExhaustedError{Range: r.rng}
		}
		ip = next
		st.Leases = append(st.Leases, fileLease{
			Instance:       k.Instance,
			OrganizationID: k.OrganizationID,
			ServiceID:      k.ServiceID,
			IP:             next,
			CreatedAt:      time.Now().UTC(),
		})
		return true, nil
	})
	if err != nil {
		return netip.Addr{}, err
	}
	return ip, nil
}

// Lookup returns the address bound to k without allocating one.
func (r *Registry) Lookup(k Key) (netip.Addr, bool, error) {
	k, err := k.normalize()
	if err != nil {
		return netip.Addr{}, false, err
	}
	var (
		ip    netip.Addr
		found bool
	)
	err = r.update(func(st *state) (bool, error) {
		if i := st.find(k); i >= 0 {
			ip, found = st.Leases[i].IP, true
		}
		return false, nil
	})
	return ip, found, err
}

// List returns every active lease ordered by address.
func (r *Registry) List() ([]Lease, error) {
	var leases []Lease
	err := r.update(func(st *state) (bool, error) {
		for _, l := range st.Leases {
			leases = append(leases, l.lease())
		}
		return false, nil
	})
	slices.SortFunc(leases, func(a, b Lease) int { return a.IP.Compare(b.IP) })
	return leases, err
}

// Forget removes the lease of k and reports whether one existed.
//
// The address is retired, never returned to the pool: hosts file entries,
// client configs and connection strings written while the lease was active
// may still point at it, and they must fail to connect rather than silently
// reach a different service. Leasing k again yields a new address.
func (r *Registry) Forget(k Key) (bool, error) {
	k, err := k.normalize()
	if err != nil {
		return false, err
	}
	var removed bool
	err = r.update(func(st *state) (bool, error) {
		i := st.find(k)
		if i < 0 {
			return false, nil
		}
		st.Retired = append(st.Retired, st.Leases[i].IP)
		st.Leases = slices.Delete(st.Leases, i, i+1)
		removed = true
		return true, nil
	})
	return removed, err
}

// SetNames records what the hostname of k's lease is built from and
// reports whether it changed. Names follow renames in Dokploy; the address
// never does. It returns ErrNotLeased when k has no lease.
func (r *Registry) SetNames(k Key, n hostname.Names) (bool, error) {
	k, err := k.normalize()
	if err != nil {
		return false, err
	}
	var changed bool
	err = r.update(func(st *state) (bool, error) {
		i := st.find(k)
		if i < 0 {
			return false, fmt.Errorf("%w: %+v", ErrNotLeased, k)
		}
		changed = st.Leases[i].Names != n
		st.Leases[i].Names = n
		return changed, nil
	})
	return changed, err
}

// update runs fn on the current state while holding the file lock and saves
// the state when fn reports a change.
func (r *Registry) update(fn func(*state) (bool, error)) (err error) {
	unlock, err := lockFile(r.path + ".lock")
	if err != nil {
		return fmt.Errorf("locking address registry: %w", err)
	}
	defer func() {
		if uerr := unlock(); uerr != nil && err == nil {
			err = fmt.Errorf("unlocking address registry: %w", uerr)
		}
	}()

	st, err := load(r.path)
	if err != nil {
		return err
	}
	changed, err := fn(st)
	if err != nil || !changed {
		return err
	}
	return save(r.path, st)
}

var (
	// ErrInvalidKey is wrapped by every Key validation error.
	ErrInvalidKey = errors.New("invalid service key")
	// ErrNotLeased means the key has no lease.
	ErrNotLeased = errors.New("service has no address lease")
)
