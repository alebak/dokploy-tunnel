package companion

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/alebak/dokploy-tunnel/internal/dokploy"
)

// Keys the fake Dokploy API knows.
const (
	ownerKey   = "dok_owner_key"
	memberKey  = "dok_member_key"
	foreignKey = "dok_other_org_key"
)

// fakeService is a service stored in the fake Dokploy API.
type fakeService struct {
	typ      dokploy.ServiceType
	serverID string
	// composeServices are the services of a compose stack's stored compose
	// file; nil means no compose file is stored yet.
	composeServices []string
}

// fakeDokploy serves the procedures the companion may call, answering the
// way Dokploy does: <type>.one checks the key's access to the service
// first (401 for a member without it, or a key of another organization),
// then answers 404 NOT_FOUND for a service that does not exist.
type fakeDokploy struct {
	t        *testing.T
	services map[string]fakeService
	// memberGrants are the service IDs memberKey may read; ownerKey reads
	// everything in the organization, foreignKey nothing.
	memberGrants map[string]bool
	// broken makes every procedure fail with HTTP 500.
	broken bool
	// block, when set, holds every request until it is closed.
	block chan struct{}

	mu    sync.Mutex
	calls []string
	keys  []string
}

func newFakeDokploy(t *testing.T) *fakeDokploy {
	t.Helper()
	return &fakeDokploy{
		t: t,
		services: map[string]fakeService{
			"pg_main":   {typ: dokploy.ServicePostgres},
			"pg_edge":   {typ: dokploy.ServicePostgres, serverID: "srv_edge"},
			"app_web":   {typ: dokploy.ServiceApplication},
			"cmp_myapp": {typ: dokploy.ServiceCompose, composeServices: []string{"postgres", "pgadmin"}},
			"cmp_new":   {typ: dokploy.ServiceCompose},
		},
		memberGrants: map[string]bool{"app_web": true},
	}
}

// start serves the fake API and returns a client for it with no API key.
func (f *fakeDokploy) start() *dokploy.Client {
	f.t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	f.t.Cleanup(srv.Close)
	u, err := dokploy.ParseBaseURL(srv.URL)
	if err != nil {
		f.t.Fatal(err)
	}
	return dokploy.New(u, "")
}

// recorded returns the procedures called so far, with their queries.
func (f *fakeDokploy) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// usedKeys returns the API keys of the requests so far.
func (f *fakeDokploy) usedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

func (f *fakeDokploy) serve(w http.ResponseWriter, r *http.Request) {
	procedure := strings.TrimPrefix(r.URL.Path, "/api/")
	key := r.Header.Get("x-api-key")
	f.mu.Lock()
	f.calls = append(f.calls, procedure+"?"+r.URL.RawQuery)
	f.keys = append(f.keys, key)
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-f.block:
		case <-r.Context().Done():
			return
		}
	}

	if f.broken {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if key != ownerKey && key != memberKey && key != foreignKey {
		fail(w, http.StatusUnauthorized, "UNAUTHORIZED")
		return
	}

	router, proc, _ := strings.Cut(procedure, ".")
	typ := dokploy.ServiceType(router)
	var id string
	switch proc {
	case "one":
		id = r.URL.Query().Get(router + "Id")
	case "loadServices":
		if typ != dokploy.ServiceCompose {
			break
		}
		if got := r.URL.Query().Get("type"); got != "cache" {
			f.t.Errorf("compose.loadServices with type=%q, want cache", got)
		}
		id = r.URL.Query().Get("composeId")
	}
	if id == "" {
		f.t.Errorf("unexpected Dokploy call %s?%s", procedure, r.URL.RawQuery)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if key == foreignKey || (key == memberKey && !f.memberGrants[id]) {
		fail(w, http.StatusUnauthorized, "UNAUTHORIZED")
		return
	}
	svc, ok := f.services[id]
	if !ok || svc.typ != typ {
		fail(w, http.StatusNotFound, "NOT_FOUND")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if proc == "loadServices" {
		if svc.composeServices == nil {
			fail(w, http.StatusNotFound, "NOT_FOUND")
			return
		}
		_ = json.NewEncoder(w).Encode(svc.composeServices)
		return
	}
	body := map[string]any{
		router + "Id": id,
		"name":        "service " + id,
		"appName":     "shop-" + strings.ReplaceAll(id, "_", "") + "-a1b2c3",
		"serverId":    nil,
		"networkIds":  []string{"net_backend"},
		"env":         "SECRET=do-not-log",
	}
	if svc.serverID != "" {
		body["serverId"] = svc.serverID
	}
	_ = json.NewEncoder(w).Encode(body)
}

// fail writes a Dokploy tRPC error.
func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"message":"error","code":%q}`, code)
}
