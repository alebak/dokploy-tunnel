// Package repeater reaches services on segmented Docker networks without
// joining them: for each target it runs an idle repeater container
// attached only to one of the target's own networks, and opens every TCP
// connection as a `socat STDIO TCP:<host>:<port>` exec in it.
//
// Targets are found by Docker metadata the way Dokploy itself finds them
// (getServiceContainer and getComposeContainer in Dokploy's
// packages/server/src/utils/docker/utils.ts), never by guessing names.
package repeater

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/alebak/dokploy-tunnel/internal/docker"
)

var (
	// ErrTargetUnreachable means the target has no running container, no
	// network a repeater can reach it on, or refused the connection.
	ErrTargetUnreachable = errors.New("the target is unreachable")
	// ErrNetworkNotAttachable means the target is only on Swarm overlay
	// networks that standalone containers may not join.
	ErrNetworkNotAttachable = errors.New("the target's network is not attachable")
)

// NotAttachableError names the networks a repeater could not join. It
// wraps ErrNetworkNotAttachable.
type NotAttachableError struct {
	Networks []string
}

func (e *NotAttachableError) Error() string {
	return fmt.Sprintf("the target is only on non-attachable overlay networks (%s)", strings.Join(e.Networks, ", "))
}

// Unwrap returns ErrNetworkNotAttachable.
func (e *NotAttachableError) Unwrap() error { return ErrNetworkNotAttachable }

// Kind is how Dokploy deploys a target.
type Kind int

const (
	// KindSwarmService is a Dokploy application or database: a Swarm
	// service named after its appName (Dokploy's
	// packages/server/src/utils/builders/index.ts and
	// utils/databases/*.ts create it with Name: appName).
	KindSwarmService Kind = iota
	// KindCompose is a service of a compose stack deployed with
	// `docker compose -p <appName>` (composeType "docker-compose", Dokploy's
	// packages/server/src/utils/builders/compose.ts).
	KindCompose
	// KindStack is a service of a compose stack deployed with
	// `docker stack deploy <appName>` (composeType "stack").
	KindStack
)

// Target is a service to reach, as Dokploy deployed it.
type Target struct {
	Kind Kind
	// AppName is Dokploy's appName: the Swarm service, Compose project or
	// stack name.
	AppName string
	// Service is the service name in the compose file, for KindCompose and
	// KindStack.
	Service string
	// Port is the container port to connect to.
	Port int
}

// Port is a port a container image exposes.
type Port struct {
	Number   int
	Protocol string
}

// Endpoint is where a repeater reaches a target.
type Endpoint struct {
	// Host is the target's name on Network: its Swarm service name, its
	// Compose service name on the project's own network, or its container
	// name.
	Host string
	// Network is the network the repeater joins.
	Network docker.Network
	// ContainerID is the target container that was inspected; empty when
	// the target was resolved from its Swarm service because no task runs
	// on this node.
	ContainerID string
	// ExposedPorts are the ports the target's image exposes, sorted. They
	// are unknown, and empty, without a local container.
	ExposedPorts []Port
}

// Docker labels set by Compose and Swarm.
const (
	labelComposeProject = "com.docker.compose.project"
	labelComposeService = "com.docker.compose.service"
	labelStackNamespace = "com.docker.stack.namespace"
	labelSwarmService   = "com.docker.swarm.service.name"
)

// sharedNetwork is the shared overlay every Dokploy service joins unless
// told not to. A repeater joins it only when the target is on no narrower
// network.
const sharedNetwork = "dokploy-network"

// dnsName matches the names a repeater may put in a socat address: socat
// gives ',' ':' and '!' meanings, so only plain DNS labels pass.
var dnsName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$`)

// selector returns the label filters that find t's containers and, for
// Swarm-based kinds, its Swarm service name.
func (t Target) selector() (labels []string, swarmService string, err error) {
	if !dnsName.MatchString(t.AppName) {
		return nil, "", fmt.Errorf("%w: unusable app name %q", ErrTargetUnreachable, t.AppName)
	}
	if t.Kind != KindSwarmService && !dnsName.MatchString(t.Service) {
		if t.Service == "" {
			return nil, "", fmt.Errorf("%w: name a service inside compose stack %s", ErrTargetUnreachable, t.AppName)
		}
		return nil, "", fmt.Errorf("%w: unusable compose service name %q", ErrTargetUnreachable, t.Service)
	}
	switch t.Kind {
	case KindSwarmService:
		return []string{labelSwarmService + "=" + t.AppName}, t.AppName, nil
	case KindStack:
		name := t.AppName + "_" + t.Service
		return []string{labelStackNamespace + "=" + t.AppName, labelSwarmService + "=" + name}, name, nil
	case KindCompose:
		return []string{labelComposeProject + "=" + t.AppName, labelComposeService + "=" + t.Service}, "", nil
	default:
		return nil, "", fmt.Errorf("%w: unknown target kind %d", ErrTargetUnreachable, t.Kind)
	}
}

// candidate is a network the target is on.
type candidate struct {
	network docker.Network
	host    string
	rank    int
}

// Resolve finds where a repeater can reach t: a running container of t,
// and the best of its networks a standalone container may join. It fails
// with ErrTargetUnreachable or a *NotAttachableError.
func Resolve(ctx context.Context, c *docker.Client, t Target) (Endpoint, error) {
	loc, err := locate(ctx, c, t)
	if err != nil {
		return Endpoint{}, err
	}
	return chooseNetwork(ctx, c, t, loc)
}

// ExposedPorts returns the ports the image of t's running container
// exposes (Config.ExposedPorts), sorted, whatever its networks. It is
// empty when no task of a Swarm service runs on this node. It fails with
// ErrTargetUnreachable when t has no running container or service.
func ExposedPorts(ctx context.Context, c *docker.Client, t Target) ([]Port, error) {
	loc, err := locate(ctx, c, t)
	if err != nil {
		return nil, err
	}
	return loc.ep.ExposedPorts, nil
}

// located is a target's container, or its Swarm service, with the networks
// it is attached to.
type located struct {
	// ep has ContainerID and ExposedPorts, and for a Compose container its
	// container name as Host.
	ep           Endpoint
	swarmService string
	// attached maps network IDs or names to the target's names there.
	attached map[string][]string
}

// locate finds t's running container, or its Swarm service when no task
// runs on this node.
func locate(ctx context.Context, c *docker.Client, t Target) (located, error) {
	labels, swarmService, err := t.selector()
	if err != nil {
		return located{}, err
	}
	found, err := c.ListContainers(ctx, docker.ListOptions{Labels: labels})
	if err != nil {
		return located{}, fmt.Errorf("listing the target's containers: %w", err)
	}
	slices.SortFunc(found, func(a, b docker.ContainerSummary) int {
		return cmp.Compare(strings.Join(a.Names, ","), strings.Join(b.Names, ","))
	})

	var ep Endpoint
	var attached map[string][]string
	switch {
	case len(found) > 0:
		ctr, err := c.InspectContainer(ctx, found[0].ID)
		if err != nil {
			return located{}, fmt.Errorf("inspecting the target's container: %w", err)
		}
		ep.ContainerID = ctr.ID
		ep.ExposedPorts = exposedPorts(ctr.Config.ExposedPorts)
		attached = map[string][]string{}
		for name, s := range ctr.NetworkSettings.Networks {
			attached[cmp.Or(s.NetworkID, name)] = append(slices.Clone(s.Aliases), s.DNSNames...)
		}
		if swarmService == "" {
			swarmService = ctr.Config.Labels[labelSwarmService]
		}
		if swarmService == "" {
			// A Compose container: its name resolves on every user-defined
			// network it is on, its service name on the project's own.
			ep.Host = strings.TrimPrefix(ctr.Name, "/")
		}
	case swarmService != "":
		// No task of the service runs on this node; its virtual IP still
		// resolves on its overlay networks.
		svc, err := c.InspectService(ctx, swarmService)
		if docker.IsNotFound(err) {
			return located{}, fmt.Errorf("%w: no running container or Swarm service %s", ErrTargetUnreachable, swarmService)
		}
		if err != nil {
			return located{}, fmt.Errorf("inspecting Swarm service %s: %w", swarmService, err)
		}
		attached = map[string][]string{}
		for _, n := range svc.Spec.TaskTemplate.Networks {
			attached[n.Target] = n.Aliases
		}
	default:
		return located{}, fmt.Errorf("%w: no running container of %s/%s", ErrTargetUnreachable, t.AppName, t.Service)
	}
	return located{ep: ep, swarmService: swarmService, attached: attached}, nil
}

// chooseNetwork picks the best network of loc a repeater may join, and
// the target's name there.
func chooseNetwork(ctx context.Context, c *docker.Client, t Target, loc located) (Endpoint, error) {
	ep, swarmService := loc.ep, loc.swarmService
	var candidates []candidate
	var closed []string
	for ref, names := range loc.attached {
		n, err := c.InspectNetwork(ctx, ref)
		if docker.IsNotFound(err) {
			continue
		}
		if err != nil {
			return Endpoint{}, fmt.Errorf("inspecting network %s: %w", ref, err)
		}
		if !usable(n) {
			continue
		}
		if n.Driver == "overlay" && !n.Attachable {
			closed = append(closed, n.Name)
			continue
		}
		cand := candidate{network: n, host: cmp.Or(swarmService, ep.Host), rank: 1}
		if owned(n, t.AppName) {
			cand.rank = 0
			if swarmService == "" && slices.Contains(names, t.Service) {
				cand.host = t.Service
			}
		} else if n.Name == sharedNetwork {
			cand.rank = 2
		}
		candidates = append(candidates, cand)
	}
	if len(candidates) == 0 {
		if len(closed) > 0 {
			slices.Sort(closed)
			return Endpoint{}, &NotAttachableError{Networks: closed}
		}
		return Endpoint{}, fmt.Errorf("%w: it is on no network a repeater can join", ErrTargetUnreachable)
	}
	best := slices.MinFunc(candidates, func(a, b candidate) int {
		return cmp.Or(cmp.Compare(a.rank, b.rank), cmp.Compare(a.network.Name, b.network.Name))
	})
	if !dnsName.MatchString(best.host) {
		return Endpoint{}, fmt.Errorf("%w: unusable host name %q", ErrTargetUnreachable, best.host)
	}
	ep.Host, ep.Network = best.host, best.network
	return ep, nil
}

// usable reports whether a standalone container joining n could reach a
// container on it by name. The default bridge has no DNS; host, none and
// the Swarm ingress network carry no service traffic.
func usable(n docker.Network) bool {
	switch {
	case n.Ingress, n.Name == "bridge", n.Driver == "host", n.Driver == "null", n.Name == "none", n.Name == "host":
		return false
	}
	return true
}

// owned reports whether n belongs to the deployment appName: created by
// its Compose project or Swarm stack, or the network Dokploy creates for an
// isolated deployment, named after the appName.
func owned(n docker.Network, appName string) bool {
	return n.Labels[labelComposeProject] == appName || n.Labels[labelStackNamespace] == appName || n.Name == appName
}

// exposedPorts parses ExposedPorts keys such as "5432/tcp", sorted by
// number then protocol. Ranges and malformed keys are skipped.
func exposedPorts(m map[string]struct{}) []Port {
	var ports []Port
	for k := range m {
		num, proto, _ := strings.Cut(k, "/")
		n, err := strconv.Atoi(num)
		if err != nil || n < 1 || n > 65535 {
			continue
		}
		ports = append(ports, Port{Number: n, Protocol: cmp.Or(proto, "tcp")})
	}
	slices.SortFunc(ports, func(a, b Port) int {
		return cmp.Or(cmp.Compare(a.Number, b.Number), cmp.Compare(a.Protocol, b.Protocol))
	})
	return ports
}
