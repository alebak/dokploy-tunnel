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
	// TaskID is the Swarm task found for the target; empty for a
	// docker-compose service.
	TaskID string
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
	dataRoot, err := rs.dataRoot(ctx)
	if err != nil {
		return located{}, err
	}
	// A task on another node cannot be inspected: the spec is all there
	// is to judge it by.
	if why := unsafeServiceSpec(svc.Spec.TaskTemplate.ContainerSpec, dataRoot); why != "" {
		return located{}, fmt.Errorf("%w: refusing Swarm service %s: %s", ErrTargetUnreachable, name, why)
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

	loc := located{ep: Endpoint{TaskID: task.ID}}
	if id := task.Status.ContainerStatus.ContainerID; id != "" {
		// The container is only inspectable on the node running it.
		ctr, err := rs.docker.InspectContainer(ctx, id)
		switch {
		case err == nil:
			why, err := rs.unsafeTarget(ctx, ctr, dataRoot)
			if err != nil {
				return located{}, fmt.Errorf("%w: judging the task container of %s: %v", ErrTargetUnreachable, name, err)
			}
			if why != "" {
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
	dataRoot, err := rs.dataRoot(ctx)
	if err != nil {
		return located{}, err
	}
	for _, ctr := range accepted {
		why, err := rs.unsafeTarget(ctx, ctr, dataRoot)
		if err != nil {
			return located{}, fmt.Errorf("%w: judging the target's container: %v", ErrTargetUnreachable, err)
		}
		if why != "" {
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
	return path.IsAbs(p) && (p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/"))
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

// dataRoot returns the data root the daemon reports, which a target must
// not reach. Without it no target can be judged, so it fails with
// ErrTargetUnreachable.
func (rs resolver) dataRoot(ctx context.Context) (string, error) {
	dir, err := rs.docker.DockerRootDir(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: reading the Docker daemon's data root: %v", ErrTargetUnreachable, err)
	}
	return dir, nil
}

// unsafeTarget returns why the companion must not forward to ctr, or "":
// the reasons of unsafeContainer, and named volumes that reach the host.
// dataRoot is the daemon's data root. It fails when a volume cannot be
// inspected.
func (rs resolver) unsafeTarget(ctx context.Context, ctr docker.Container, dataRoot string) (string, error) {
	if why := unsafeContainer(ctr, dataRoot); why != "" {
		return why, nil
	}
	for _, m := range ctr.Mounts {
		if m.Type != "volume" {
			continue
		}
		if m.Name == "" {
			return "it mounts a volume without a name", nil
		}
		v, err := rs.docker.InspectVolume(ctx, m.Name)
		if docker.IsNotFound(err) {
			return "it mounts volume " + m.Name + ", which the daemon does not report", nil
		}
		if err != nil {
			return "", fmt.Errorf("inspecting volume %s: %v", m.Name, err)
		}
		if volumeExposesHost(v.Driver, v.Options, dataRoot) {
			return mountsHostPath, nil
		}
	}
	return "", nil
}

// unsafeContainer returns why the companion must not forward to ctr, or
// "", judging by its inspection alone. A privileged container, one sharing
// the host's network, PID, IPC or UTS namespace, one given host devices,
// capabilities or kernel parameters that reach past the container, one
// running with seccomp, AppArmor, SELinux labeling or the masked system
// paths turned off, or one that can reach a container daemon's socket or
// data, including dataRoot, is host infrastructure, such as a Docker socket
// proxy, or has a weakened boundary to the host: a tunnel to it would hand
// whoever may read the Dokploy service control of the host. Named volumes
// are judged by unsafeTarget.
func unsafeContainer(ctr docker.Container, dataRoot string) string {
	hc := ctr.HostConfig
	switch {
	case hc.Privileged:
		return "it runs privileged"
	case hc.NetworkMode == "host":
		return "it uses the host's network"
	case hc.PidMode == "host":
		return "it uses the host's PID namespace"
	case hc.IpcMode == "host":
		return "it uses the host's IPC namespace"
	case hc.UTSMode == "host":
		return "it uses the host's UTS namespace"
	case len(hc.Devices) > 0, len(hc.DeviceRequests) > 0:
		return "it is given host devices"
	}
	if c := dangerousCapability(hc.CapAdd); c != "" {
		return "it adds capability " + c
	}
	if k := hostSysctl(hc.Sysctls); k != "" {
		return "it sets kernel parameter " + k
	}
	if o := weakenedSecurityOpt(hc.SecurityOpt); o != "" {
		return "it runs with security option " + o
	}
	// The daemon fills in its default masked and read-only paths; the
	// client's "systempaths=unconfined" empties both. A daemon too old to
	// report them leaves them out.
	if (hc.MaskedPaths != nil && len(hc.MaskedPaths) == 0) || (hc.ReadonlyPaths != nil && len(hc.ReadonlyPaths) == 0) {
		return "it unmasks the kernel's system paths"
	}
	for _, m := range ctr.Mounts {
		// A volume's source is its mountpoint in the daemon's data root;
		// what it mounts is judged from the volume itself.
		exposes := mountExposesHost(m.Source)
		if m.Type != "volume" {
			exposes = bindExposesHost(m.Source, dataRoot)
		}
		if exposes || isSocketPath(m.Destination) {
			return mountsHostPath
		}
	}
	for _, b := range hc.Binds {
		src, rest, _ := strings.Cut(b, ":")
		dst, _, _ := strings.Cut(rest, ":")
		if bindExposesHost(src, dataRoot) || isSocketPath(dst) {
			return mountsHostPath
		}
	}
	return ""
}

// unsafeServiceSpec returns why the companion must not forward to the
// tasks of a Swarm service with spec, or "". A task on another node cannot
// be inspected, so the spec is judged with the rules of unsafeContainer
// that it can express, against dataRoot, the data root of the daemon
// judging it. Swarm has no privileged mode, host namespaces, device
// mappings, device requests or system paths to check.
func unsafeServiceSpec(spec docker.ContainerSpec, dataRoot string) string {
	for _, m := range spec.Mounts {
		if isSocketPath(m.Target) {
			return mountsHostPath
		}
		if m.Type != "volume" {
			if bindExposesHost(m.Source, dataRoot) {
				return mountsHostPath
			}
			continue
		}
		// Each node creates a missing volume from the driver config.
		if dc := volumeDriverConfig(m); dc != nil && volumeExposesHost(dc.Name, dc.Options, dataRoot) {
			return mountsHostPath
		}
	}
	if c := dangerousCapability(spec.CapabilityAdd); c != "" {
		return "it adds capability " + c
	}
	if k := hostSysctl(spec.Sysctls); k != "" {
		return "it sets kernel parameter " + k
	}
	if p := spec.Privileges; p != nil {
		switch {
		case p.Seccomp != nil && p.Seccomp.Mode == "unconfined":
			return "it runs without seccomp"
		case p.AppArmor != nil && p.AppArmor.Mode == "disabled":
			return "it runs without AppArmor"
		case p.SELinuxContext != nil && p.SELinuxContext.Disable:
			return "it runs with SELinux labeling disabled"
		}
	}
	return ""
}

// weakenedSecurityOpt returns the first of opts that turns off seccomp,
// AppArmor, SELinux labeling or the masked system paths, or "". It parses
// options as the daemon does: "key=value", or the legacy "key:value" when
// there is no "=", and a bare "disable" for "label=disable". Default and
// custom profiles, no-new-privileges and other label options are safe.
func weakenedSecurityOpt(opts []string) string {
	for _, opt := range opts {
		o := strings.TrimSpace(opt)
		if strings.EqualFold(o, "disable") {
			return opt
		}
		k, v, ok := strings.Cut(o, "=")
		if !ok {
			k, v, _ = strings.Cut(o, ":")
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		switch {
		case (k == "seccomp" || k == "apparmor" || k == "systempaths") && strings.EqualFold(v, "unconfined"),
			k == "label" && strings.EqualFold(v, "disable"):
			return opt
		}
	}
	return ""
}

// volumeDriverConfig is the driver config of a Swarm volume mount, or nil.
func volumeDriverConfig(m docker.ServiceMount) *docker.VolumeDriverConfig {
	if m.VolumeOptions == nil {
		return nil
	}
	return m.VolumeOptions.DriverConfig
}

const mountsHostPath = "it mounts a daemon socket, a directory that may hold one, a daemon's data or a host device"

// volumeExposesHost reports whether a volume of driver with options
// mounts a host path or device, dataRoot being the daemon's data root. The local driver, also used when driver
// is empty, passes "o" to mount(8): with "bind" or "rbind" it binds the
// host path "device", judged as a bind mount; without, an absolute
// "device" is a host block device, such as a disk holding the host's
// root file system. NFS, CIFS (device "//server/share") and tmpfs volumes
// name no host path. A bind whose device is not an absolute path cannot be
// judged and is refused.
func volumeExposesHost(driver string, options map[string]string, dataRoot string) bool {
	if driver != "" && driver != "local" {
		return false
	}
	device := options["device"]
	binds := slices.ContainsFunc(strings.Split(options["o"], ","), func(o string) bool {
		o = strings.TrimSpace(o)
		return o == "bind" || o == "rbind"
	})
	if binds {
		return !path.IsAbs(device) || bindExposesHost(device, dataRoot)
	}
	return path.IsAbs(device) && !strings.HasPrefix(device, "//")
}

// daemonSockets are where container daemons listen. /var/run is a link to
// /run on current distributions, but a mount names whichever path it was
// given.
var daemonSockets = []string{
	"/run/docker.sock", "/var/run/docker.sock",
	"/run/containerd/containerd.sock", "/var/run/containerd/containerd.sock",
	"/run/podman/podman.sock", "/var/run/podman/podman.sock",
}

// rootlessRuntimeDirs hold the per-user runtime directories, such as
// /run/user/1000, where rootless daemons put their sockets.
var rootlessRuntimeDirs = []string{"/run/user", "/var/run/user"}

// mountExposesHost reports whether mounting the host path src could hand
// a container a daemon socket: src is a socket by name, a known daemon
// socket or a directory holding one, or a rootless runtime directory, a
// path inside one or a directory holding one. A source that is not an
// absolute path names a volume and is judged by its destination alone.
func mountExposesHost(src string) bool {
	if !path.IsAbs(src) {
		return false
	}
	src = path.Clean(src)
	if isSocketPath(src) {
		return true
	}
	for _, s := range daemonSockets {
		if within(s, src) {
			return true
		}
	}
	for _, d := range rootlessRuntimeDirs {
		if within(d, src) || within(src, d) {
			return true
		}
	}
	return false
}

// daemonDataRoots are the default data roots of container daemons, holding
// every container's file system, image and volume: Docker's, containerd's
// and Podman's (shared with CRI-O).
var daemonDataRoots = []string{"/var/lib/docker", "/var/lib/containerd", "/var/lib/containers"}

// rootlessDataRoot ends the default data root of a rootless daemon,
// $HOME/.local/share/docker.
const rootlessDataRoot = "/.local/share/docker"

// bindExposesHost reports whether binding the host path src into a
// container could hand it control of the host: mountExposesHost, or src
// is a daemon data root, a path inside one or a directory holding one.
// The data roots are the defaults and dataRoot, the one the daemon
// reports, when it is an absolute path. The rootless data root is
// recognized by its default location in any home directory; a bind of a
// whole home directory is not. A volume's source is under a data root, so
// volumes are judged by mountExposesHost.
func bindExposesHost(src, dataRoot string) bool {
	if mountExposesHost(src) {
		return true
	}
	if !path.IsAbs(src) {
		return false
	}
	src = path.Clean(src)
	roots := daemonDataRoots
	if path.IsAbs(dataRoot) {
		roots = append(slices.Clip(daemonDataRoots), path.Clean(dataRoot))
	}
	for _, d := range roots {
		if within(d, src) || within(src, d) {
			return true
		}
	}
	return strings.Contains(src+"/", rootlessDataRoot+"/") ||
		strings.HasSuffix(src, "/.local") || strings.HasSuffix(src, "/.local/share")
}

// ipcSysctls are the kernel parameters scoped to a container's own IPC
// namespace, as runc allows them.
var ipcSysctls = []string{
	"kernel.msgmax", "kernel.msgmnb", "kernel.msgmni", "kernel.sem",
	"kernel.shmall", "kernel.shmmax", "kernel.shmmni", "kernel.shm_rmid_forced",
}

// hostSysctl returns the first of sysctls, sorted, that is not scoped to
// the container's own namespaces, or "". It follows runc's validation: IPC
// and fs.mqueue parameters are scoped to the IPC namespace, net ones to the
// network namespace, kernel.domainname to the UTS namespace and user ones
// to the user namespace; sharing the host's namespaces is refused
// separately.
func hostSysctl(sysctls map[string]string) string {
	for _, k := range slices.Sorted(maps.Keys(sysctls)) {
		if !namespacedSysctl(sysctlDots(k)) {
			return k
		}
	}
	return ""
}

func namespacedSysctl(k string) bool {
	return slices.Contains(ipcSysctls, k) || k == "kernel.domainname" ||
		strings.HasPrefix(k, "fs.mqueue.") || strings.HasPrefix(k, "net.") ||
		strings.HasPrefix(k, "user.")
}

// sysctlDots converts a slash-separated sysctl name such as
// net/ipv4/ip_forward to its dotted form, swapping any dots in it for
// slashes, as runc does.
func sysctlDots(k string) string {
	if i := strings.IndexAny(k, "./"); i < 0 || k[i] == '.' {
		return k
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case '/':
			return '.'
		case '.':
			return '/'
		}
		return r
	}, k)
}

// isSocketPath reports whether p names a Unix socket by convention.
func isSocketPath(p string) bool {
	return p != "" && strings.HasSuffix(path.Base(p), ".sock")
}

// dangerousCaps are capabilities that let a container act on the host:
// mount file systems, trace or read host processes and files, load kernel
// modules or programs, or reconfigure networking.
var dangerousCaps = []string{
	"ALL", "SYS_ADMIN", "SYS_PTRACE", "SYS_MODULE", "SYS_RAWIO",
	"DAC_READ_SEARCH", "NET_ADMIN", "BPF", "PERFMON",
}

// dangerousCapability returns the first of caps, which may carry a CAP_
// prefix in any case, that is in dangerousCaps, or "".
func dangerousCapability(caps []string) string {
	for _, c := range caps {
		if slices.Contains(dangerousCaps, strings.TrimPrefix(strings.ToUpper(c), "CAP_")) {
			return c
		}
	}
	return ""
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
