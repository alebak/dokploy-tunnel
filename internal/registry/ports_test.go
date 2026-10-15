package registry

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// mappingSummary renders mappings as "<service>→<local>" strings, with
// "(was <n>)" for a replaced stored port.
func mappingSummary(ms []PortMapping) []string {
	var s []string
	for _, m := range ms {
		line := fmt.Sprintf("%d→%d", m.ServicePort, m.LocalPort)
		if m.Replaced != 0 {
			line += fmt.Sprintf(" (was %d)", m.Replaced)
		}
		s = append(s, line)
	}
	return s
}

func TestPreferredLocalPort(t *testing.T) {
	tests := []struct{ port, want int }{
		{80, 8080}, {443, 8443}, {25, 8025}, {110, 8110}, {22, 8022}, {1, 8001}, {1023, 9023},
		{1024, 1024}, {3000, 3000}, {5432, 5432}, {8080, 8080}, {65535, 65535},
	}
	for _, tt := range tests {
		if got := PreferredLocalPort(tt.port); got != tt.want {
			t.Errorf("PreferredLocalPort(%d) = %d, want %d", tt.port, got, tt.want)
		}
	}
}

func TestMapPorts_Rule(t *testing.T) {
	tests := []struct {
		name    string
		ports   []int
		exposed []int
		want    []string
	}{
		{name: "ports from 1024 up keep their number", ports: []int{3000, 3001, 5432, 8080}, want: []string{"3000→3000", "3001→3001", "5432→5432", "8080→8080"}},
		{name: "80 and 443 shift by 8000", ports: []int{80, 443}, want: []string{"80→8080", "443→8443"}},
		{name: "a port the service exposes itself is skipped", ports: []int{80, 8080}, want: []string{"80→8081", "8080→8080"}},
		{name: "companion order does not matter", ports: []int{8080, 80}, want: []string{"8080→8080", "80→8081"}},
		{name: "exposed but not forwarded ports are skipped too", ports: []int{80}, exposed: []int{80, 8080, 8081}, want: []string{"80→8082"}},
		{name: "a remapped port is not handed to another low port", ports: []int{80, 81}, exposed: []int{8081}, want: []string{"80→8080", "81→8082"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := newTestRegistry(t)
			k := testKey("svc")
			mustLease(t, r, k)
			got, err := r.MapPorts(k, tt.ports, tt.exposed, nil)
			if err != nil {
				t.Fatalf("MapPorts: %v", err)
			}
			if s := mappingSummary(got); !slices.Equal(s, tt.want) {
				t.Errorf("mappings = %q, want %q", s, tt.want)
			}
		})
	}
}

func TestMapPorts_EachServiceHasItsOwnPorts(t *testing.T) {
	r, _ := newTestRegistry(t)
	for _, svc := range []string{"api", "web", "admin"} {
		k := testKey(svc)
		mustLease(t, r, k)
		got, err := r.MapPorts(k, []int{80}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if s := mappingSummary(got); !slices.Equal(s, []string{"80→8080"}) {
			t.Errorf("%s: mappings = %q, want 80→8080 on its own address", svc, s)
		}
	}
}

func TestMapPorts_PersistsAndReusesTheMapping(t *testing.T) {
	r, path := newTestRegistry(t)
	k := testKey("svc")
	mustLease(t, r, k)
	if _, err := r.MapPorts(k, []int{80, 8080}, nil, nil); err != nil {
		t.Fatal(err)
	}

	// A new Registry on the same file reuses 8081 although 8080 is free
	// now, and whatever the order of the ports.
	r2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r2.MapPorts(k, []int{443, 80}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := mappingSummary(got); !slices.Equal(s, []string{"443→8443", "80→8081"}) {
		t.Errorf("mappings = %q, want the stored 80→8081 and a new 443→8443", s)
	}
	leases, err := r2.List()
	if err != nil {
		t.Fatal(err)
	}
	if want := map[int]int{80: 8081, 443: 8443}; len(leases) != 1 || !maps.Equal(leases[0].LocalPorts, want) {
		t.Errorf("leases = %+v, want local ports %v", leases, want)
	}
}

func TestMapPorts_StoredPortNowExposedIsRecomputed(t *testing.T) {
	r, _ := newTestRegistry(t)
	k := testKey("svc")
	mustLease(t, r, k)
	if _, err := r.MapPorts(k, []int{80}, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, err := r.MapPorts(k, []int{80, 8080}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := mappingSummary(got); !slices.Equal(s, []string{"80→8081 (was 8080)", "8080→8080"}) {
		t.Errorf("mappings = %q, want 80 moved off 8080 and reported", s)
	}
	// The new port is stored, so the next forward reports nothing.
	got, err = r.MapPorts(k, []int{80, 8080}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := mappingSummary(got); !slices.Equal(s, []string{"80→8081", "8080→8080"}) {
		t.Errorf("mappings = %q, want the stored 80→8081", s)
	}
}

func TestMapPorts_FixedLocalPort(t *testing.T) {
	r, _ := newTestRegistry(t)
	k := testKey("svc")
	mustLease(t, r, k)
	if _, err := r.MapPorts(k, []int{443}, nil, nil); err != nil {
		t.Fatal(err)
	}

	got, err := r.MapPorts(k, []int{80}, nil, map[int]int{80: 9000})
	if err != nil {
		t.Fatal(err)
	}
	if s := mappingSummary(got); !slices.Equal(s, []string{"80→9000"}) {
		t.Errorf("mappings = %q, want the fixed port", s)
	}
	// A fixed port is not stored: the rule applies again afterwards.
	got, err = r.MapPorts(k, []int{80}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := mappingSummary(got); !slices.Equal(s, []string{"80→8080"}) {
		t.Errorf("mappings = %q, want the rule after a fixed port", s)
	}

	collisions := []struct {
		name          string
		ports         []int
		exposed       []int
		fixed         map[int]int
		wantLocal     int
		wantOtherPort int
	}{
		{name: "a port the service exposes", ports: []int{80}, exposed: []int{80, 3000}, fixed: map[int]int{80: 3000}, wantLocal: 3000, wantOtherPort: 3000},
		{name: "the stored local port of another port", ports: []int{80}, fixed: map[int]int{80: 8443}, wantLocal: 8443, wantOtherPort: 443},
	}
	for _, tt := range collisions {
		t.Run(tt.name, func(t *testing.T) {
			_, err := r.MapPorts(k, tt.ports, tt.exposed, tt.fixed)
			var ce *PortCollisionError
			if !errors.As(err, &ce) {
				t.Fatalf("err = %v, want a PortCollisionError", err)
			}
			if ce.ServicePort != 80 || ce.LocalPort != tt.wantLocal || ce.OtherPort != tt.wantOtherPort {
				t.Errorf("collision = %+v, want port 80 on %d colliding with port %d", ce, tt.wantLocal, tt.wantOtherPort)
			}
		})
	}
}

func TestMapPorts_WithoutLease(t *testing.T) {
	r, _ := newTestRegistry(t)
	if _, err := r.MapPorts(testKey("svc"), []int{80}, nil, nil); !errors.Is(err, ErrNotLeased) {
		t.Errorf("err = %v, want ErrNotLeased", err)
	}
}

func TestMapPorts_ExhaustedPorts(t *testing.T) {
	r, _ := newTestRegistry(t)
	k := testKey("svc")
	mustLease(t, r, k)
	var exposed []int
	for p := 9023; p <= 65535; p++ {
		exposed = append(exposed, p)
	}
	_, err := r.MapPorts(k, []int{1023}, exposed, nil)
	if err == nil || !errors.Is(err, ErrNoLocalPort) {
		t.Errorf("err = %v, want ErrNoLocalPort", err)
	}
}

// An addresses.json written before local ports existed loads unchanged and
// gains them additively, without a version bump.
func TestMapPorts_OlderFileLoadsAndGainsLocalPorts(t *testing.T) {
	r, path := newTestRegistry(t)
	old := `{"version":1,"leases":[{"instance":"https://dokploy.example.com","organization_id":"org-1","service_id":"svc",` +
		`"ip":"127.77.0.9","created_at":"2026-01-01T00:00:00Z","names":{"context":"prod"}}]}`
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	leases, err := r.List()
	if err != nil {
		t.Fatalf("List of an older file: %v", err)
	}
	if len(leases) != 1 || leases[0].IP.String() != "127.77.0.9" || leases[0].LocalPorts != nil {
		t.Fatalf("leases = %+v, want the old lease without local ports", leases)
	}
	if _, err := r.MapPorts(testKey("svc"), []int{80}, nil, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := load(path)
	if err != nil {
		t.Fatalf("reloading: %v (%s)", err, b)
	}
	if st.Version != 1 || st.Leases[0].IP.String() != "127.77.0.9" || st.Leases[0].Names.Context != "prod" ||
		!maps.Equal(st.Leases[0].LocalPorts, map[int]int{80: 8080}) {
		t.Errorf("file = %s, want version 1, the same lease and local_ports {80: 8080}", b)
	}
}
