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
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
		// ExposedPorts are keyed like "5432/tcp".
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
	} `json:"Config"`
	NetworkSettings struct {
		// Networks are keyed by network name.
		Networks map[string]EndpointSettings `json:"Networks"`
	} `json:"NetworkSettings"`
}

// EndpointSettings is a container's attachment to one network.
type EndpointSettings struct {
	NetworkID string `json:"NetworkID"`
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
	Name         string       `json:"Name"`
	TaskTemplate TaskTemplate `json:"TaskTemplate"`
}

// TaskTemplate is the template of a Swarm service's tasks.
type TaskTemplate struct {
	Networks []NetworkAttachment `json:"Networks"`
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

// InspectService returns the Swarm service with the given ID or name.
func (c *Client) InspectService(ctx context.Context, id string) (Service, error) {
	var out Service
	err := c.do(ctx, http.MethodGet, "/services/"+url.PathEscape(id), nil, nil, &out)
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

// RemoveContainer kills and removes a container with its anonymous volumes.
func (c *Client) RemoveContainer(ctx context.Context, id string) error {
	q := url.Values{"force": {"1"}, "v": {"1"}}
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
