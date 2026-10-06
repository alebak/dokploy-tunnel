package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sample() Context {
	return Context{Name: "prod", URL: "https://panel.example.com", OrganizationID: "org1", OrganizationName: "Acme"}
}

func TestLoad_MissingFileIsEmpty(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "doktunnel", "config.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Version != SchemaVersion || c.Current != "" || len(c.Contexts) != 0 {
		t.Errorf("config = %+v, want empty with version %d", c, SchemaVersion)
	}
}

func TestSave_RoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doktunnel", "config.json")
	c := New()
	if err := c.Add(sample()); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := c.Use("prod"); err != nil {
		t.Fatalf("Use: %v", err)
	}
	if err := c.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Current != "prod" || len(got.Contexts) != 1 || got.Contexts[0] != sample() {
		t.Errorf("loaded %+v, want the saved context as current", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no Unix permission bits; Go reports 0666 there.
	if perm := info.Mode().Perm(); perm&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("config mode = %v, want no group or other access", perm)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("config dir has %d entries, want only config.json (no temp files left)", len(entries))
	}
}

func TestSave_NeverStoresSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	c := New()
	if err := c.Add(sample()); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"key", "token", "secret", "password"} {
		if strings.Contains(strings.ToLower(string(b)), field) {
			t.Errorf("config file mentions %q:\n%s", field, b)
		}
	}
}

func TestLoad_RejectsBadFiles(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    error
	}{
		{"not JSON", "{", ErrCorrupt},
		{"future version", `{"version":99,"contexts":[]}`, ErrUnsupportedVersion},
		{"duplicate names", `{"version":1,"contexts":[{"name":"a","url":"https://x"},{"name":"a","url":"https://y"}]}`, ErrCorrupt},
		{"invalid name", `{"version":1,"contexts":[{"name":"bad name","url":"https://x"}]}`, ErrCorrupt},
		{"missing url", `{"version":1,"contexts":[{"name":"a"}]}`, ErrCorrupt},
		{"dangling current", `{"version":1,"current_context":"gone","contexts":[]}`, ErrCorrupt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); !errors.Is(err, tt.want) {
				t.Errorf("Load: err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestConfig_AddUseRemove(t *testing.T) {
	c := New()
	if err := c.Add(sample()); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := c.Add(sample()); !errors.Is(err, ErrExists) {
		t.Errorf("Add duplicate: err = %v, want ErrExists", err)
	}
	bad := sample()
	bad.Name = "-flag"
	if err := c.Add(bad); !errors.Is(err, ErrInvalidName) {
		t.Errorf("Add invalid name: err = %v, want ErrInvalidName", err)
	}
	if err := c.Use("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Use unknown: err = %v, want ErrNotFound", err)
	}
	if err := c.Use("prod"); err != nil || c.Current != "prod" {
		t.Fatalf("Use: err = %v, current = %q", err, c.Current)
	}
	if err := c.Remove("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Remove unknown: err = %v, want ErrNotFound", err)
	}
	if err := c.Remove("prod"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if c.Current != "" || len(c.Contexts) != 0 {
		t.Errorf("after removing the current context: %+v, want empty with no current", c)
	}
}

func TestConfig_Resolve(t *testing.T) {
	staging := sample()
	staging.Name = "staging"
	tests := []struct {
		name     string
		current  string
		override string
		want     string
		wantErr  error
	}{
		{"current context", "prod", "", "prod", nil},
		{"override wins", "prod", "staging", "staging", nil},
		{"override without current", "", "staging", "staging", nil},
		{"unknown override", "prod", "nope", "", ErrNotFound},
		{"no current", "", "", "", ErrNoCurrent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New()
			for _, ctx := range []Context{sample(), staging} {
				if err := c.Add(ctx); err != nil {
					t.Fatal(err)
				}
			}
			c.Current = tt.current
			got, err := c.Resolve(tt.override)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Resolve: err = %v, want %v", err, tt.wantErr)
			}
			if got.Name != tt.want {
				t.Errorf("Resolve = %q, want %q", got.Name, tt.want)
			}
		})
	}
}

func TestValidateName(t *testing.T) {
	for _, name := range []string{"prod", "my-org.staging", "a", "Org_2", strings.Repeat("a", 64)} {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{"", "-x", ".x", "a b", "a/b", "ñ", strings.Repeat("a", 65)} {
		if err := ValidateName(name); !errors.Is(err, ErrInvalidName) {
			t.Errorf("ValidateName(%q) = %v, want ErrInvalidName", name, err)
		}
	}
}

func TestDefaultPath_UsesUserConfigDir(t *testing.T) {
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Skipf("no user config dir: %v", err)
	}
	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if want := filepath.Join(dir, "doktunnel", "config.json"); got != want {
		t.Errorf("DefaultPath = %q, want %q", got, want)
	}
}

func TestLoad_FileWithoutCompanionURL(t *testing.T) {
	// A file written before contexts stored a companion URL still loads,
	// with the field empty.
	path := filepath.Join(t.TempDir(), "config.json")
	old := `{"version":1,"current_context":"prod","contexts":[{"name":"prod","url":"https://panel.example.com","organization_id":"org1","organization_name":"Acme"}]}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Contexts) != 1 || c.Contexts[0] != sample() || c.Contexts[0].CompanionURL != "" {
		t.Errorf("loaded %+v, want the context with no companion URL", c.Contexts)
	}
}

func TestSave_RoundTripCompanionURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	ctx := sample()
	ctx.CompanionURL = "https://panel.example.com/doktunnel"
	c := New()
	if err := c.Add(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"companion_url": "https://panel.example.com/doktunnel"`) {
		t.Errorf("config file does not store companion_url:\n%s", b)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Contexts[0] != ctx {
		t.Errorf("loaded %+v, want %+v", got.Contexts[0], ctx)
	}
}

func TestConfig_SetCompanionURL(t *testing.T) {
	c := New()
	if err := c.Add(sample()); err != nil {
		t.Fatal(err)
	}
	if err := c.SetCompanionURL("nope", "https://x/doktunnel"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetCompanionURL unknown: err = %v, want ErrNotFound", err)
	}
	if err := c.SetCompanionURL("prod", "https://x/doktunnel"); err != nil {
		t.Fatalf("SetCompanionURL: %v", err)
	}
	if got, _ := c.Find("prod"); got.CompanionURL != "https://x/doktunnel" {
		t.Errorf("companion URL = %q, want https://x/doktunnel", got.CompanionURL)
	}
}
