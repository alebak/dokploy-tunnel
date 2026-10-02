package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

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
		name   string
		args   []string
		golden string
	}{
		{"human", nil, "services.golden"},
		{"json", []string{"--json"}, "services.json.golden"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newServicesHarness(t)
			r := h.run("", false, append([]string{"services"}, tt.args...)...)
			if r.exit != 0 {
				t.Fatalf("exit = %d (stdout %q, stderr %q)", r.exit, r.stdout, r.stderr)
			}
			if r.stderr != "" {
				t.Errorf("stderr = %q, want empty", r.stderr)
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
