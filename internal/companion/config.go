package companion

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/docker"
	"github.com/alebak/dokploy-tunnel/internal/dokploy"
	"github.com/alebak/dokploy-tunnel/internal/repeater"
	"github.com/alebak/dokploy-tunnel/internal/tunnel"
)

// Environment variables that configure the companion; flags win over them.
const (
	EnvListen     = "DOKTUNNEL_COMPANION_LISTEN"
	EnvDokployURL = "DOKTUNNEL_COMPANION_DOKPLOY_URL"
	EnvServerID   = "DOKTUNNEL_COMPANION_SERVER_ID"

	EnvBridge        = "DOKTUNNEL_COMPANION_BRIDGE"
	EnvRepeaterImage = "DOKTUNNEL_COMPANION_REPEATER_IMAGE"
	EnvRepeaterGrace = "DOKTUNNEL_COMPANION_REPEATER_GRACE"
	EnvReaperTTL     = "DOKTUNNEL_COMPANION_REAPER_TTL"
	// EnvDockerHost is Docker's own variable, so the companion finds the
	// daemon the way the docker CLI does.
	EnvDockerHost = "DOCKER_HOST"
)

// Bridges the companion can forward with.
const (
	// BridgeDocker reaches targets through repeater containers.
	BridgeDocker = "docker"
	// BridgeNone forwards nothing: authorized tunnels are refused with
	// target_unreachable.
	BridgeNone = "none"
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

	// Bridge is BridgeDocker or BridgeNone.
	Bridge string
	// DockerHost is the Docker daemon's endpoint, such as
	// "unix:///var/run/docker.sock".
	DockerHost string
	// RepeaterImage is the image of the repeater containers.
	RepeaterImage string
	// RepeaterGrace is how long a repeater outlives its last tunnel.
	RepeaterGrace time.Duration
	// ReaperTTL is how old an orphaned repeater must be to be removed.
	ReaperTTL time.Duration
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
	bridge := fs.String("bridge", orDefault(getenv(EnvBridge), BridgeDocker),
		"how to reach targets: \""+BridgeDocker+"\" (repeater containers) or \""+BridgeNone+"\" (refuse every tunnel) (env "+EnvBridge+")")
	dockerHost := fs.String("docker-host", orDefault(getenv(EnvDockerHost), docker.DefaultHost),
		"Docker daemon `endpoint`: unix:///path or tcp://host:port (env "+EnvDockerHost+")")
	image := fs.String("repeater-image", orDefault(getenv(EnvRepeaterImage), repeater.DefaultImage),
		"`image` of the repeater containers; it must provide socat and sleep (env "+EnvRepeaterImage+")")
	grace := fs.String("repeater-grace", orDefault(getenv(EnvRepeaterGrace), repeater.DefaultGrace.String()),
		"`duration` a repeater outlives its last tunnel (env "+EnvRepeaterGrace+")")
	ttl := fs.String("reaper-ttl", orDefault(getenv(EnvReaperTTL), repeater.DefaultTTL.String()),
		"`duration` after which an orphaned repeater is removed (env "+EnvReaperTTL+")")
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

	switch *bridge {
	case BridgeDocker, BridgeNone:
		cfg.Bridge = *bridge
	default:
		return Config{}, fmt.Errorf("--bridge: unknown bridge %q", *bridge)
	}
	if _, err := docker.New(*dockerHost); err != nil {
		return Config{}, fmt.Errorf("--docker-host: %w", err)
	}
	cfg.DockerHost, cfg.RepeaterImage = *dockerHost, *image
	if cfg.RepeaterGrace, err = positiveDuration("--repeater-grace", *grace); err != nil {
		return Config{}, err
	}
	if cfg.ReaperTTL, err = positiveDuration("--reaper-ttl", *ttl); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// positiveDuration parses the value v of the flag name.
func positiveDuration(name, v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s: must be positive, not %s", name, v)
	}
	return d, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
