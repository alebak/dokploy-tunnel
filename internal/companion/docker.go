package companion

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"net/url"
	"slices"

	"github.com/alebak/dokploy-tunnel/internal/docker"
	"github.com/alebak/dokploy-tunnel/internal/dokploy"
	"github.com/alebak/dokploy-tunnel/internal/repeater"
	"github.com/alebak/dokploy-tunnel/internal/tunnel"
)

// DockerBridge is a Bridge that reaches targets through repeater
// containers on the Docker daemon the companion runs next to.
type DockerBridge struct {
	repeater *repeater.Repeater
}

var (
	_ Bridge     = (*DockerBridge)(nil)
	_ PortLister = (*DockerBridge)(nil)
)

// NewDockerBridge returns a DockerBridge that runs repeaters through c.
func NewDockerBridge(c *docker.Client, opts repeater.Options) *DockerBridge {
	return &DockerBridge{repeater: repeater.New(c, opts)}
}

// Open implements Bridge.
func (b *DockerBridge) Open(ctx context.Context, target Target) (io.ReadWriteCloser, error) {
	stream, err := b.repeater.Open(ctx, repeaterTarget(target))
	switch {
	case err == nil:
		return stream, nil
	case errors.Is(err, repeater.ErrNetworkNotAttachable):
		return nil, fmt.Errorf("%w: %v", ErrNetworkNotAttachable, err)
	case errors.Is(err, repeater.ErrTooManyRepeaters):
		return nil, fmt.Errorf("%w: %v", ErrTooManyTunnels, err)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return nil, err
	default:
		return nil, fmt.Errorf("%w: %v", ErrTargetUnreachable, err)
	}
}

// ExposedPorts implements PortLister: the TCP ports the image of target's
// running container exposes, as Docker reports them in
// Config.ExposedPorts, for clients that need a port the user did not give.
// It only inspects the target; no repeater is created.
func (b *DockerBridge) ExposedPorts(ctx context.Context, target Target) ([]tunnel.Port, error) {
	ports, err := b.repeater.ExposedPorts(ctx, repeaterTarget(target))
	switch {
	case err == nil:
		return tcpPorts(ports), nil
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return nil, err
	default:
		return nil, fmt.Errorf("%w: %v", ErrTargetUnreachable, err)
	}
}

// tcpPorts returns the TCP ports among ports, sorted by number and without
// duplicates.
func tcpPorts(ports []repeater.Port) []tunnel.Port {
	var tcp []tunnel.Port
	for _, p := range ports {
		if p.Protocol == tunnel.ProtocolTCP {
			tcp = append(tcp, tunnel.Port{Port: p.Number, Protocol: tunnel.ProtocolTCP})
		}
	}
	slices.SortFunc(tcp, func(a, b tunnel.Port) int { return cmp.Compare(a.Port, b.Port) })
	return slices.Compact(tcp)
}

// Run removes orphaned repeaters, now and periodically, until ctx is done.
func (b *DockerBridge) Run(ctx context.Context) {
	b.repeater.Run(ctx)
}

// Close removes every repeater the bridge runs.
func (b *DockerBridge) Close(ctx context.Context) error {
	return b.repeater.Close(ctx)
}

// exposedDockerHost reports whether host is a tcp:// endpoint that is not
// on the loopback interface.
func exposedDockerHost(host string) bool {
	u, err := url.Parse(host)
	if err != nil || u.Scheme != "tcp" {
		return false
	}
	if u.Hostname() == "localhost" {
		return false
	}
	addr, err := netip.ParseAddr(u.Hostname())
	return err != nil || !addr.IsLoopback()
}

// repeaterTarget describes target the way Dokploy deployed it.
func repeaterTarget(target Target) repeater.Target {
	t := repeater.Target{Kind: repeater.KindSwarmService, AppName: target.Service.AppName, Port: target.Port}
	if target.ServiceType == dokploy.ServiceCompose {
		t.Kind, t.Service = repeater.KindCompose, target.ComposeService
		if target.Service.ComposeType == "stack" {
			t.Kind = repeater.KindStack
		}
	}
	return t
}

// NewBridge returns the Bridge cfg selects, and a func that releases it.
// The Docker bridge checks that the daemon answers, and removes orphaned
// repeaters until ctx is done.
func NewBridge(ctx context.Context, cfg Config, log *slog.Logger) (Bridge, func(context.Context) error, error) {
	if cfg.Bridge == BridgeNone {
		log.Warn("forwarding is disabled: every authorized tunnel is refused with target_unreachable")
		return UnavailableBridge{}, func(context.Context) error { return nil }, nil
	}
	client, err := docker.New(cfg.DockerHost)
	if err != nil {
		return nil, nil, err
	}
	if exposedDockerHost(cfg.DockerHost) {
		log.Warn("the Docker API is reached over plain TCP on a non-loopback address: whoever can reach it controls the host; keep it on a private network only the companion joins",
			"docker_host", cfg.DockerHost)
	}
	if err := client.Ping(ctx); err != nil {
		return nil, nil, fmt.Errorf("reaching Docker at %s: %w", cfg.DockerHost, err)
	}
	key, err := repeaterKey(cfg.RepeaterKeyFile, log)
	if err != nil {
		return nil, nil, err
	}
	b := NewDockerBridge(client, repeater.Options{
		Key:          key,
		Image:        cfg.RepeaterImage,
		Grace:        cfg.RepeaterGrace,
		TTL:          cfg.ReaperTTL,
		MaxRepeaters: cfg.MaxRepeaters,
		Log:          log.With("component", "repeater"),
	})
	log.Info("forwarding through Docker", "docker_host", cfg.DockerHost, "docker_api", client.APIVersion(),
		"repeater_image", cfg.RepeaterImage)
	go b.Run(ctx)
	return b, b.Close, nil
}
