package companion

import (
	"flag"
	"fmt"
	"io"
	"net/url"

	"github.com/alebak/dokploy-tunnel/internal/dokploy"
	"github.com/alebak/dokploy-tunnel/internal/tunnel"
)

// Environment variables that configure the companion; flags win over them.
const (
	EnvListen     = "DOKTUNNEL_COMPANION_LISTEN"
	EnvDokployURL = "DOKTUNNEL_COMPANION_DOKPLOY_URL"
	EnvServerID   = "DOKTUNNEL_COMPANION_SERVER_ID"
)

const defaultListen = ":8080"

// Config configures doktunnel-companion.
type Config struct {
	// Listen is the TCP address to serve on, such as ":8080".
	Listen string
	// DokployURL is the Dokploy panel as the companion reaches it.
	DokployURL *url.URL
	// ServerID is the Dokploy server the companion runs on, or empty for
	// the Dokploy server itself.
	ServerID string
	// Version asks for the build version instead of serving.
	Version bool
}

// ParseConfig reads the configuration from the command-line arguments args
// and from the environment through getenv; flags win. Usage is written to
// usage. It returns flag.ErrHelp for --help.
func ParseConfig(args []string, getenv func(string) string, usage io.Writer) (Config, error) {
	fs := flag.NewFlagSet("doktunnel-companion", flag.ContinueOnError)
	fs.SetOutput(usage)
	listen := fs.String("listen", orDefault(getenv(EnvListen), defaultListen),
		"TCP `address` to serve on (env "+EnvListen+")")
	rawURL := fs.String("dokploy-url", getenv(EnvDokployURL),
		"`URL` of the Dokploy panel, as reachable from the companion (env "+EnvDokployURL+")")
	serverID := fs.String("server-id", getenv(EnvServerID),
		"`ID` of the Dokploy server the companion runs on; empty or \""+tunnel.LocalServer+"\" for the Dokploy server itself (env "+EnvServerID+")")
	version := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	cfg := Config{Listen: *listen, Version: *version}
	if cfg.Version {
		return cfg, nil
	}
	if *rawURL == "" {
		return Config{}, fmt.Errorf("missing --dokploy-url (or %s)", EnvDokployURL)
	}
	u, err := dokploy.ParseBaseURL(*rawURL)
	if err != nil {
		return Config{}, fmt.Errorf("--dokploy-url: %w", err)
	}
	cfg.DokployURL = u
	switch id := *serverID; {
	case id == "" || id == tunnel.LocalServer:
	case tunnel.ValidID(id):
		cfg.ServerID = id
	default:
		return Config{}, fmt.Errorf("--server-id: malformed Dokploy server ID %q", id)
	}
	return cfg, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
