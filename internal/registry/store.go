package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"time"
)

// schemaVersion is the lease file format version. Bump it on incompatible
// changes and teach load to migrate older versions.
const schemaVersion = 1

var (
	// ErrCorrupt means the lease file exists but cannot be trusted.
	ErrCorrupt = errors.New("address registry file is corrupt")
	// ErrUnsupportedVersion means the lease file was written by an
	// incompatible doktunnel version.
	ErrUnsupportedVersion = errors.New("address registry file has an unsupported version")
)

// state is the on-disk lease file.
type state struct {
	Version int         `json:"version"`
	Leases  []fileLease `json:"leases"`
	// Retired holds forgotten addresses; they are never leased again.
	Retired []netip.Addr `json:"retired,omitempty"`
}

type fileLease struct {
	Instance       string     `json:"instance"`
	OrganizationID string     `json:"organization_id"`
	ServiceID      string     `json:"service_id"`
	IP             netip.Addr `json:"ip"`
	CreatedAt      time.Time  `json:"created_at"`
}

func (l fileLease) key() Key {
	return Key{Instance: l.Instance, OrganizationID: l.OrganizationID, ServiceID: l.ServiceID}
}

func (l fileLease) lease() Lease {
	return Lease{Key: l.key(), IP: l.IP, CreatedAt: l.CreatedAt}
}

// find returns the index of the lease for the normalized key k, or -1.
func (s *state) find(k Key) int {
	for i, l := range s.Leases {
		if l.key() == k {
			return i
		}
	}
	return -1
}

// nextFree returns the lowest address in rng that is neither leased nor
// retired, skipping host octets 0 and 255.
func (s *state) nextFree(rng netip.Prefix) (netip.Addr, bool) {
	used := make(map[netip.Addr]bool, len(s.Leases)+len(s.Retired))
	for _, l := range s.Leases {
		used[l.IP] = true
	}
	for _, ip := range s.Retired {
		used[ip] = true
	}
	for ip := rng.Addr(); ip.IsValid() && rng.Contains(ip); ip = ip.Next() {
		if last := ip.As4()[3]; last == 0 || last == 255 || used[ip] {
			continue
		}
		return ip, true
	}
	return netip.Addr{}, false
}

// validate rejects files that would break the never-reassign guarantee.
func (s *state) validate() error {
	if s.Version != schemaVersion {
		return fmt.Errorf("%w: got %d, want %d", ErrUnsupportedVersion, s.Version, schemaVersion)
	}
	addrs := make(map[netip.Addr]bool)
	for _, ip := range s.Retired {
		if !ip.IsValid() {
			return fmt.Errorf("%w: invalid retired address", ErrCorrupt)
		}
		addrs[ip] = true
	}
	keys := make(map[Key]bool, len(s.Leases))
	for _, l := range s.Leases {
		k, err := l.key().normalize()
		if err != nil || k != l.key() {
			return fmt.Errorf("%w: invalid key %+v", ErrCorrupt, l.key())
		}
		if !l.IP.IsValid() {
			return fmt.Errorf("%w: lease %+v has no address", ErrCorrupt, k)
		}
		if addrs[l.IP] {
			return fmt.Errorf("%w: address %s is used twice", ErrCorrupt, l.IP)
		}
		if keys[k] {
			return fmt.Errorf("%w: key %+v is leased twice", ErrCorrupt, k)
		}
		addrs[l.IP], keys[k] = true, true
	}
	return nil
}

// load reads the lease file at path; a missing file is an empty state.
func load(path string) (*state, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &state{Version: schemaVersion}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading address registry: %w", err)
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, path, err)
	}
	if err := st.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &st, nil
}

// save writes st atomically: a crash leaves either the old or the new file,
// never a truncated one.
func save(path string, st *state) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding address registry: %w", err)
	}
	b = append(b, '\n')

	dir, base := filepath.Split(path)
	tmp, err := os.CreateTemp(dir, "."+base+".tmp*")
	if err != nil {
		return fmt.Errorf("writing address registry: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("writing address registry: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("writing address registry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing address registry: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replacing address registry: %w", err)
	}
	return nil
}
