package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
	"github.com/alebak/dokploy-tunnel/internal/dokploy"
)

// servicesFixture is what project.all returns for an owner key: databases
// such as pg_main come without name and status.
func servicesFixture() []dokploy.Project {
	return []dokploy.Project{
		{
			ID: "prj_shop", Name: "shop",
			Environments: []dokploy.Environment{
				{
					ID: "env_shop_prod", Name: "production", IsDefault: true,
					Services: []dokploy.Service{
						{ID: "app_web", Type: dokploy.ServiceApplication, Name: "web", Status: "done"},
						{ID: "cmp_stack", Type: dokploy.ServiceCompose, Name: "observability", Status: "running"},
						{ID: "pg_main", Type: dokploy.ServicePostgres},
						{ID: "redis_cache", Type: dokploy.ServiceRedis, Name: "cache", Status: "error"},
					},
				},
				{
					ID: "env_shop_stg", Name: "staging",
					Services: []dokploy.Service{
						{ID: "maria_stg", Type: dokploy.ServiceMariaDB, Name: "legacy-db", Status: "idle"},
						{ID: "libsql_edge", Type: dokploy.ServiceLibSQL},
					},
				},
				{ID: "env_shop_preview", Name: "preview", Services: []dokploy.Service{}},
			},
		},
		{ID: "prj_empty", Name: "empty", Environments: []dokploy.Environment{}},
	}
}

func newServicesHarness(t *testing.T) *contextHarness {
	t.Helper()
	h := newContextHarness(t)
	h.add("prod", "https://panel.example.com")
	h.api.projects = servicesFixture()
	// The owner key's project.all leaves out database names: pg_main is
	// completed from postgres.one, and libsql.one fails for libsql_edge.
	h.api.details = map[string]dokploy.ServiceDetails{
		"postgres/pg_main": {Service: dokploy.Service{ID: "pg_main", Type: dokploy.ServicePostgres, Name: "main-db", Status: "done"}},
	}
	h.api.detailErrs = map[string]error{
		"libsql/libsql_edge": fmt.Errorf("%w (HTTP 401 from libsql.one)", dokploy.ErrUnauthorized),
	}
	return h
}

// indentJSON pretty-prints a JSON result so its golden file is reviewable;
// field order and values are kept exactly.
func indentJSON(t *testing.T, s string) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Indent(&b, []byte(s), "", "  "); err != nil {
		t.Fatalf("output is not JSON: %v (%q)", err, s)
	}
	return b.String() + "\n"
}

func TestServices_Golden(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		golden     string
		wantStderr string
	}{
		{"human", nil, "services.golden", "warning: libsql libsql_edge: name and status unknown: " +
			"Dokploy rejected the API key (HTTP 401 from libsql.one)\n"},
		// In JSON mode the warning is part of the service instead.
		{"json", []string{"--json"}, "services.json.golden", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newServicesHarness(t)
			r := h.run("", false, append([]string{"services"}, tt.args...)...)
			if r.exit != 0 {
				t.Fatalf("exit = %d (stdout %q, stderr %q)", r.exit, r.stdout, r.stderr)
			}
			if r.stderr != tt.wantStderr {
				t.Errorf("stderr = %q, want %q", r.stderr, tt.wantStderr)
			}
			if h.api.base != "https://panel.example.com" || h.api.key != "key-prod" {
				t.Errorf("API built with base %q key %q, want the prod context and its key", h.api.base, h.api.key)
			}
			got := r.stdout
			if tt.args != nil {
				got = indentJSON(t, got)
			}
			assertGolden(t, tt.golden, got)
		})
	}
}

func TestServices_ProjectFilter(t *testing.T) {
	tests := []struct {
		name    string
		project string
		want    []string
	}{
		{"by name", "shop", []string{"prj_shop"}},
		{"by ID", "prj_empty", []string{"prj_empty"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newServicesHarness(t)
			r := h.mustRun("", "services", "--project", tt.project, "--json")
			got := decodeJSON[servicesJSON](t, r.stdout)
			var ids []string
			for _, p := range got.Projects {
				ids = append(ids, p.ID)
			}
			if fmt.Sprint(ids) != fmt.Sprint(tt.want) {
				t.Errorf("projects = %v, want %v", ids, tt.want)
			}
		})
	}
}

func TestServices_NoProjects(t *testing.T) {
	h := newContextHarness(t)
	h.add("prod", "https://panel.example.com")

	r := h.mustRun("", "services", "--json")
	if got := strings.TrimSpace(r.stdout); got != `{"context":"prod","projects":[]}` {
		t.Errorf("JSON output = %s", got)
	}
	r = h.mustRun("", "services")
	if r.stdout != "" || !strings.Contains(r.stderr, "No projects") {
		t.Errorf("human output: stdout %q, stderr %q; want a notice on stderr only", r.stdout, r.stderr)
	}
}

func TestServices_Errors(t *testing.T) {
	tests := []struct {
		name        string
		noContext   bool
		projectsErr error
		args        []string
		want        clierr.Code
		mention     string
	}{
		{"no context", true, nil, nil, clierr.MissingInput, "--context"},
		{"unknown context", false, nil, []string{"--context", "nope"}, clierr.InvalidArgument, "context list"},
		{"unknown project", false, nil, []string{"--project", "nope"}, clierr.NotFound, "doktunnel services"},
		{"unexpected argument", false, nil, []string{"extra"}, clierr.InvalidArgument, "--project"},
		{"rejected key", false, dokploy.ErrUnauthorized, nil, clierr.PermissionDenied, "API key"},
		{"unreachable panel", false, dokploy.ErrUnreachable, nil, clierr.Unreachable, "panel.example.com"},
		{"not found", false, fmt.Errorf("%w (project.all)", dokploy.ErrNotFound), nil, clierr.NotFound, "project.all"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newContextHarness(t)
			if !tt.noContext {
				h.add("prod", "https://panel.example.com")
			}
			h.api.projects = servicesFixture()
			h.api.projectsErr = tt.projectsErr
			r := h.run("", false, append([]string{"services", "--json", "--no-input"}, tt.args...)...)
			e := decodeError(t, r.stdout)
			if e.Code != tt.want || !strings.Contains(e.Message+" "+e.Hint, tt.mention) {
				t.Errorf("error = %+v, want code %q mentioning %q", e, tt.want, tt.mention)
			}
			if r.exit != tt.want.ExitCode() {
				t.Errorf("exit = %d, want %d", r.exit, tt.want.ExitCode())
			}
			if r.stderr != "" {
				t.Errorf("stderr = %q, want empty in JSON mode", r.stderr)
			}
		})
	}
}

func TestServices_DetailsOnlyForIncompleteServices(t *testing.T) {
	h := newServicesHarness(t)
	h.mustRun("", "services", "--json")
	got := map[string]bool{}
	for _, call := range h.api.detailCalls {
		got[call] = true
	}
	want := map[string]bool{"postgres/pg_main": true, "libsql/libsql_edge": true}
	if fmt.Sprint(got) != fmt.Sprint(want) || len(h.api.detailCalls) != len(want) {
		t.Errorf("detail calls = %v, want one each for %v", h.api.detailCalls, want)
	}
}

// slowDetailer records how many Details calls run at once.
type slowDetailer struct {
	mu       sync.Mutex
	inFlight int
	max      int
}

func (d *slowDetailer) Details(ctx context.Context, typ dokploy.ServiceType, id string) (dokploy.ServiceDetails, error) {
	d.mu.Lock()
	d.inFlight++
	d.max = max(d.max, d.inFlight)
	d.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	d.mu.Lock()
	d.inFlight--
	d.mu.Unlock()
	return dokploy.ServiceDetails{Service: dokploy.Service{ID: id, Type: typ, Name: "db-" + id, Status: "done"}}, nil
}

func manyDatabases(n int) []dokploy.Project {
	env := dokploy.Environment{ID: "env", Name: "production"}
	for i := range n {
		env.Services = append(env.Services, dokploy.Service{ID: fmt.Sprintf("pg_%d", i), Type: dokploy.ServicePostgres})
	}
	return []dokploy.Project{{ID: "prj", Name: "p", Environments: []dokploy.Environment{env}}}
}

func TestFillServiceDetails_BoundsConcurrency(t *testing.T) {
	d := &slowDetailer{}
	projects := manyDatabases(3 * detailConcurrency)
	warnings, err := fillServiceDetails(context.Background(), d, projects)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	if d.max > detailConcurrency {
		t.Errorf("%d detail calls in flight, want at most %d", d.max, detailConcurrency)
	}
	for _, s := range projects[0].Environments[0].Services {
		if s.Name != "db-"+s.ID || s.Status != "done" {
			t.Errorf("service %s = %+v, want its name and status filled in", s.ID, s)
		}
	}
}

func TestFillServiceDetails_Canceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	api := &fakeAPI{}
	if _, err := fillServiceDetails(ctx, api, manyDatabases(10)); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if len(api.detailCalls) != 0 {
		t.Errorf("detail calls = %v after cancellation, want none", api.detailCalls)
	}
}
