package dockerproxy

import (
	"flag"
	"fmt"
	"io"

	"github.com/alebak/dokploy-tunnel/internal/docker"
	"github.com/alebak/dokploy-tunnel/internal/repeater"
)

// Environment variables that configure the proxy; flags win over them.
const (
	EnvListen        = "DOKTUNNEL_SOCKET_PROXY_LISTEN"
	EnvRepeaterImage = "DOKTUNNEL_SOCKET_PROXY_REPEATER_IMAGE"
	// EnvDockerHost is Docker's own variable, so the proxy finds the
	// daemon the way the docker CLI does.
	EnvDockerHost = "DOCKER_HOST"
)

const defaultListen = ":2375"

// Config configures doktunnel-socket-proxy.
type Config struct {
	// Listen is the TCP address to serve on, such as ":2375".
	Listen string
	// DockerHost is the Docker daemon's endpoint, such as
	// "unix:///var/run/docker.sock".
	DockerHost string
	// RepeaterImage is the only image repeaters may run; it must be the
	// companion's.
	RepeaterImage string
	// Version asks for the build version instead of serving.
	Version bool
}

// ParseConfig reads the configuration from the command-line arguments args
// and from the environment through getenv; flags win. Usage is written to
// usage. It returns flag.ErrHelp for --help.
func ParseConfig(args []string, getenv func(string) string, usage io.Writer) (Config, error) {
	fs := flag.NewFlagSet("doktunnel-socket-proxy", flag.ContinueOnError)
	fs.SetOutput(usage)
	listen := fs.String("listen", orDefault(getenv(EnvListen), defaultListen),
		"TCP `address` to serve on; only the companion may reach it (env "+EnvListen+")")
	dockerHost := fs.String("docker-host", orDefault(getenv(EnvDockerHost), docker.DefaultHost),
		"Docker daemon `endpoint`: unix:///path or tcp://host:port (env "+EnvDockerHost+")")
	image := fs.String("repeater-image", orDefault(getenv(EnvRepeaterImage), repeater.DefaultImage),
		"the only `image` repeater containers may run; it must equal the companion's --repeater-image (env "+EnvRepeaterImage+")")
	version := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	cfg := Config{Listen: *listen, DockerHost: *dockerHost, RepeaterImage: *image, Version: *version}
	if cfg.Version {
		return cfg, nil
	}
	if _, _, err := docker.ParseHost(cfg.DockerHost); err != nil {
		return Config{}, fmt.Errorf("--docker-host: %w", err)
	}
	return cfg, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
