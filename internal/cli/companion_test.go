package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
	"github.com/alebak/dokploy-tunnel/internal/dokploy"
	"github.com/alebak/dokploy-tunnel/internal/tunnel"
)

func TestDefaultCompanionURL(t *testing.T) {
	tests := []struct {
		name  string
		panel string
		want  string
	}{
		{"bare host", "https://dokploy.example.com", "https://dokploy.example.com/doktunnel"},
		{"trailing slash", "https://dokploy.example.com/", "https://dokploy.example.com/doktunnel"},
		{"port", "http://192.168.1.20:3000", "http://192.168.1.20:3000/doktunnel"},
		{"path", "https://example.com/dokploy/", "https://example.com/dokploy/doktunnel"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, err := dokploy.ParseBaseURL(tt.panel)
			if err != nil {
				t.Fatal(err)
			}
			if got := defaultCompanionURL(base).String(); got != tt.want {
				t.Errorf("defaultCompanionURL(%q) = %q, want %q", tt.panel, got, tt.want)
			}
			if base.String() != strings.TrimRight(tt.panel, "/") {
				t.Errorf("defaultCompanionURL modified the panel URL: %q", base)
			}
		})
	}
}

// companionServer serves handler under the base path /doktunnel, like a
// companion behind Traefik, and records the requests it receives.
func companionServer(t *testing.T, handler http.HandlerFunc) (*url.URL, *[]*http.Request) {
	t.Helper()
	var reqs []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs = append(reqs, r)
		if r.URL.Path != "/doktunnel"+tunnel.HealthPath {
			http.NotFound(w, r)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL + "/doktunnel")
	if err != nil {
		t.Fatal(err)
	}
	return u, &reqs
}

func TestProbeCompanion(t *testing.T) {
	ok := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantErr string
	}{
		{"healthy", ok, ""},
		{"not found", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, "404"},
		{"draining", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"shutting_down"}`))
		}, "503"},
		{"other body", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`<html>Dokploy</html>`))
		}, "companion"},
		{"other status field", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"status":"starting"}`))
		}, "starting"},
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://elsewhere.example.com/healthz", http.StatusMovedPermanently)
		}, "elsewhere.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, reqs := companionServer(t, tt.handler)
			err := probeCompanion(context.Background(), u)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("probeCompanion = %v, want nil", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("probeCompanion = %v, want an error mentioning %q", err, tt.wantErr)
			}
			if len(*reqs) != 1 {
				t.Fatalf("companion received %d requests, want 1 (no redirects followed)", len(*reqs))
			}
			r := (*reqs)[0]
			if r.Method != http.MethodGet || r.URL.Path != "/doktunnel/healthz" {
				t.Errorf("request = %s %s, want GET /doktunnel/healthz", r.Method, r.URL.Path)
			}
			if r.Header.Get(tunnel.HeaderAPIKey) != "" {
				t.Errorf("probe sent an API key")
			}
		})
	}
}

func TestProbeCompanion_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	u, _ := url.Parse(srv.URL + "/doktunnel")
	srv.Close()
	if err := probeCompanion(context.Background(), u); err == nil {
		t.Fatal("probeCompanion = nil, want an error for a closed server")
	}
}

func TestContextAdd_CompanionURL(t *testing.T) {
	tests := []struct {
		name         string
		panel        string
		args         []string
		probeErr     error
		wantURL      string
		wantWarnings [][]string // substrings each warning must contain
	}{
		{
			name:    "convention, reachable",
			panel:   "https://panel.example.com/",
			wantURL: "https://panel.example.com/doktunnel",
		},
		{
			name:    "convention keeps a panel path",
			panel:   "https://example.com/dokploy",
			wantURL: "https://example.com/dokploy/doktunnel",
		},
		{
			name:         "convention, unreachable",
			panel:        "https://panel.example.com",
			probeErr:     errors.New("connection refused"),
			wantURL:      "https://panel.example.com/doktunnel",
			wantWarnings: [][]string{{"connection refused", "--companion-url", "set-companion"}},
		},
		{
			name:    "override",
			panel:   "https://panel.example.com",
			args:    []string{"--companion-url", "https://tunnel.example.com/"},
			wantURL: "https://tunnel.example.com",
		},
		{
			name:         "plain http override",
			panel:        "https://panel.example.com",
			args:         []string{"--companion-url", "http://10.0.0.5:8080"},
			wantURL:      "http://10.0.0.5:8080",
			wantWarnings: [][]string{{"companion URL uses plain http://"}},
		},
		{
			// The panel warning already covers the derived companion URL.
			name:         "plain http panel",
			panel:        "http://192.168.1.20:3000",
			wantURL:      "http://192.168.1.20:3000/doktunnel",
			wantWarnings: [][]string{{"panel URL uses plain http://"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newContextHarness(t)
			h.probeErr = tt.probeErr
			h.env["DOKTUNNEL_API_KEY"] = "k"
			args := append([]string{"context", "add", "--url", tt.panel, "--name", "prod", "--json"}, tt.args...)
			r := h.mustRun("", args...)

			got := decodeJSON[contextJSON](t, r.stdout)
			if got.CompanionURL != tt.wantURL {
				t.Errorf("output companion_url = %q, want %q", got.CompanionURL, tt.wantURL)
			}
			if stored, _ := h.config().Find("prod"); stored.CompanionURL != tt.wantURL {
				t.Errorf("stored companion URL = %q, want %q", stored.CompanionURL, tt.wantURL)
			}
			if len(h.probed) != 1 || h.probed[0] != tt.wantURL {
				t.Errorf("probed %q, want exactly %q", h.probed, tt.wantURL)
			}
			if len(got.Warnings) != len(tt.wantWarnings) {
				t.Fatalf("warnings = %q, want %d", got.Warnings, len(tt.wantWarnings))
			}
			for i, parts := range tt.wantWarnings {
				for _, part := range parts {
					if !strings.Contains(got.Warnings[i], part) {
						t.Errorf("warning %q does not mention %q", got.Warnings[i], part)
					}
				}
			}
		})
	}
}

func TestContextAdd_CompanionWarningOnStderr(t *testing.T) {
	h := newContextHarness(t)
	h.probeErr = errors.New("404 Not Found")
	h.env["DOKTUNNEL_API_KEY"] = "k"
	r := h.mustRun("", "context", "add", "--url", "https://p", "--name", "prod")
	if !strings.Contains(r.stderr, "warning:") || !strings.Contains(r.stderr, "--companion-url") {
		t.Errorf("stderr = %q, want a companion warning naming --companion-url", r.stderr)
	}
	if !strings.Contains(r.stdout, "Added context") {
		t.Errorf("stdout = %q, want the context added despite the warning", r.stdout)
	}
}

func TestContextAdd_ProbesAfterKeyValidation(t *testing.T) {
	h := newContextHarness(t)
	h.api.err = dokploy.ErrUnauthorized
	h.env["DOKTUNNEL_API_KEY"] = "k"
	r := h.run("", false, "context", "add", "--url", "https://p", "--name", "prod", "--json")
	if e := decodeError(t, r.stdout); e.Code != clierr.PermissionDenied {
		t.Fatalf("error = %+v, want permission_denied", e)
	}
	if len(h.probed) != 0 {
		t.Errorf("probed %q before the API key was accepted", h.probed)
	}
}

func TestContextAdd_InvalidCompanionURL(t *testing.T) {
	for _, raw := range []string{"tunnel.example.com", "ftp://x", "https://u:p@x", "https://x/?q=1"} {
		t.Run(raw, func(t *testing.T) {
			h := newContextHarness(t)
			h.env["DOKTUNNEL_API_KEY"] = "k"
			r := h.run("", false, "context", "add", "--url", "https://p", "--name", "prod", "--companion-url", raw, "--json")
			e := decodeError(t, r.stdout)
			if e.Code != clierr.InvalidArgument || !strings.Contains(e.Hint, "--companion-url") {
				t.Errorf("error = %+v, want invalid_argument with a --companion-url hint", e)
			}
			if h.api.calls != 0 || len(h.probed) != 0 || len(h.keyring.Secrets()) != 0 {
				t.Errorf("work done for an invalid companion URL: API %d, probed %q", h.api.calls, h.probed)
			}
		})
	}
}

// TestContextAdd_RealProbe runs the real HTTP probe against a fake companion
// served under the panel's /doktunnel path.
func TestContextAdd_RealProbe(t *testing.T) {
	tests := []struct {
		name        string
		handler     http.HandlerFunc
		wantWarning bool
	}{
		{"healthy", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"status":"ok"}`)) }, false},
		{"panel without companion", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			companion, reqs := companionServer(t, tt.handler)
			panel := strings.TrimSuffix(companion.String(), "/doktunnel")
			h := newContextHarness(t)
			h.probe = nil
			h.env["DOKTUNNEL_API_KEY"] = "s3cret-key"
			r := h.mustRun("", "context", "add", "--url", panel, "--name", "lan", "--json")

			got := decodeJSON[contextJSON](t, r.stdout)
			if got.CompanionURL != companion.String() {
				t.Errorf("companion_url = %q, want %q", got.CompanionURL, companion)
			}
			// The test server is plain http, so the panel warning is always there.
			wantWarnings := 1
			if tt.wantWarning {
				wantWarnings = 2
			}
			if len(got.Warnings) != wantWarnings {
				t.Errorf("warnings = %q, want %d", got.Warnings, wantWarnings)
			}
			for _, r := range *reqs {
				if r.Header.Get(tunnel.HeaderAPIKey) != "" || strings.Contains(r.URL.String(), "s3cret-key") {
					t.Errorf("probe leaked the API key: %s %v", r.URL, r.Header)
				}
			}
		})
	}
}

func TestContextSetCompanion(t *testing.T) {
	h := newContextHarness(t)
	h.add("prod", "https://a")
	h.add("staging", "https://b")
	h.probed = nil

	r := h.mustRun("", "context", "set-companion", "staging", "https://tunnel.example.com/", "--json")
	got := decodeJSON[contextJSON](t, r.stdout)
	if got.Name != "staging" || got.CompanionURL != "https://tunnel.example.com" || got.Current || len(got.Warnings) != 0 {
		t.Errorf("output = %+v, want staging with the new companion URL and no warnings", got)
	}
	if len(h.probed) != 1 || h.probed[0] != "https://tunnel.example.com" {
		t.Errorf("probed %q, want the new URL once", h.probed)
	}
	c := h.config()
	if s, _ := c.Find("staging"); s.CompanionURL != "https://tunnel.example.com" {
		t.Errorf("stored companion URL = %q", s.CompanionURL)
	}
	if p, _ := c.Find("prod"); p.CompanionURL != "https://a/doktunnel" {
		t.Errorf("other context changed: %q", p.CompanionURL)
	}
	if len(h.keyring.Secrets()) != 2 {
		t.Errorf("keyring = %v, want both keys untouched", h.keyring.Secrets())
	}

	// An unreachable companion is stored anyway, with a warning.
	h.probeErr = errors.New("connection refused")
	r = h.mustRun("", "context", "set-companion", "prod", "http://10.0.0.5:8080")
	if !strings.Contains(r.stderr, "connection refused") || !strings.Contains(r.stderr, "plain http://") {
		t.Errorf("stderr = %q, want the probe and plain-HTTP warnings", r.stderr)
	}
	if p, _ := h.config().Find("prod"); p.CompanionURL != "http://10.0.0.5:8080" {
		t.Errorf("stored companion URL = %q, want it stored despite the warning", p.CompanionURL)
	}
}

func TestContextSetCompanion_Errors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"unknown context", []string{"nope", "https://x"}},
		{"missing URL", []string{"prod"}},
		{"extra argument", []string{"prod", "https://x", "extra"}},
		{"invalid URL", []string{"prod", "x.example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newContextHarness(t)
			h.add("prod", "https://a")
			h.probed = nil
			before, err := os.ReadFile(h.configPath)
			if err != nil {
				t.Fatal(err)
			}
			r := h.run("", false, append([]string{"context", "set-companion", "--json"}, tt.args...)...)
			if e := decodeError(t, r.stdout); e.Code != clierr.InvalidArgument {
				t.Errorf("error = %+v, want invalid_argument", e)
			}
			after, _ := os.ReadFile(h.configPath)
			if string(after) != string(before) || len(h.probed) != 0 {
				t.Errorf("config changed or companion probed (%q) on an invalid request", h.probed)
			}
		})
	}
}

func TestContextList_ContextWithoutCompanionURL(t *testing.T) {
	h := newContextHarness(t)
	if err := os.MkdirAll(filepath.Dir(h.configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"version":1,"current_context":"prod","contexts":[{"name":"prod","url":"https://a","organization_id":"org1","organization_name":"Acme"}]}`
	if err := os.WriteFile(h.configPath, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	r := h.mustRun("", "context", "list", "--json")
	if !strings.Contains(r.stdout, `"companion_url":""`) {
		t.Errorf("JSON list = %s, want an empty companion_url", r.stdout)
	}
	human := h.mustRun("", "context", "list").stdout
	lines := strings.Split(strings.TrimSpace(human), "\n")
	if len(lines) != 2 || !strings.HasSuffix(strings.TrimSpace(lines[1]), "-") {
		t.Errorf("human list = %q, want '-' in the companion column", human)
	}
}
