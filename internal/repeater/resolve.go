// Package repeater reaches services on segmented Docker networks without
// joining them: for each target it runs an idle repeater container
// attached only to one of the target's own networks, and opens every TCP
// connection as a `socat STDIO TCP:<ip>:<port>` exec in it.
//
// The endpoint is derived only from sources tied to the identity Dokploy
// authorized, never from names or labels a tenant could copy:
//
//   - A Dokploy application or database, and a service of a compose stack
//     deployed with `docker stack deploy`, is a Swarm service. It is found
//     by its exact name through the Swarm API, and its running tasks give
//     the container IDs, networks and addresses. Swarm names services
//     uniquely; container labels are never read for these targets.
//   - A service of a compose stack deployed with `docker compose` has no
//     Swarm object. Its containers are found by their Compose labels, which
//     Compose sets itself and lets no compose file override, and are only
//     accepted when their project directory is the one Dokploy deploys that
//     appName from. A container carrying any Swarm label is a Swarm task,
//     whose labels a stack file can set freely, and is refused.
//
// socat then dials the IP address the daemon reports for the target on the
// chosen network, so no DNS name another container could claim is involved.
package repeater

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/alebak/dokploy-tunnel/internal/docker"
)

var (
	// ErrTargetUnreachable means the target has no running container, no
	// network a repeater can reach it on, is one the companion refuses to
	// forward to, or refused the connection.
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
	// `docker stack deploy <appName>` (composeType "stack"): the Swarm
	// service <appName>_<service>.
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

// String names t for logs and labels: its appName, and its compose service
// when it has one.
func (t Target) String() string {
	if t.Kind == KindSwarmService {
		return t.AppName
	}
	return t.AppName + "/" + t.Service
}

// Port is a port a container image exposes.
type Port struct {
	Number   int
	Protocol string
}

// Endpoint is where a repeater reaches a target.
type Endpoint struct {
	// Addr is the target's IP address on Network, as the daemon reports it.
	Addr netip.Addr
	// Name describes the target, for logs and labels only.
	Name string
	// Network is the network the repeater joins.
	Network docker.Network
	// ContainerID is the target container that was inspected; empty when
	// the Swarm task runs on another node.
	ContainerID string
	// ExposedPorts are the ports the target's image exposes, sorted. They
	// are unknown, and empty, without a local container.
	ExposedPorts []Port
}

// DefaultComposeDir is where Dokploy keeps compose deployments: it runs
// `docker compose -p <appName>` from <dir>/<appName>/code (COMPOSE_PATH
// and getBuildComposeCommand in Dokploy's packages/server/src/constants/
// index.ts and utils/builders/compose.ts), the same path on remote servers.
const DefaultComposeDir = "/etc/dokploy/compose"

// Docker labels set by Compose and Swarm.
const (
	labelComposeProject     = "com.docker.compose.project"
	labelComposeService     = "com.docker.compose.service"
	labelComposeWorkingDir  = "com.docker.compose.project.working_dir"
	labelComposeConfigFiles = "com.docker.compose.project.config_files"
	labelStackNamespace     = "com.docker.stack.namespace"
	// swarmLabelPrefix starts the labels Swarm sets on task containers.
	swarmLabelPrefix = "com.docker.swarm."
)

// sharedNetwork is the shared overlay every Dokploy service joins unless
// told not to. A repeater joins it only when the target is on no narrower
// network.
const sharedNetwork = "dokploy-network"

// dnsName matches the app and service names a target may have; nothing
// outside it reaches the Docker API.
var dnsName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$`)

// reservedAppName reports whether appName is one of Dokploy's own: its
// panel, database, cache and proxy run as "dokploy", "dokploy-postgres",
// "dokploy-redis" and "dokploy-traefik". Dokploy lets users pick appNames
// freely (^[a-z](?!.*--)([a-z0-9-]*[a-z0-9])?$ admits "dokploy-postgres"),
// so a user's service could be named like Dokploy's own and be resolved to
// it. Those names are refused outright.
func reservedAppName(appName string) bool {
	return appName == "dokploy" || strings.HasPrefix(appName, "dokploy-")
}

// validate checks t's names before anything is asked of Docker.
func (t Target) validate() error {
	if !dnsName.MatchString(t.AppName) {
		return fmt.Errorf("%w: unusable app name %q", ErrTargetUnreachable, t.AppName)
	}
	if reservedAppName(t.AppName) {
		return fmt.Errorf("%w: app name %q is reserved for Dokploy's own services", ErrTargetUnreachable, t.AppName)
	}
	switch t.Kind {
	case KindSwarmService:
		return nil
	case KindCompose, KindStack:
		if t.Service == "" {
			return fmt.Errorf("%w: name a service inside compose stack %s", ErrTargetUnreachable, t.AppName)
		}
		if !dnsName.MatchString(t.Service) {
			return fmt.Errorf("%w: unusable compose service name %q", ErrTargetUnreachable, t.Service)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown target kind %d", ErrTargetUnreachable, t.Kind)
	}
}

// resolver finds targets on one Docker daemon.
type resolver struct {
	docker *docker.Client
	// composeDir is where Dokploy keeps compose deployments.
	composeDir string
}

// candidate is a network the target is on.
type candidate struct {
	network docker.Network
	addr    netip.Addr
	rank    int
}

// resolve finds where a repeater can reach t: a running container or task
// of t, and the best of its networks a standalone container may join. It
// fails with ErrTargetUnreachable or a *NotAttachableError.
func (rs resolver) resolve(ctx context.Context, t Target) (Endpoint, error) {
	loc, err := rs.locate(ctx, t)
	if err != nil {
		return Endpoint{}, err
	}
	return rs.chooseNetwork(ctx, t, loc)
}

// located is a target's container or task, with its address on each
// network it is attached to.
type located struct {
	// ep has ContainerID and ExposedPorts.
	ep Endpoint
	// attached maps network IDs to the target's address there.
	attached map[string]netip.Addr
}

// locate finds t's running container or Swarm task.
func (rs resolver) locate(ctx context.Context, t Target) (located, error) {
	if err := t.validate(); err != nil {
		return located{}, err
	}
	var loc located
	var err error
	switch t.Kind {
	case KindCompose:
		loc, err = rs.locateCompose(ctx, t)
	default:
		loc, err = rs.locateSwarm(ctx, t)
	}
	loc.ep.Name = t.String()
	return loc, err
}

// locateSwarm finds a running task of t's Swarm service, by the service's
// exact name.
func (rs resolver) locateSwarm(ctx context.Context, t Target) (located, error) {
	name := t.AppName
	if t.Kind == KindStack {
		name = t.AppName + "_" + t.Service
	}
	svc, err := rs.docker.InspectService(ctx, name)
	if docker.IsNotFound(err) {
		return located{}, fmt.Errorf("%w: no Swarm service %s", ErrTargetUnreachable, name)
	}
	if err != nil {
		return located{}, fmt.Errorf("%w: inspecting Swarm service %s: %v", ErrTargetUnreachable, name, err)
	}
	// The daemon also resolves an ID prefix; only the exact name counts.
	if svc.Spec.Name != name || svc.ID == "" {
		return located{}, fmt.Errorf("%w: no Swarm service %s", ErrTargetUnreachable, name)
	}
	if t.Kind == KindStack && svc.Spec.Labels[labelStackNamespace] != t.AppName {
		return located{}, fmt.Errorf("%w: Swarm service %s is not part of stack %s", ErrTargetUnreachable, name, t.AppName)
	}
	for _, m := range svc.Spec.TaskTemplate.ContainerSpec.Mounts {
		if isDockerSocket(m.Source) || isDockerSocket(m.Target) {
			return located{}, fmt.Errorf("%w: refusing Swarm service %s: it mounts the Docker socket", ErrTargetUnreachable, name)
		}
	}

	tasks, err := rs.docker.ListTasks(ctx, docker.TaskListOptions{Service: svc.ID, DesiredState: "running"})
	if err != nil {
		return located{}, fmt.Errorf("%w: listing the tasks of %s: %v", ErrTargetUnreachable, name, err)
	}
	tasks = slices.DeleteFunc(tasks, func(task docker.Task) bool {
		return task.ServiceID != svc.ID || task.Status.State != "running"
	})
	if len(tasks) == 0 {
		return located{}, fmt.Errorf("%w: no running task of Swarm service %s", ErrTargetUnreachable, name)
	}
	slices.SortFunc(tasks, func(a, b docker.Task) int { return cmp.Compare(a.ID, b.ID) })
	task := tasks[0]

	var loc located
	if id := task.Status.ContainerStatus.ContainerID; id != "" {
		// The container is only inspectable on the node running it.
		ctr, err := rs.docker.InspectContainer(ctx, id)
		switch {
		case err == nil:
			if why := unsafeContainer(ctr); why != "" {
				return located{}, fmt.Errorf("%w: refusing Swarm service %s: %s", ErrTargetUnreachable, name, why)
			}
			loc.ep.ContainerID = ctr.ID
			loc.ep.ExposedPorts = exposedPorts(ctr.Config.ExposedPorts)
		case !docker.IsNotFound(err):
			return located{}, fmt.Errorf("%w: inspecting the task container of %s: %v", ErrTargetUnreachable, name, err)
		}
	}
	loc.attached = map[string]netip.Addr{}
	for _, att := range task.NetworksAttachments {
		if att.Network.ID == "" || len(att.Addresses) == 0 {
			continue
		}
		if p, err := netip.ParsePrefix(att.Addresses[0]); err == nil {
			loc.attached[att.Network.ID] = p.Addr()
		}
	}
	return loc, nil
}

// locateCompose finds the running container of a docker-compose service
// that Dokploy deployed for t.AppName.
func (rs resolver) locateCompose(ctx context.Context, t Target) (located, error) {
	found, err := rs.docker.ListContainers(ctx, docker.ListOptions{Labels: []string{
		labelComposeProject + "=" + t.AppName, labelComposeService + "=" + t.Service,
	}})
	if err != nil {
		return located{}, fmt.Errorf("%w: listing the target's containers: %v", ErrTargetUnreachable, err)
	}
	// Order by ID, which the daemon assigns, so that no name chosen by a
	// tenant can put its container first.
	slices.SortFunc(found, func(a, b docker.ContainerSummary) int { return cmp.Compare(a.ID, b.ID) })

	var accepted []docker.Container
	for _, s := range found {
		ctr, err := rs.docker.InspectContainer(ctx, s.ID)
		if docker.IsNotFound(err) {
			continue
		}
		if err != nil {
			return located{}, fmt.Errorf("%w: inspecting the target's container: %v", ErrTargetUnreachable, err)
		}
		if rs.composeContainerOf(ctr, t) {
			accepted = append(accepted, ctr)
		}
	}
	if len(accepted) == 0 {
		return located{}, fmt.Errorf("%w: no running container of %s deployed by Dokploy", ErrTargetUnreachable, t)
	}
	// Replicas of one service share a project directory and networks.
	// Containers that disagree mean something else wears the labels:
	// choosing one would be a guess.
	first := accepted[0]
	for _, ctr := range accepted[1:] {
		if projectDir(ctr) != projectDir(first) || !slices.Equal(networkIDs(ctr), networkIDs(first)) {
			return located{}, fmt.Errorf("%w: the containers labelled %s disagree on their project or networks; refusing to choose", ErrTargetUnreachable, t)
		}
	}
	for _, ctr := range accepted {
		if why := unsafeContainer(ctr); why != "" {
			return located{}, fmt.Errorf("%w: refusing %s: %s", ErrTargetUnreachable, t, why)
		}
	}

	loc := located{attached: map[string]netip.Addr{}}
	loc.ep.ContainerID = first.ID
	loc.ep.ExposedPorts = exposedPorts(first.Config.ExposedPorts)
	for _, s := range first.NetworkSettings.Networks {
		if addr, err := netip.ParseAddr(s.IPAddress); err == nil && s.NetworkID != "" {
			loc.attached[s.NetworkID] = addr
		}
	}
	return loc, nil
}

// composeContainerOf reports whether ctr is a running container that
// docker compose created for t from Dokploy's directory for t.AppName.
func (rs resolver) composeContainerOf(ctr docker.Container, t Target) bool {
	labels := ctr.Config.Labels
	if !ctr.State.Running || labels[labelComposeProject] != t.AppName || labels[labelComposeService] != t.Service {
		return false
	}
	// A Swarm task's container labels come from the stack file, which
	// may copy any Compose label.
	for k := range labels {
		if strings.HasPrefix(k, swarmLabelPrefix) {
			return false
		}
	}
	dir := path.Join(rs.composeDir, t.AppName)
	if wd := labels[labelComposeWorkingDir]; wd != "" {
		return within(wd, dir)
	}
	files := strings.Split(labels[labelComposeConfigFiles], ",")
	return labels[labelComposeConfigFiles] != "" && !slices.ContainsFunc(files, func(f string) bool { return !within(f, dir) })
}

// within reports whether the absolute path p is dir or inside it.
func within(p, dir string) bool {
	p = path.Clean(p)
	return path.IsAbs(p) && (p == dir || strings.HasPrefix(p, dir+"/"))
}

// projectDir is a Compose container's project directory, or its compose
// files when it has none.
func projectDir(ctr docker.Container) string {
	return cmp.Or(ctr.Config.Labels[labelComposeWorkingDir], ctr.Config.Labels[labelComposeConfigFiles])
}

// networkIDs returns the IDs of the networks ctr is on, sorted.
func networkIDs(ctr docker.Container) []string {
	ids := make([]string, 0, len(ctr.NetworkSettings.Networks))
	for _, s := range ctr.NetworkSettings.Networks {
		ids = append(ids, s.NetworkID)
	}
	slices.Sort(ids)
	return ids
}

// unsafeContainer returns why the companion must not forward to ctr, or
// "". A privileged container, one on the host's network stack, or one
// holding the Docker socket is host infrastructure, such as a Docker
// socket proxy: a tunnel to it would hand whoever may read the Dokploy
// service control of the host.
func unsafeContainer(ctr docker.Container) string {
	switch {
	case ctr.HostConfig.Privileged:
		return "it runs privileged"
	case ctr.HostConfig.NetworkMode == "host":
		return "it uses the host's network"
	}
	for _, m := range ctr.Mounts {
		if isDockerSocket(m.Source) || isDockerSocket(m.Destination) {
			return "it mounts the Docker socket"
		}
	}
	for _, b := range ctr.HostConfig.Binds {
		src, rest, _ := strings.Cut(b, ":")
		dst, _, _ := strings.Cut(rest, ":")
		if isDockerSocket(src) || isDockerSocket(dst) {
			return "it mounts the Docker socket"
		}
	}
	return ""
}

// isDockerSocket reports whether p is a Docker daemon socket, at
// /var/run/docker.sock, /run/docker.sock or a rootless daemon's path.
func isDockerSocket(p string) bool {
	return p != "" && path.Base(p) == "docker.sock"
}

// chooseNetwork picks the best network of loc a repeater may join, and
// the target's address there.
func (rs resolver) chooseNetwork(ctx context.Context, t Target, loc located) (Endpoint, error) {
	var candidates []candidate
	var closed []string
	for _, id := range slices.Sorted(maps.Keys(loc.attached)) {
		n, err := rs.docker.InspectNetwork(ctx, id)
		if docker.IsNotFound(err) {
			continue
		}
		if err != nil {
			return Endpoint{}, fmt.Errorf("%w: inspecting network %s: %v", ErrTargetUnreachable, id, err)
		}
		if n.ID != id || !usable(n) {
			continue
		}
		if n.Driver == "overlay" && !n.Attachable {
			closed = append(closed, n.Name)
			continue
		}
		cand := candidate{network: n, addr: loc.attached[id], rank: 1}
		if owned(n, t.AppName) {
			cand.rank = 0
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
	ep := loc.ep
	ep.Addr, ep.Network = best.addr, best.network
	return ep, nil
}

// usable reports whether a standalone container joining n could reach a
// container on it. The default bridge isolates nothing a repeater should
// rely on; host, none and the Swarm ingress network carry no service
// traffic.
func usable(n docker.Network) bool {
	switch {
	case n.Ingress, n.Name == "bridge", n.Driver == "host", n.Driver == "null", n.Name == "none", n.Name == "host":
		return false
	}
	return true
}

// owned reports whether n belongs to the deployment appName: created by
// its Compose project or Swarm stack, or the network Dokploy creates for an
// isolated deployment, named after the appName. It only ranks networks the
// target is already on.
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
