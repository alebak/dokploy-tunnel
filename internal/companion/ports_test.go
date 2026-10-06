package companion

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"

	"github.com/alebak/dokploy-tunnel/internal/dokploy"
	"github.com/alebak/dokploy-tunnel/internal/tunnel"
)

// listingBridge is a pipeBridge that also lists ports.
type listingBridge struct {
	*pipeBridge
	ports []tunnel.Port
	err   error

	mu     sync.Mutex
	listed []Target
}

var _ PortLister = (*listingBridge)(nil)

func (b *listingBridge) ExposedPorts(_ context.Context, target Target) ([]tunnel.Port, error) {
	b.mu.Lock()
	b.listed = append(b.listed, target)
	b.mu.Unlock()
	return b.ports, b.err
}

func (b *listingBridge) calls() []Target {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.listed)
}

// newListingCompanion is a testCompanion whose bridge lists ports.
func newListingCompanion(t *testing.T, limits Limits) (*testCompanion, *listingBridge) {
	t.Helper()
	var lb *listingBridge
	tc := newWrappedCompanion(t, limits, func(b *pipeBridge) Bridge {
		lb = &listingBridge{pipeBridge: b}
		return lb
	})
	return tc, lb
}

func (tc *testCompanion) portsURL(query string) string {
	return tc.http.URL + tunnel.PortsPath + "?" + query
}

const pgRef = "serviceType=postgres&serviceId=pg_main"

// assertPorts checks a successful ports response and returns its raw body.
func assertPorts(t *testing.T, resp *http.Response, want []tunnel.Port) string {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d %s, want 200", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var body tunnel.PortsResponse
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("body %q: %v", raw, err)
	}
	if !slices.Equal(body.Ports, want) {
		t.Errorf("ports = %v, want %v", body.Ports, want)
	}
	return string(raw)
}

func TestServer_Ports_ListsTheTargetsPorts(t *testing.T) {
	tc, lb := newListingCompanion(t, Limits{})
	lb.ports = []tunnel.Port{{Port: 5432, Protocol: "tcp"}, {Port: 9187, Protocol: "tcp"}}

	raw := assertPorts(t, request(t, http.MethodGet, tc.portsURL(pgRef), ownerKey, nil), lb.ports)
	if want := `{"ports":[{"port":5432,"protocol":"tcp"},{"port":9187,"protocol":"tcp"}]}`; strings.TrimSpace(raw) != want {
		t.Errorf("body = %s, want %s", raw, want)
	}

	calls := lb.calls()
	if len(calls) != 1 {
		t.Fatalf("ExposedPorts called %d times, want 1", len(calls))
	}
	if calls[0].Target != (tunnel.Target{ServiceType: dokploy.ServicePostgres, ServiceID: "pg_main"}) ||
		calls[0].Service.AppName != "shop-pgmain-a1b2c3" {
		t.Errorf("listed target = %+v", calls[0])
	}
	if tc.bridge.opened() {
		t.Error("listing ports opened a stream")
	}
}

func TestServer_Ports_ComposeServiceOnTheExpectedServer(t *testing.T) {
	tc, lb := newListingCompanion(t, Limits{})
	lb.ports = []tunnel.Port{{Port: 5432, Protocol: "tcp"}}
	query := "serviceType=compose_service&serviceId=cmp_myapp%2Fpostgres&serverId=local"
	assertPorts(t, request(t, http.MethodGet, tc.portsURL(query), ownerKey, nil), lb.ports)
	if calls := lb.calls(); len(calls) != 1 || calls[0].ComposeService != "postgres" {
		t.Errorf("listed targets = %+v", calls)
	}
}

func TestServer_Ports_EmptyIsAList(t *testing.T) {
	tests := []struct {
		name string
		wrap func(*pipeBridge) Bridge
	}{
		{"no ports known", func(b *pipeBridge) Bridge { return &listingBridge{pipeBridge: b} }},
		{"bridge that cannot list ports", func(b *pipeBridge) Bridge { return b }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := newWrappedCompanion(t, Limits{}, tt.wrap)
			raw := assertPorts(t, request(t, http.MethodGet, tc.portsURL(pgRef), ownerKey, nil), nil)
			if want := `{"ports":[]}`; strings.TrimSpace(raw) != want {
				t.Errorf("body = %s, want %s", raw, want)
			}
		})
	}
}

func TestServer_Ports_Rejected(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		key     string
		query   string
		headers http.Header
		status  int
		code    tunnel.Code
	}{
		{"not a GET", http.MethodPost, ownerKey, pgRef, nil, 405, tunnel.CodeMethodNotAllowed},
		{"cross-site origin", http.MethodGet, ownerKey, pgRef, http.Header{"Origin": {"https://evil.example.com"}}, 403, tunnel.CodeForbiddenOrigin},
		{"missing serviceId", http.MethodGet, ownerKey, "serviceType=postgres", nil, 400, tunnel.CodeInvalidArgument},
		{"malformed serverId", http.MethodGet, ownerKey, pgRef + "&serverId=a%2Fb", nil, 400, tunnel.CodeInvalidArgument},
		{"key too long", http.MethodGet, strings.Repeat("k", tunnel.MaxAPIKeyBytes+1), pgRef, nil, 400, tunnel.CodeInvalidArgument},
		{"no key", http.MethodGet, "", pgRef, nil, 401, tunnel.CodeUnauthenticated},
		{"invalid key", http.MethodGet, "dok_revoked", pgRef, nil, 403, tunnel.CodePermissionDenied},
		{"key of another organization", http.MethodGet, foreignKey, pgRef, nil, 403, tunnel.CodePermissionDenied},
		{"member without access", http.MethodGet, memberKey, pgRef, nil, 403, tunnel.CodePermissionDenied},
		{"unknown compose service", http.MethodGet, ownerKey, "serviceType=compose_service&serviceId=cmp_myapp%2Fredis", nil, 404, tunnel.CodeNotFound},
		{"service on another server", http.MethodGet, ownerKey, "serviceType=postgres&serviceId=pg_edge", nil, 421, tunnel.CodeWrongServer},
		{"another server expected", http.MethodGet, ownerKey, pgRef + "&serverId=srv_edge", nil, 421, tunnel.CodeWrongServer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc, lb := newListingCompanion(t, Limits{})
			resp := request(t, tt.method, tc.portsURL(tt.query), tt.key, tt.headers)
			body := assertRejected(t, resp, tt.status, tt.code)
			if tt.code == tunnel.CodeWrongServer && body.ExpectedServerID == "" {
				t.Error("no expected_server_id")
			}
			if calls := lb.calls(); len(calls) != 0 {
				t.Errorf("ports were listed for a rejected request: %+v", calls)
			}
		})
	}
}

func TestServer_Ports_RejectedBeforeDokploy(t *testing.T) {
	tc, _ := newListingCompanion(t, Limits{})
	for _, query := range []string{"serviceType=postgres", pgRef} {
		request(t, http.MethodGet, tc.portsURL(query), "", nil)
	}
	if calls := tc.fake.recorded(); len(calls) != 0 {
		t.Errorf("Dokploy was called for unauthenticated or malformed requests: %v", calls)
	}
}

func TestServer_Ports_Failures(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*testCompanion, *listingBridge)
		status int
		code   tunnel.Code
	}{
		{"Dokploy is broken", func(tc *testCompanion, _ *listingBridge) { tc.fake.broken = true }, 502, tunnel.CodeUnreachable},
		{"nothing running", func(_ *testCompanion, lb *listingBridge) {
			lb.err = errors.Join(ErrTargetUnreachable, errors.New("no running task"))
		}, 502, tunnel.CodeTargetUnreachable},
		{"Docker is slow", func(_ *testCompanion, lb *listingBridge) { lb.err = context.DeadlineExceeded }, 504, tunnel.CodeTimeout},
		{"companion drains", func(tc *testCompanion, _ *listingBridge) {
			if err := tc.server.Drain(context.Background()); err != nil {
				t.Fatal(err)
			}
		}, 503, tunnel.CodeUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc, lb := newListingCompanion(t, Limits{})
			tt.setup(tc, lb)
			assertRejected(t, request(t, http.MethodGet, tc.portsURL(pgRef), ownerKey, nil), tt.status, tt.code)
		})
	}
}

func TestServer_Ports_TakesNoTunnelSlot(t *testing.T) {
	tc, lb := newListingCompanion(t, Limits{PerKey: 1, Total: 1})
	lb.ports = []tunnel.Port{{Port: 5432, Protocol: "tcp"}}

	// Listing never takes a slot, so a tunnel still fits afterwards...
	for range 3 {
		assertPorts(t, request(t, http.MethodGet, tc.portsURL(pgRef), ownerKey, nil), lb.ports)
	}
	c := tc.dial(t, ownerKey, pgQuery)
	tc.bridge.target(t)
	defer c.Close(websocket.StatusNormalClosure, "")

	// ...and the full slots never refuse a listing.
	assertPorts(t, request(t, http.MethodGet, tc.portsURL(pgRef), ownerKey, nil), lb.ports)
}
