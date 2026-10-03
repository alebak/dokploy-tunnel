package registry

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/alebak/dokploy-tunnel/internal/hostname"
)

func newTestRegistry(t *testing.T, opts ...Option) (*Registry, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "addresses.json")
	r, err := Open(path, opts...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return r, path
}

func testKey(service string) Key {
	return Key{Instance: "https://dokploy.example.com", OrganizationID: "org-1", ServiceID: service}
}

func mustLease(t *testing.T, r *Registry, k Key) netip.Addr {
	t.Helper()
	ip, err := r.Lease(k)
	if err != nil {
		t.Fatalf("Lease(%v): %v", k, err)
	}
	return ip
}

func TestDefaultRange_AvoidsReservedLoopbackRanges(t *testing.T) {
	reserved := []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/24"),  // 127.0.0.1, systemd-resolved 127.0.0.53
		netip.MustParsePrefix("127.0.1.0/24"),  // Debian hostname entry
		netip.MustParsePrefix("127.24.0.0/16"), // Northflank
	}
	loopback := netip.MustParsePrefix("127.0.0.0/8")
	if !loopback.Contains(DefaultRange.Addr()) || DefaultRange.Bits() != 16 {
		t.Fatalf("DefaultRange %v is not a /16 inside %v", DefaultRange, loopback)
	}
	for _, p := range reserved {
		if p.Overlaps(DefaultRange) {
			t.Errorf("DefaultRange %v overlaps reserved %v", DefaultRange, p)
		}
	}
}

func TestLease_FirstAddressSkipsNetworkOctet(t *testing.T) {
	r, _ := newTestRegistry(t)
	got := mustLease(t, r, testKey("svc-a"))
	want := netip.MustParseAddr("127.77.0.1")
	if got != want {
		t.Fatalf("first lease = %v, want %v", got, want)
	}
}

func TestLease_SameKeySameIPAcrossReopen(t *testing.T) {
	r, path := newTestRegistry(t)
	first := mustLease(t, r, testKey("svc-a"))
	mustLease(t, r, testKey("svc-b"))

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if again := mustLease(t, reopened, testKey("svc-a")); again != first {
		t.Fatalf("lease after reopen = %v, want %v", again, first)
	}
	ip, ok, err := reopened.Lookup(testKey("svc-a"))
	if err != nil || !ok || ip != first {
		t.Fatalf("Lookup = %v, %v, %v; want %v, true, nil", ip, ok, err, first)
	}
}

func TestLease_DifferentKeysNeverCollide(t *testing.T) {
	r, _ := newTestRegistry(t)
	keys := []Key{
		{Instance: "https://a.example.com", OrganizationID: "org-1", ServiceID: "postgres"},
		{Instance: "https://b.example.com", OrganizationID: "org-1", ServiceID: "postgres"},
		{Instance: "https://a.example.com", OrganizationID: "org-2", ServiceID: "postgres"},
		{Instance: "https://a.example.com", OrganizationID: "org-1", ServiceID: "redis"},
		{Instance: "https://a.example.com:8443", OrganizationID: "org-1", ServiceID: "postgres"},
		{Instance: "http://a.example.com", OrganizationID: "org-1", ServiceID: "postgres"},
	}
	seen := map[netip.Addr]Key{}
	for _, k := range keys {
		ip := mustLease(t, r, k)
		if prev, dup := seen[ip]; dup {
			t.Fatalf("%v and %v share %v", prev, k, ip)
		}
		seen[ip] = k
	}
}

func TestLease_InstanceURLNormalizationKeepsIP(t *testing.T) {
	r, _ := newTestRegistry(t)
	base := mustLease(t, r, Key{Instance: "https://dokploy.example.com", OrganizationID: "o", ServiceID: "s"})
	variants := []string{
		"HTTPS://Dokploy.Example.COM/",
		"https://dokploy.example.com:443",
		"https://dokploy.example.com/dashboard?x=1#frag",
	}
	for _, v := range variants {
		t.Run(v, func(t *testing.T) {
			if got := mustLease(t, r, Key{Instance: v, OrganizationID: "o", ServiceID: "s"}); got != base {
				t.Fatalf("lease = %v, want %v", got, base)
			}
		})
	}
}

func TestNormalizeInstanceURL(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "plain https", in: "https://dokploy.example.com", want: "https://dokploy.example.com"},
		{name: "uppercase and trailing slash", in: "HTTPS://Dokploy.Example.com/", want: "https://dokploy.example.com"},
		{name: "default https port dropped", in: "https://x.io:443", want: "https://x.io"},
		{name: "default http port dropped", in: "http://x.io:80/", want: "http://x.io"},
		{name: "custom port kept", in: "http://x.io:3000", want: "http://x.io:3000"},
		{name: "ipv6 host", in: "http://[::1]:3000/", want: "http://[::1]:3000"},
		{name: "ipv6 host default port", in: "https://[FE80::1]", want: "https://[fe80::1]"},
		{name: "missing scheme", in: "dokploy.example.com", wantErr: true},
		{name: "unsupported scheme", in: "ftp://x.io", wantErr: true},
		{name: "empty", in: "", wantErr: true},
		{name: "no host", in: "https://", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeInstanceURL(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeInstanceURL(%q) = %q, want error", tt.in, got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("NormalizeInstanceURL(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
			}
		})
	}
}

func TestLease_InvalidKey(t *testing.T) {
	r, _ := newTestRegistry(t)
	tests := []struct {
		name string
		key  Key
	}{
		{name: "missing organization", key: Key{Instance: "https://x.io", ServiceID: "s"}},
		{name: "missing service", key: Key{Instance: "https://x.io", OrganizationID: "o"}},
		{name: "bad instance", key: Key{Instance: "x.io", OrganizationID: "o", ServiceID: "s"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := r.Lease(tt.key); err == nil {
				t.Fatal("Lease succeeded, want error")
			}
		})
	}
}

// Display names are deliberately not part of Key: a renamed service is the
// same Dokploy service ID and must keep its address.
func TestLease_RenameIrrelevant(t *testing.T) {
	r, _ := newTestRegistry(t)
	k := Key{Instance: "https://x.io", OrganizationID: "o", ServiceID: "svc-123"}
	before := mustLease(t, r, k)
	// The caller renames "db" to "primary-db"; the key it builds is unchanged.
	renamed := Key{Instance: "https://x.io", OrganizationID: "o", ServiceID: "svc-123"}
	if after := mustLease(t, r, renamed); after != before {
		t.Fatalf("lease after rename = %v, want %v", after, before)
	}
}

func TestForget_NeverFreesIP(t *testing.T) {
	r, path := newTestRegistry(t)
	gone := mustLease(t, r, testKey("old"))

	removed, err := r.Forget(testKey("old"))
	if err != nil || !removed {
		t.Fatalf("Forget = %v, %v; want true, nil", removed, err)
	}
	if _, ok, _ := r.Lookup(testKey("old")); ok {
		t.Fatal("Lookup found a forgotten key")
	}
	if removed, err := r.Forget(testKey("old")); err != nil || removed {
		t.Fatalf("second Forget = %v, %v; want false, nil", removed, err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	for _, k := range []Key{testKey("new"), testKey("old")} {
		if ip := mustLease(t, reopened, k); ip == gone {
			t.Fatalf("Lease(%v) reused forgotten address %v", k, gone)
		}
	}
}

func TestList_ReturnsLeasesInAddressOrder(t *testing.T) {
	r, _ := newTestRegistry(t)
	if leases, err := r.List(); err != nil || len(leases) != 0 {
		t.Fatalf("List on missing file = %v, %v; want empty, nil", leases, err)
	}
	for _, s := range []string{"a", "b", "c"} {
		mustLease(t, r, testKey(s))
	}
	if _, err := r.Forget(testKey("b")); err != nil {
		t.Fatal(err)
	}
	leases, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range leases {
		got = append(got, l.Key.ServiceID+"="+l.IP.String())
	}
	want := "a=127.77.0.1,c=127.77.0.3"
	if strings.Join(got, ",") != want {
		t.Fatalf("List = %v, want %v", got, want)
	}
	if leases[0].Key.Instance != "https://dokploy.example.com" || leases[0].CreatedAt.IsZero() {
		t.Fatalf("lease metadata not persisted: %+v", leases[0])
	}
}

func TestSetNames_PersistsAndReportsChanges(t *testing.T) {
	r, path := newTestRegistry(t)
	k := testKey("pg")
	ip := mustLease(t, r, k)
	names := hostname.Names{Context: "prod", Organization: "Acme", Project: "shop", Compose: "myapp", Service: "postgres"}

	changed, err := r.SetNames(k, names)
	if err != nil || !changed {
		t.Fatalf("SetNames = %v, %v; want true, nil", changed, err)
	}
	if changed, err := r.SetNames(k, names); err != nil || changed {
		t.Fatalf("SetNames with the same names = %v, %v; want false, nil", changed, err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	leases, err := reopened.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Names != names || leases[0].IP != ip {
		t.Fatalf("List = %+v, want one lease at %v with names %+v", leases, ip, names)
	}

	renamed := names
	renamed.Service = "primary"
	if changed, err := reopened.SetNames(k, renamed); err != nil || !changed {
		t.Fatalf("SetNames after a rename = %v, %v; want true, nil", changed, err)
	}
	if got, _, _ := reopened.Lookup(k); got != ip {
		t.Fatalf("rename moved the lease from %v to %v", ip, got)
	}
}

func TestSetNames_WithoutLease(t *testing.T) {
	r, _ := newTestRegistry(t)
	_, err := r.SetNames(testKey("none"), hostname.Names{Service: "x"})
	if !errors.Is(err, ErrNotLeased) {
		t.Fatalf("SetNames error = %v, want ErrNotLeased", err)
	}
}

func TestLease_ExhaustedRange(t *testing.T) {
	// 127.77.0.0/30 holds .0 to .3; .0 is skipped, leaving three addresses.
	tiny := netip.MustParsePrefix("127.77.0.0/30")
	r, _ := newTestRegistry(t, WithRange(tiny))
	mustLease(t, r, testKey("a"))
	mustLease(t, r, testKey("b"))
	if _, err := r.Forget(testKey("b")); err != nil {
		t.Fatal(err)
	}
	mustLease(t, r, testKey("c"))

	_, err := r.Lease(testKey("d"))
	var exhausted *ExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("Lease error = %v, want *ExhaustedError", err)
	}
	if exhausted.Range != tiny {
		t.Fatalf("ExhaustedError.Range = %v, want %v", exhausted.Range, tiny)
	}
	// Existing leases still resolve when the range is full.
	if ip := mustLease(t, r, testKey("a")); ip != netip.MustParseAddr("127.77.0.1") {
		t.Fatalf("existing lease = %v", ip)
	}
}

func TestLease_SkipsBroadcastLikeOctet(t *testing.T) {
	r, _ := newTestRegistry(t, WithRange(netip.MustParsePrefix("127.77.0.0/23")))
	var last netip.Addr
	for i := range 255 {
		last = mustLease(t, r, testKey(strconv.Itoa(i)))
		if b := last.As4()[3]; b == 0 || b == 255 {
			t.Fatalf("leased %v", last)
		}
	}
	if want := netip.MustParseAddr("127.77.1.1"); last != want {
		t.Fatalf("255th lease = %v, want %v", last, want)
	}
}

func TestOpen_InvalidRange(t *testing.T) {
	tests := []string{"10.0.0.0/16", "::1/128", "127.77.0.0/31"}
	for _, p := range tests {
		t.Run(p, func(t *testing.T) {
			_, err := Open(filepath.Join(t.TempDir(), "r.json"), WithRange(netip.MustParsePrefix(p)))
			if err == nil {
				t.Fatal("Open succeeded, want error")
			}
		})
	}
}

func TestLoad_BadFile(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr error
	}{
		{name: "not json", content: "{nope", wantErr: ErrCorrupt},
		{name: "missing version", content: `{"leases":[]}`, wantErr: ErrUnsupportedVersion},
		{name: "future version", content: `{"version":99,"leases":[]}`, wantErr: ErrUnsupportedVersion},
		{name: "bad ip", content: `{"version":1,"leases":[{"instance":"https://x.io","organization_id":"o","service_id":"s","ip":"nope"}]}`, wantErr: ErrCorrupt},
		{name: "duplicate ip", content: `{"version":1,"leases":[` +
			`{"instance":"https://x.io","organization_id":"o","service_id":"a","ip":"127.77.0.1"},` +
			`{"instance":"https://x.io","organization_id":"o","service_id":"b","ip":"127.77.0.1"}]}`, wantErr: ErrCorrupt},
		{name: "duplicate key", content: `{"version":1,"leases":[` +
			`{"instance":"https://x.io","organization_id":"o","service_id":"a","ip":"127.77.0.1"},` +
			`{"instance":"https://x.io","organization_id":"o","service_id":"a","ip":"127.77.0.2"}]}`, wantErr: ErrCorrupt},
		{name: "lease on retired ip", content: `{"version":1,"retired":["127.77.0.1"],"leases":[` +
			`{"instance":"https://x.io","organization_id":"o","service_id":"a","ip":"127.77.0.1"}]}`, wantErr: ErrCorrupt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, path := newTestRegistry(t)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := r.Lease(testKey("x"))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Lease error = %v, want %v", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("error %q does not name the file", err)
			}
			// The bad file must be left untouched for the user to inspect.
			if b, _ := os.ReadFile(path); string(b) != tt.content {
				t.Fatalf("bad file was modified: %s", b)
			}
		})
	}
}

func TestLease_PersistsVersionedFileAtomically(t *testing.T) {
	r, path := newTestRegistry(t)
	mustLease(t, r, testKey("a"))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"version": 1`) {
		t.Fatalf("file lacks schema version: %s", b)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".addresses.json.tmp") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestLease_ConcurrentGoroutines(t *testing.T) {
	_, path := newTestRegistry(t)
	const workers, perWorker = 8, 10
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ips  = map[netip.Addr]string{}
		errs []error
	)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A separate Registry per goroutine models separate CLI
			// processes sharing only the file.
			r, err := Open(path)
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			for i := range perWorker {
				name := fmt.Sprintf("w%d-%d", w, i)
				ip, err := r.Lease(testKey(name))
				shared, serr := r.Lease(testKey("shared"))
				mu.Lock()
				if err != nil || serr != nil {
					errs = append(errs, errors.Join(err, serr))
				} else {
					if prev, dup := ips[ip]; dup {
						errs = append(errs, fmt.Errorf("%s and %s share %v", prev, name, ip))
					}
					ips[ip] = name
					if prev, dup := ips[shared]; dup && prev != "shared" {
						errs = append(errs, fmt.Errorf("shared collides with %s", prev))
					}
					ips[shared] = "shared"
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		t.Error(err)
	}
	if want := workers*perWorker + 1; len(ips) != want {
		t.Fatalf("distinct addresses = %d, want %d", len(ips), want)
	}
}

const helperEnv = "DOKTUNNEL_REGISTRY_HELPER"

// TestHelperProcess is not a real test: TestLease_ConcurrentProcesses
// re-executes the test binary to run it as a separate OS process.
func TestHelperProcess(t *testing.T) {
	spec := os.Getenv(helperEnv)
	if spec == "" {
		t.Skip("helper process only")
	}
	path, worker, _ := strings.Cut(spec, "|")
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 15 {
		if _, err := r.Lease(testKey(fmt.Sprintf("p%s-%d", worker, i))); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Lease(testKey("shared")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLease_ConcurrentProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	_, path := newTestRegistry(t)
	const procs = 4
	cmds := make([]*exec.Cmd, procs)
	for p := range procs {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.count=1")
		cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%s|%d", helperEnv, path, p))
		if err := cmd.Start(); err != nil {
			t.Fatalf("starting helper: %v", err)
		}
		cmds[p] = cmd
	}
	for _, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("helper failed: %v", err)
		}
	}

	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	leases, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if want := procs*15 + 1; len(leases) != want {
		t.Fatalf("leases = %d, want %d (lost update?)", len(leases), want)
	}
	seen := map[netip.Addr]bool{}
	for _, l := range leases {
		if seen[l.IP] {
			t.Fatalf("address %v leased twice", l.IP)
		}
		seen[l.IP] = true
	}
}

func TestStateDir(t *testing.T) {
	home := func() (string, error) { return "/home/u", nil }
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	tests := []struct {
		name string
		goos string
		env  map[string]string
		want string
	}{
		{name: "linux xdg", goos: "linux", env: map[string]string{"XDG_STATE_HOME": "/xdg/state"}, want: filepath.Join("/xdg/state", "doktunnel")},
		{name: "linux default", goos: "linux", want: filepath.Join("/home/u", ".local", "state", "doktunnel")},
		{name: "linux relative xdg ignored", goos: "linux", env: map[string]string{"XDG_STATE_HOME": "rel"}, want: filepath.Join("/home/u", ".local", "state", "doktunnel")},
		{name: "darwin default", goos: "darwin", want: filepath.Join("/home/u", ".local", "state", "doktunnel")},
		{name: "windows", goos: "windows", env: map[string]string{"LOCALAPPDATA": `C:\Users\u\AppData\Local`}, want: filepath.Join(`C:\Users\u\AppData\Local`, "doktunnel")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := stateDir(tt.goos, env(tt.env), home)
			if err != nil || got != tt.want {
				t.Fatalf("stateDir = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
	if _, err := stateDir("windows", env(nil), home); err == nil {
		t.Fatal("windows without LOCALAPPDATA: want error")
	}
}
