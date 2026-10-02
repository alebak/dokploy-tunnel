package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
	"github.com/alebak/dokploy-tunnel/internal/config"
	"github.com/alebak/dokploy-tunnel/internal/dokploy"
	"github.com/alebak/dokploy-tunnel/internal/keyring"
)

// fakeAPI answers Organization with org or err and records how it was built.
type fakeAPI struct {
	org   dokploy.Organization
	err   error
	calls int
	base  string
	key   string
}

func (f *fakeAPI) Organization(context.Context) (dokploy.Organization, error) {
	f.calls++
	return f.org, f.err
}

// contextHarness is an App wired to a temp config file, an in-memory keyring
// and a fake Dokploy API.
type contextHarness struct {
	t          *testing.T
	configPath string
	keyring    *keyring.Memory
	api        *fakeAPI
	env        map[string]string
	secret     string
}

func newContextHarness(t *testing.T) *contextHarness {
	return &contextHarness{
		t:          t,
		configPath: filepath.Join(t.TempDir(), "doktunnel", "config.json"),
		keyring:    keyring.NewMemory(),
		api:        &fakeAPI{org: dokploy.Organization{ID: "org1", Name: "Acme"}},
		env:        map[string]string{},
	}
}

func (h *contextHarness) run(stdin string, terminal bool, args ...string) result {
	h.t.Helper()
	var stdout, stderr bytes.Buffer
	app := &App{
		Root:            NewRoot(),
		Stdin:           strings.NewReader(stdin),
		Stdout:          &stdout,
		Stderr:          &stderr,
		StdinIsTerminal: terminal,
		ConfigPath:      h.configPath,
		Keyring:         h.keyring,
		NewAPI: func(base *url.URL, apiKey string) dokploy.API {
			h.api.base, h.api.key = base.String(), apiKey
			return h.api
		},
		ReadSecret: func() (string, error) { return h.secret, nil },
		Getenv:     func(k string) string { return h.env[k] },
	}
	exit := app.Run(args)
	return result{exit: exit, stdout: stdout.String(), stderr: stderr.String()}
}

func (h *contextHarness) mustRun(stdin string, args ...string) result {
	h.t.Helper()
	r := h.run(stdin, false, args...)
	if r.exit != 0 {
		h.t.Fatalf("%q: exit = %d (stdout %q, stderr %q)", args, r.exit, r.stdout, r.stderr)
	}
	return r
}

func (h *contextHarness) config() *config.Config {
	h.t.Helper()
	c, err := config.Load(h.configPath)
	if err != nil {
		h.t.Fatalf("loading config: %v", err)
	}
	return c
}

func (h *contextHarness) add(name, rawURL string) {
	h.t.Helper()
	h.env["DOKTUNNEL_API_KEY"] = "key-" + name
	h.mustRun("", "context", "add", "--url", rawURL, "--name", name, "--json")
	delete(h.env, "DOKTUNNEL_API_KEY")
}

func decodeJSON[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("stdout is not the expected JSON: %v (%q)", err, s)
	}
	return v
}

func TestContextAdd_StoresKeyOnlyInKeyring(t *testing.T) {
	h := newContextHarness(t)
	h.env["DOKTUNNEL_API_KEY"] = "s3cret-key"
	r := h.mustRun("", "context", "add", "--url", "https://panel.example.com/", "--name", "prod", "--json")

	got := decodeJSON[contextJSON](t, r.stdout)
	want := contextJSON{Name: "prod", URL: "https://panel.example.com", OrganizationID: "org1", OrganizationName: "Acme", Current: true}
	if got.Name != want.Name || got.URL != want.URL || got.OrganizationID != want.OrganizationID ||
		got.OrganizationName != want.OrganizationName || got.Current != want.Current || len(got.Warnings) != 0 {
		t.Errorf("output = %+v, want %+v", got, want)
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q, want empty in JSON mode", r.stderr)
	}
	if h.api.key != "s3cret-key" || h.api.base != "https://panel.example.com" {
		t.Errorf("API built with base %q key %q", h.api.base, h.api.key)
	}
	if secrets := h.keyring.Secrets(); secrets["prod"] != "s3cret-key" || len(secrets) != 1 {
		t.Errorf("keyring = %v, want only prod", secrets)
	}
	b, err := os.ReadFile(h.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "s3cret-key") {
		t.Errorf("config file contains the API key:\n%s", b)
	}
	if c := h.config(); c.Current != "prod" || len(c.Contexts) != 1 {
		t.Errorf("config = %+v, want prod as the only and current context", c)
	}
}

func TestContextAdd_KeySources(t *testing.T) {
	tests := []struct {
		name     string
		stdin    string
		terminal bool
		envKey   string
		secret   string
		args     []string
		wantKey  string
	}{
		{"stdin flag", "from-stdin\n", false, "", "", []string{"--url", "https://p", "--name", "x", "--api-key-stdin"}, "from-stdin"},
		{"stdin without newline", "from-stdin", false, "", "", []string{"--url", "https://p", "--name", "x", "--api-key-stdin"}, "from-stdin"},
		{"stdin wins over env", "from-stdin\n", false, "from-env", "", []string{"--url", "https://p", "--name", "x", "--api-key-stdin"}, "from-stdin"},
		{"env", "", false, "from-env", "", []string{"--url", "https://p", "--name", "x"}, "from-env"},
		{"hidden prompt", "https://p\nx\n", true, "", "typed", nil, "typed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newContextHarness(t)
			h.env["DOKTUNNEL_API_KEY"] = tt.envKey
			h.secret = tt.secret
			r := h.run(tt.stdin, tt.terminal, append([]string{"context", "add"}, tt.args...)...)
			if r.exit != 0 {
				t.Fatalf("exit = %d (stdout %q, stderr %q)", r.exit, r.stdout, r.stderr)
			}
			if h.api.key != tt.wantKey {
				t.Errorf("API key = %q, want %q", h.api.key, tt.wantKey)
			}
			if got := h.keyring.Secrets()["x"]; got != tt.wantKey {
				t.Errorf("stored key = %q, want %q", got, tt.wantKey)
			}
			if strings.Contains(r.stdout+r.stderr, tt.wantKey) {
				t.Errorf("output echoes the API key: stdout %q stderr %q", r.stdout, r.stderr)
			}
		})
	}
}

func TestContextAdd_MissingInputNamesFlag(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantFlag string
	}{
		{"url", []string{"--name", "x"}, "--url"},
		{"name", []string{"--url", "https://p"}, "--name"},
		{"key", []string{"--url", "https://p", "--name", "x"}, "--api-key-stdin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newContextHarness(t)
			r := h.run("", true, append([]string{"context", "add", "--no-input", "--json"}, tt.args...)...)
			e := decodeError(t, r.stdout)
			if e.Code != clierr.MissingInput || !strings.Contains(e.Message+e.Hint, tt.wantFlag) {
				t.Errorf("error = %+v, want %s naming %s", e, clierr.MissingInput, tt.wantFlag)
			}
			if h.api.calls != 0 || len(h.keyring.Secrets()) != 0 {
				t.Errorf("API called %d times, keyring %v; want nothing done", h.api.calls, h.keyring.Secrets())
			}
		})
	}
}

func TestContextAdd_RejectsKeyAsFlag(t *testing.T) {
	h := newContextHarness(t)
	r := h.run("", false, "context", "add", "--url", "https://p", "--name", "x", "--api-key", "k", "--json")
	if e := decodeError(t, r.stdout); e.Code != clierr.InvalidArgument {
		t.Errorf("code = %q, want %q", e.Code, clierr.InvalidArgument)
	}
}

func TestContextAdd_WarnsOnPlainHTTP(t *testing.T) {
	h := newContextHarness(t)
	h.env["DOKTUNNEL_API_KEY"] = "k"
	r := h.mustRun("", "context", "add", "--url", "http://192.168.1.20:3000", "--name", "lan")
	if !strings.Contains(r.stderr, "warning:") || !strings.Contains(r.stderr, "http://") {
		t.Errorf("stderr = %q, want a plain-HTTP warning", r.stderr)
	}

	r = h.mustRun("", "context", "add", "--url", "http://192.168.1.20:3000", "--name", "lan2", "--json")
	if r.stderr != "" {
		t.Errorf("stderr = %q, want empty in JSON mode", r.stderr)
	}
	if got := decodeJSON[contextJSON](t, r.stdout); len(got.Warnings) != 1 {
		t.Errorf("warnings = %q, want one plain-HTTP warning", got.Warnings)
	}
}

func TestContextAdd_Failures(t *testing.T) {
	tests := []struct {
		name     string
		apiErr   error
		existing bool
		args     []string
		want     clierr.Code
	}{
		{"rejected key", dokploy.ErrUnauthorized, false, nil, clierr.PermissionDenied},
		{"unreachable panel", dokploy.ErrUnreachable, false, nil, clierr.Unreachable},
		{"not a Dokploy panel", dokploy.ErrUnexpectedResponse, false, nil, clierr.Unreachable},
		{"duplicate name", nil, true, nil, clierr.InvalidArgument},
		{"invalid name", nil, false, []string{"--name", "bad name"}, clierr.InvalidArgument},
		{"invalid url", nil, false, []string{"--url", "panel.example.com"}, clierr.InvalidArgument},
		{"unexpected argument", nil, false, []string{"extra"}, clierr.InvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newContextHarness(t)
			if tt.existing {
				h.add("prod", "https://other")
			}
			calls := h.api.calls
			h.api.err = tt.apiErr
			h.env["DOKTUNNEL_API_KEY"] = "k"
			args := append([]string{"context", "add", "--json", "--url", "https://p", "--name", "prod"}, tt.args...)
			r := h.run("", false, args...)
			if e := decodeError(t, r.stdout); e.Code != tt.want {
				t.Fatalf("error = %+v, want code %q", e, tt.want)
			}
			if r.exit != tt.want.ExitCode() {
				t.Errorf("exit = %d, want %d", r.exit, tt.want.ExitCode())
			}
			if tt.apiErr == nil && h.api.calls != calls {
				t.Errorf("API called on a request that was invalid up front")
			}
			wantSecrets := 0
			if tt.existing {
				wantSecrets = 1
			}
			if n := len(h.keyring.Secrets()); n != wantSecrets {
				t.Errorf("keyring holds %d secrets, want %d", n, wantSecrets)
			}
		})
	}
}

func TestContextAdd_KeyringFailureWritesNothing(t *testing.T) {
	h := newContextHarness(t)
	h.keyring.Fail = errors.New("no secret service")
	h.env["DOKTUNNEL_API_KEY"] = "k"
	r := h.run("", false, "context", "add", "--url", "https://p", "--name", "prod", "--json")
	if r.exit == 0 {
		t.Fatal("exit = 0, want an error when the keyring is unavailable")
	}
	if _, err := os.Stat(h.configPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("config file written although the key could not be stored: %v", err)
	}
}

func TestContextAdd_KeepsExistingCurrent(t *testing.T) {
	h := newContextHarness(t)
	h.add("prod", "https://a")
	h.add("staging", "https://b")
	if c := h.config(); c.Current != "prod" {
		t.Errorf("current = %q, want the first context to stay current", c.Current)
	}
}

type listJSON struct {
	CurrentContext string        `json:"current_context"`
	Contexts       []contextJSON `json:"contexts"`
}

func TestContextList(t *testing.T) {
	h := newContextHarness(t)
	r := h.mustRun("", "context", "list", "--json")
	if strings.TrimSpace(r.stdout) != `{"current_context":"","contexts":[]}` {
		t.Errorf("empty list = %s", r.stdout)
	}
	if _, err := os.Stat(h.configPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("listing created the config file: %v", err)
	}

	h.add("prod", "https://a")
	h.add("staging", "https://b")
	h.mustRun("", "context", "use", "staging")

	got := decodeJSON[listJSON](t, h.mustRun("", "context", "list", "--json").stdout)
	if got.CurrentContext != "staging" || len(got.Contexts) != 2 ||
		got.Contexts[0].Name != "prod" || got.Contexts[0].Current ||
		got.Contexts[1].Name != "staging" || !got.Contexts[1].Current {
		t.Errorf("list = %+v, want prod and staging with staging current", got)
	}

	human := h.mustRun("", "context", "list").stdout
	for _, want := range []string{"CURRENT", "NAME", "ORGANIZATION", "*", "staging", "https://b", "Acme"} {
		if !strings.Contains(human, want) {
			t.Errorf("human list does not contain %q:\n%s", want, human)
		}
	}
}

func TestContextUse(t *testing.T) {
	h := newContextHarness(t)
	h.add("prod", "https://a")
	h.add("staging", "https://b")

	got := decodeJSON[contextJSON](t, h.mustRun("", "context", "use", "staging", "--json").stdout)
	if got.Name != "staging" || !got.Current {
		t.Errorf("use output = %+v, want staging as current", got)
	}
	if c := h.config(); c.Current != "staging" {
		t.Errorf("current = %q, want staging", c.Current)
	}

	for _, args := range [][]string{{"nope"}, {}, {"prod", "staging"}} {
		r := h.run("", false, append([]string{"context", "use", "--json"}, args...)...)
		if e := decodeError(t, r.stdout); e.Code != clierr.InvalidArgument {
			t.Errorf("use %q: code = %q, want %q", args, e.Code, clierr.InvalidArgument)
		}
	}
}

func TestContextRemove(t *testing.T) {
	h := newContextHarness(t)
	h.add("prod", "https://a")
	h.add("staging", "https://b")

	r := h.mustRun("", "context", "remove", "prod", "--json")
	if strings.TrimSpace(r.stdout) != `{"name":"prod","removed":true}` {
		t.Errorf("remove output = %s", r.stdout)
	}
	c := h.config()
	if c.Current != "" || len(c.Contexts) != 1 || c.Contexts[0].Name != "staging" {
		t.Errorf("config = %+v, want only staging and no current context", c)
	}
	if secrets := h.keyring.Secrets(); len(secrets) != 1 || secrets["staging"] == "" {
		t.Errorf("keyring = %v, want only staging", secrets)
	}

	if e := decodeError(t, h.run("", false, "context", "remove", "prod", "--json").stdout); e.Code != clierr.InvalidArgument {
		t.Errorf("removing an unknown context: code = %q, want %q", e.Code, clierr.InvalidArgument)
	}
}

func TestContextRemove_MissingSecretStillRemoves(t *testing.T) {
	h := newContextHarness(t)
	h.add("prod", "https://a")
	if err := h.keyring.Delete("prod"); err != nil {
		t.Fatal(err)
	}
	h.mustRun("", "context", "remove", "prod")
	if c := h.config(); len(c.Contexts) != 0 {
		t.Errorf("contexts = %+v, want none", c.Contexts)
	}
}

func TestContextRemove_KeyringFailureKeepsContext(t *testing.T) {
	h := newContextHarness(t)
	h.add("prod", "https://a")
	h.keyring.Fail = errors.New("keyring locked")
	if r := h.run("", false, "context", "remove", "prod", "--json"); r.exit == 0 {
		t.Fatal("exit = 0, want an error when the secret cannot be deleted")
	}
	if c := h.config(); len(c.Contexts) != 1 {
		t.Errorf("context removed although its secret was kept: %+v", c)
	}
}

func TestEnv_ResolveContext(t *testing.T) {
	tests := []struct {
		name     string
		current  string
		override string
		dropKey  bool
		want     string
		wantCode clierr.Code
		wantHint string
	}{
		{"current", "prod", "", false, "prod", "", ""},
		{"override", "prod", "staging", false, "staging", "", ""},
		{"no current", "", "", false, "", clierr.MissingInput, "--context"},
		{"unknown", "prod", "nope", false, "", clierr.InvalidArgument, "context list"},
		{"key missing from keyring", "prod", "", true, "", clierr.PermissionDenied, "context add"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newContextHarness(t)
			h.add("prod", "https://a")
			h.add("staging", "https://b")
			c := h.config()
			c.Current = tt.current
			if err := c.Save(h.configPath); err != nil {
				t.Fatal(err)
			}
			if tt.dropKey {
				_ = h.keyring.Delete("prod")
			}
			env := &Env{Globals: Globals{Context: tt.override}, ConfigPath: h.configPath, Keyring: h.keyring}
			ctx, key, err := env.ResolveContext()
			if tt.wantCode != "" {
				e := clierr.From(err)
				if e == nil || e.Code != tt.wantCode || !strings.Contains(e.Message+" "+e.Hint, tt.wantHint) {
					t.Fatalf("err = %+v, want code %q mentioning %q", e, tt.wantCode, tt.wantHint)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveContext: %v", err)
			}
			if ctx.Name != tt.want || key != "key-"+tt.want {
				t.Errorf("got %q with key %q, want %q with its key", ctx.Name, key, tt.want)
			}
		})
	}
}

func TestContextCommands_CorruptConfig(t *testing.T) {
	h := newContextHarness(t)
	if err := os.MkdirAll(filepath.Dir(h.configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.configPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := decodeError(t, h.run("", false, "context", "list", "--json").stdout)
	if e.Code != clierr.Internal || !strings.Contains(e.Hint, h.configPath) {
		t.Errorf("error = %+v, want internal with a hint naming the config file", e)
	}
}
