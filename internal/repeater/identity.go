package repeater

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/alebak/dokploy-tunnel/internal/docker"
)

// A tunnel dials the target's IP address, but an address only names a
// container while that container holds it: once it stops, Docker may hand
// the address to another container on the network, possibly another
// tenant's on a shared network such as dokploy-network. Open therefore
// binds each dial to the target's identity, not only to its address:
//
//  1. Just before running socat, it pins the target: the container it
//     resolved must still be running and hold the dialed address on the
//     repeater's network. It records the container's start time and its
//     endpoint on that network, which Docker renews on every start and on
//     every reconnection.
//  2. Once socat reports the connection open, it observes the target again
//     and requires the same identity. A container that is still running
//     with the same start time and endpoint held the address the whole
//     time, so the connection, made in between, reached it. Otherwise the
//     stream is closed before Open returns: no byte from the client has
//     been sent, and none read from the peer is passed on.
//
// A Swarm task that runs on another node cannot be inspected. It is pinned
// by its task ID instead: Swarm never restarts a task, and its address on
// an overlay network is fixed for the task's life, so a task still
// reported running with the same address held it throughout. That relies
// on the Swarm manager's view, which learns of a task's end from its node
// asynchronously; this is the residual window.
//
// After Open returns, the TCP connection is bound to the peer it reached:
// a container that later takes over the address cannot join it.

// identity is what tells a target apart from a later holder of its
// address.
type identity struct {
	// startedAt and endpointID are the local container's; both are empty
	// for a task on another node, which is identified by its task ID.
	startedAt  string
	endpointID string
}

// errTargetChanged is why Open refuses a target that is no longer the one
// resolved.
const errTargetChanged = "the target changed while connecting"

// observe returns the current identity of ep's target, failing with
// ErrTargetUnreachable when it no longer holds ep.Addr on ep.Network.
func (r *Repeater) observe(ctx context.Context, ep Endpoint) (identity, error) {
	switch {
	case ep.ContainerID != "":
		return r.observeContainer(ctx, ep)
	case ep.TaskID != "":
		return identity{}, r.observeTask(ctx, ep)
	default:
		return identity{}, fmt.Errorf("%w: %s: nothing identifies it", ErrTargetUnreachable, errTargetChanged)
	}
}

func (r *Repeater) observeContainer(ctx context.Context, ep Endpoint) (identity, error) {
	ctr, err := r.docker.InspectContainer(ctx, ep.ContainerID)
	if docker.IsNotFound(err) {
		return identity{}, fmt.Errorf("%w: %s: its container is gone", ErrTargetUnreachable, errTargetChanged)
	}
	if err != nil {
		return identity{}, fmt.Errorf("%w: inspecting the target's container: %v", ErrTargetUnreachable, err)
	}
	// The daemon also resolves names and ID prefixes; only the same ID
	// counts.
	if ctr.ID != ep.ContainerID || !ctr.State.Running || ctr.State.StartedAt == "" {
		return identity{}, fmt.Errorf("%w: %s: its container is not running", ErrTargetUnreachable, errTargetChanged)
	}
	for _, s := range ctr.NetworkSettings.Networks {
		if s.NetworkID != ep.Network.ID {
			continue
		}
		if addr, err := netip.ParseAddr(s.IPAddress); err != nil || addr != ep.Addr || s.EndpointID == "" {
			break
		}
		return identity{startedAt: ctr.State.StartedAt, endpointID: s.EndpointID}, nil
	}
	return identity{}, fmt.Errorf("%w: %s: it no longer has address %s on %s", ErrTargetUnreachable, errTargetChanged, ep.Addr, ep.Network.Name)
}

func (r *Repeater) observeTask(ctx context.Context, ep Endpoint) error {
	task, err := r.docker.InspectTask(ctx, ep.TaskID)
	if docker.IsNotFound(err) {
		return fmt.Errorf("%w: %s: its Swarm task is gone", ErrTargetUnreachable, errTargetChanged)
	}
	if err != nil {
		return fmt.Errorf("%w: inspecting the target's Swarm task: %v", ErrTargetUnreachable, err)
	}
	if task.ID != ep.TaskID || task.Status.State != "running" {
		return fmt.Errorf("%w: %s: its Swarm task is not running", ErrTargetUnreachable, errTargetChanged)
	}
	for _, att := range task.NetworksAttachments {
		if att.Network.ID != ep.Network.ID || len(att.Addresses) == 0 {
			continue
		}
		if p, err := netip.ParsePrefix(att.Addresses[0]); err == nil && p.Addr() == ep.Addr {
			return nil
		}
	}
	return fmt.Errorf("%w: %s: its Swarm task no longer has address %s on %s", ErrTargetUnreachable, errTargetChanged, ep.Addr, ep.Network.Name)
}
