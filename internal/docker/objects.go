package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ContainerSummary is a container as the list endpoint reports it.
type ContainerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Labels map[string]string `json:"Labels"`
	// Created is the creation time in Unix seconds.
	Created int64  `json:"Created"`
	State   string `json:"State"`
}

// Container is a container as the inspect endpoint reports it, reduced to
// the fields the companion uses.
type Container struct {
	ID string `json:"Id"`
	// Name starts with a slash, as in "/myapp-postgres-1".
	Name    string `json:"Name"`
	Created string `json:"Created"`
	State   struct {
		Running bool   `json:"Running"`
		Status  string `json:"Status"`
		// StartedAt is when the container last started; every start sets
		// it anew.
		StartedAt string `json:"StartedAt"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
		// ExposedPorts are keyed like "5432/tcp".
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
	} `json:"Config"`
	HostConfig struct {
		Privileged bool `json:"Privileged"`
		// NetworkMode is "host" for a container on the host's network
		// stack.
		NetworkMode string `json:"NetworkMode"`
		// Binds are bind mounts as "source:destination[:options]".
		Binds []string `json:"Binds"`
		// PidMode is "host" for a container in the host's PID namespace.
		PidMode string `json:"PidMode"`
		// CapAdd are the capabilities added to the default set, such as
		// "NET_ADMIN" or "CAP_SYS_ADMIN".
		CapAdd  []string        `json:"CapAdd"`
		Devices []DeviceMapping `json:"Devices"`
		// DeviceRequests ask a device driver, such as NVIDIA's, for host
		// devices like GPUs.
		DeviceRequests []DeviceRequest `json:"DeviceRequests"`
		// IpcMode is "host" for a container in the host's IPC namespace.
		IpcMode string `json:"IpcMode"`
		// UTSMode is "host" for a container in the host's UTS namespace.
		UTSMode string `json:"UTSMode"`
		// Sysctls are the kernel parameters set in the container's
		// namespaces, such as "net.core.somaxconn".
		Sysctls map[string]string `json:"Sysctls"`
	} `json:"HostConfig"`
	Mounts          []Mount `json:"Mounts"`
	NetworkSettings struct {
		// Networks are keyed by network name.
		Networks map[string]EndpointSettings `json:"Networks"`
	} `json:"NetworkSettings"`
}

// Mount is a volume or bind mount of a container.
type Mount struct {
	// Type is "bind", "volume", "tmpfs", "npipe" or "cluster".
	Type string `json:"Type"`
	// Name is the volume's name, for a volume mount.
	Name string `json:"Name"`
	// Driver is the volume's driver, for a volume mount.
	Driver string `json:"Driver"`
	// Source is the host path: the volume's mountpoint for a volume
	// mount.
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
}

// DeviceRequest asks a device driver for host devices.
type DeviceRequest struct {
	Driver       string     `json:"Driver"`
	Count        int        `json:"Count"`
	DeviceIDs    []string   `json:"DeviceIDs"`
	Capabilities [][]string `json:"Capabilities"`
}

// Volume is a Docker volume, reduced to the fields the companion uses.
type Volume struct {
	Name   string `json:"Name"`
	Driver string `json:"Driver"`
	// Options are the driver's options. The local driver mounts Options
	// "device" with mount(8) options "o" and file system "type"; "o"
	// containing "bind" makes it a bind mount of the host path "device".
	Options    map[string]string `json:"Options"`
	Mountpoint string            `json:"Mountpoint"`
}

// DeviceMapping is a host device passed into a container.
type DeviceMapping struct {
	PathOnHost      string `json:"PathOnHost"`
	PathInContainer string `json:"PathInContainer"`
}

// EndpointSettings is a container's attachment to one network.
type EndpointSettings struct {
	NetworkID string `json:"NetworkID"`
	// EndpointID identifies this attachment; connecting the container to
	// the network again, or restarting it, creates a new one.
	EndpointID string `json:"EndpointID"`
	// IPAddress is the container's IPv4 address on the network.
	IPAddress string `json:"IPAddress"`
	// Aliases are the names the container has on the network, such as its
	// Compose service name.
	Aliases []string `json:"Aliases"`
	// DNSNames are every name that resolves to the container on the network
	// (API 1.45 and later).
	DNSNames []string `json:"DNSNames"`
}

// Network is a Docker network, reduced to the fields the companion uses.
type Network struct {
	ID     string            `json:"Id"`
	Name   string            `json:"Name"`
	Driver string            `json:"Driver"`
	Scope  string            `json:"Scope"`
	Labels map[string]string `json:"Labels"`
	// Attachable reports whether standalone containers may join a Swarm
	// overlay network.
	Attachable bool `json:"Attachable"`
	// Ingress reports the Swarm routing-mesh network.
	Ingress bool `json:"Ingress"`
}

// Service is a Swarm service, reduced to the fields the companion uses.
type Service struct {
	ID   string      `json:"ID"`
	Spec ServiceSpec `json:"Spec"`
}

// ServiceSpec is the specification of a Swarm service.
type ServiceSpec struct {
	Name string `json:"Name"`
	// Labels are the service's own labels; `docker stack deploy` sets
	// com.docker.stack.namespace on them.
	Labels       map[string]string `json:"Labels"`
	TaskTemplate TaskTemplate      `json:"TaskTemplate"`
}

// TaskTemplate is the template of a Swarm service's tasks.
type TaskTemplate struct {
	ContainerSpec ContainerSpec       `json:"ContainerSpec"`
	Networks      []NetworkAttachment `json:"Networks"`
}

// ContainerSpec is the container a Swarm service's tasks run.
type ContainerSpec struct {
	Mounts []ServiceMount `json:"Mounts"`
	// CapabilityAdd are the capabilities added to the default set.
	CapabilityAdd []string `json:"CapabilityAdd"`
	// Sysctls are the kernel parameters set in the tasks' namespaces.
	// Swarm has no device mappings or device requests to report.
	Sysctls map[string]string `json:"Sysctls"`
}

// ServiceMount is a mount of a Swarm service's containers.
type ServiceMount struct {
	// Type is "bind", "volume", "tmpfs", "npipe" or "cluster".
	Type string `json:"Type"`
	// Source is a host path for a bind mount, a volume name for a volume
	// mount.
	Source        string                `json:"Source"`
	Target        string                `json:"Target"`
	VolumeOptions *ServiceVolumeOptions `json:"VolumeOptions,omitempty"`
}

// ServiceVolumeOptions configure a volume mount of a Swarm service. Each
// node creates the volume from DriverConfig when it does not have it yet.
type ServiceVolumeOptions struct {
	DriverConfig *VolumeDriverConfig `json:"DriverConfig,omitempty"`
}

// VolumeDriverConfig is the driver, and its options, a Swarm service's
// volume is created with.
type VolumeDriverConfig struct {
	Name    string            `json:"Name"`
	Options map[string]string `json:"Options"`
}

// Task is a Swarm task: one replica of a service, reduced to the fields
// the companion uses.
type Task struct {
	ID           string `json:"ID"`
	ServiceID    string `json:"ServiceID"`
	NodeID       string `json:"NodeID"`
	DesiredState string `json:"DesiredState"`
	Status       struct {
		// State is "running" once the task's container runs.
		State           string `json:"State"`
		ContainerStatus struct {
			ContainerID string `json:"ContainerID"`
		} `json:"ContainerStatus"`
	} `json:"Status"`
	// NetworksAttachments are the networks the task is on, with its
	// addresses there.
	NetworksAttachments []TaskNetwork `json:"NetworksAttachments"`
}

// TaskNetwork is a task's attachment to one network.
type TaskNetwork struct {
	Network struct {
		ID string `json:"ID"`
	} `json:"Network"`
	// Addresses are in CIDR notation, such as "10.0.1.5/24".
	Addresses []string `json:"Addresses"`
}

// TaskListOptions select Swarm tasks to list.
type TaskListOptions struct {
	// Service is a service ID.
	Service string
	// DesiredState is "running", "shutdown" or "accepted".
	DesiredState string
}

// NetworkAttachment attaches a Swarm service to a network.
type NetworkAttachment struct {
	// Target is the network's ID or name.
	Target  string   `json:"Target"`
	Aliases []string `json:"Aliases"`
}

// ContainerConfig is what CreateContainer creates.
type ContainerConfig struct {
	Image      string            `json:"Image"`
	Entrypoint []string          `json:"Entrypoint,omitempty"`
	Cmd        []string          `json:"Cmd,omitempty"`
	User       string            `json:"User,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
	HostConfig HostConfig        `json:"HostConfig"`
}

// HostConfig is the host side of a ContainerConfig.
type HostConfig struct {
	// NetworkMode is the only network the container joins at creation.
	NetworkMode    string   `json:"NetworkMode,omitempty"`
	ReadonlyRootfs bool     `json:"ReadonlyRootfs,omitempty"`
	CapDrop        []string `json:"CapDrop,omitempty"`
	SecurityOpt    []string `json:"SecurityOpt,omitempty"`
	Init           *bool    `json:"Init,omitempty"`
	PidsLimit      *int64   `json:"PidsLimit,omitempty"`
	Memory         int64    `json:"Memory,omitempty"`
}

// ListOptions select containers to list.
type ListOptions struct {
	// All includes stopped containers.
	All bool
	// Labels are "key" or "key=value" filters, all of which must match.
	Labels []string
}

// ListContainers lists the containers matching opts.
func (c *Client) ListContainers(ctx context.Context, opts ListOptions) ([]ContainerSummary, error) {
	q := url.Values{}
	if opts.All {
		q.Set("all", "1")
	}
	if len(opts.Labels) > 0 {
		f, err := json.Marshal(map[string][]string{"label": opts.Labels})
		if err != nil {
			return nil, err
		}
		q.Set("filters", string(f))
	}
	var out []ContainerSummary
	if err := c.do(ctx, http.MethodGet, "/containers/json", q, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// InspectContainer returns the container with the given ID or name.
func (c *Client) InspectContainer(ctx context.Context, id string) (Container, error) {
	var out Container
	err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, nil, &out)
	return out, err
}

// InspectNetwork returns the network with the given ID or name.
func (c *Client) InspectNetwork(ctx context.Context, id string) (Network, error) {
	var out Network
	err := c.do(ctx, http.MethodGet, "/networks/"+url.PathEscape(id), nil, nil, &out)
	return out, err
}

// InspectVolume returns the volume with the given name.
func (c *Client) InspectVolume(ctx context.Context, name string) (Volume, error) {
	var out Volume
	err := c.do(ctx, http.MethodGet, "/volumes/"+url.PathEscape(name), nil, nil, &out)
	return out, err
}

// InspectService returns the Swarm service with the given ID or name.
func (c *Client) InspectService(ctx context.Context, id string) (Service, error) {
	var out Service
	err := c.do(ctx, http.MethodGet, "/services/"+url.PathEscape(id), nil, nil, &out)
	return out, err
}

// ListTasks lists the Swarm tasks matching opts. The daemon must be a
// Swarm manager.
func (c *Client) ListTasks(ctx context.Context, opts TaskListOptions) ([]Task, error) {
	filters := map[string][]string{}
	if opts.Service != "" {
		filters["service"] = []string{opts.Service}
	}
	if opts.DesiredState != "" {
		filters["desired-state"] = []string{opts.DesiredState}
	}
	q := url.Values{}
	if len(filters) > 0 {
		f, err := json.Marshal(filters)
		if err != nil {
			return nil, err
		}
		q.Set("filters", string(f))
	}
	var out []Task
	if err := c.do(ctx, http.MethodGet, "/tasks", q, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// InspectTask returns the Swarm task with the given ID. The daemon must
// be a Swarm manager.
func (c *Client) InspectTask(ctx context.Context, id string) (Task, error) {
	var out Task
	err := c.do(ctx, http.MethodGet, "/tasks/"+url.PathEscape(id), nil, nil, &out)
	return out, err
}

// CreateContainer creates a container named name and returns its ID.
func (c *Client) CreateContainer(ctx context.Context, name string, cfg ContainerConfig) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	q := url.Values{"name": {name}}
	if err := c.do(ctx, http.MethodPost, "/containers/create", q, cfg, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("docker: creating container %s: no ID returned", name)
	}
	return out.ID, nil
}

// StartContainer starts a created container.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil)
}

// RemoveContainer kills and removes a container. Its volumes are kept:
// repeaters have none, and a container removed by mistake must not take
// data with it.
func (c *Client) RemoveContainer(ctx context.Context, id string) error {
	q := url.Values{"force": {"1"}}
	return c.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id), q, nil, nil)
}

// PullImage pulls ref, such as "alpine/socat:1.8.1.1" or one pinned with
// "@sha256:...", and waits until the pull ends. A digest wins over a tag.
func (c *Client) PullImage(ctx context.Context, ref string) error {
	name, tag := splitReference(ref)
	q := url.Values{"fromImage": {name}, "tag": {tag}}
	req, err := c.newRequest(ctx, http.MethodPost, "/images/create", q, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("docker: pulling %s: %w", ref, err)
	}
	defer resp.Body.Close()
	if err := checkResponse(resp); err != nil {
		return err
	}
	// The daemon answers 200 at once and reports a failed pull inside the
	// progress stream.
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var msg struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) == nil && msg.Error != "" {
			return fmt.Errorf("docker: pulling %s: %s", ref, msg.Error)
		}
	}
	if err := sc.Err(); err != nil && err != io.EOF {
		return fmt.Errorf("docker: pulling %s: %w", ref, err)
	}
	return nil
}

// splitReference splits an image reference into the fromImage and tag
// parameters of a pull: the digest when there is one, else the tag.
func splitReference(ref string) (name, tag string) {
	if name, digest, ok := strings.Cut(ref, "@"); ok {
		if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
			name = name[:i]
		}
		return name, digest
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}
