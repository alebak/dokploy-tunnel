package companion

import (
	"errors"
	"flag"
	"io"
	"testing"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// wantConfig is a Config with its URL as a string.
type wantConfig struct {
	Listen, DokployURL, ServerID string
	Version                      bool
}

func TestParseConfig_Valid(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want wantConfig
	}{
		{
			"flags, defaults for the rest",
			[]string{"--dokploy-url", "http://dokploy:3000/"},
			nil,
			wantConfig{Listen: ":8080", DokployURL: "http://dokploy:3000"},
		},
		{
			"environment",
			nil,
			map[string]string{
				EnvListen:     "127.0.0.1:9000",
				EnvDokployURL: "https://dokploy.example.com",
				EnvServerID:   "srv_edge",
			},
			wantConfig{Listen: "127.0.0.1:9000", DokployURL: "https://dokploy.example.com", ServerID: "srv_edge"},
		},
		{
			"flags win over the environment",
			[]string{"--listen", ":7000", "--server-id", "srv_b"},
			map[string]string{EnvListen: ":9000", EnvDokployURL: "http://dokploy:3000", EnvServerID: "srv_a"},
			wantConfig{Listen: ":7000", DokployURL: "http://dokploy:3000", ServerID: "srv_b"},
		},
		{
			"local is the Dokploy server itself",
			[]string{"--dokploy-url", "http://dokploy:3000", "--server-id", "local"},
			nil,
			wantConfig{Listen: ":8080", DokployURL: "http://dokploy:3000"},
		},
		{
			"version",
			[]string{"--version"},
			nil,
			wantConfig{Listen: ":8080", Version: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseConfig(tt.args, env(tt.env), io.Discard)
			if err != nil {
				t.Fatalf("ParseConfig: %v", err)
			}
			gotURL := ""
			if got.DokployURL != nil {
				gotURL = got.DokployURL.String()
			}
			if got.Listen != tt.want.Listen || gotURL != tt.want.DokployURL || got.ServerID != tt.want.ServerID || got.Version != tt.want.Version {
				t.Errorf("ParseConfig = %+v (URL %q), want %+v", got, gotURL, tt.want)
			}
		})
	}
}

func TestParseConfig_Invalid(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
	}{
		{"missing Dokploy URL", nil, nil},
		{"malformed Dokploy URL", []string{"--dokploy-url", "dokploy:3000"}, nil},
		{"Dokploy URL with credentials", nil, map[string]string{EnvDokployURL: "https://u:p@dokploy.example.com"}},
		{"malformed server ID", []string{"--dokploy-url", "http://dokploy:3000", "--server-id", "srv edge"}, nil},
		{"unknown flag", []string{"--dokploy-url", "http://dokploy:3000", "--api-key", "x"}, nil},
		{"positional argument", []string{"--dokploy-url", "http://dokploy:3000", "serve"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := ParseConfig(tt.args, env(tt.env), io.Discard); err == nil {
				t.Fatalf("ParseConfig = %+v, want an error", got)
			}
		})
	}
}

func TestParseConfig_Help(t *testing.T) {
	if _, err := ParseConfig([]string{"--help"}, env(nil), io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("ParseConfig(--help) = %v, want flag.ErrHelp", err)
	}
}
