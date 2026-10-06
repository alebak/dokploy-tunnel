// Package docker is a minimal client for the few Docker Engine API
// endpoints the companion needs: listing and inspecting containers,
// networks and Swarm services, creating and removing the repeater
// containers, and running commands in them with a hijacked exec stream.
//
// It uses the standard library only. The official Docker Go module would
// pull in dozens of dependencies for a dozen HTTP calls.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultHost is the Docker Engine socket used when no host is configured.
const DefaultHost = "unix:///var/run/docker.sock"

// MaxAPIVersion is the newest Engine API version the client speaks. The
// client uses the daemon's version when it is older; every endpoint used
// exists unchanged since well before it.
const MaxAPIVersion = "1.47"

const (
	// maxErrorBytes bounds the error body read from the daemon.
	maxErrorBytes = 64 << 10
	// maxResponseBytes bounds a JSON response; container and network
	// inspections are a few KiB.
	maxResponseBytes = 8 << 20
)

// APIError is an error answered by the Docker daemon.
type APIError struct {
	// StatusCode is the HTTP status of the response.
	StatusCode int
	// Message is the daemon's message.
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("docker: %s (HTTP %d)", e.Message, e.StatusCode)
}

// IsNotFound reports whether err is a daemon answer that the object, such
// as a container, image or network, does not exist.
func IsNotFound(err error) bool {
	return hasStatus(err, http.StatusNotFound)
}

// IsConflict reports whether err is a daemon answer that the object is in
// the wrong state, such as an exec in a stopped container.
func IsConflict(err error) bool {
	return hasStatus(err, http.StatusConflict)
}

func hasStatus(err error, status int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == status
}

// Client talks to one Docker daemon. It is safe for concurrent use.
type Client struct {
	// base is the scheme and host of request URLs.
	base string
	// dial opens a connection to the daemon.
	dial func(ctx context.Context) (net.Conn, error)
	http *http.Client

	mu sync.Mutex
	// version is the negotiated API version, empty until negotiated.
	version string

	// now is the clock of the data root cache.
	now func() time.Time
	// rootDirSem admits one caller of DockerRootDir at a time; holding it
	// guards rootDir and rootDirAt.
	rootDirSem chan struct{}
	// rootDir is the data root last read from the daemon, at rootDirAt;
	// empty until read.
	rootDir   string
	rootDirAt time.Time
}

// ParseHost parses a Docker host, such as "unix:///var/run/docker.sock" or
// "tcp://docker-proxy:2375", into the network ("unix" or "tcp") and address
// to dial. An empty host is DefaultHost.
func ParseHost(host string) (network, addr string, err error) {
	if host == "" {
		host = DefaultHost
	}
	u, err := url.Parse(host)
	if err != nil {
		return "", "", fmt.Errorf("parsing Docker host %q: %w", host, err)
	}
	switch u.Scheme {
	case "unix":
		if u.Path == "" {
			return "", "", fmt.Errorf("Docker host %q has no socket path", host)
		}
		return "unix", u.Path, nil
	case "tcp":
		if u.Port() == "" {
			return "", "", fmt.Errorf("Docker host %q has no port", host)
		}
		return "tcp", u.Host, nil
	default:
		return "", "", fmt.Errorf("unsupported Docker host %q: use unix:// or tcp://", host)
	}
}

// New returns a client for the daemon at host, such as
// "unix:///var/run/docker.sock" or "tcp://docker-proxy:2375" (plain HTTP,
// as a socket proxy serves it). An empty host is DefaultHost. Nothing is
// dialed until the first call.
func New(host string) (*Client, error) {
	network, addr, err := ParseHost(host)
	if err != nil {
		return nil, err
	}
	base := "http://docker"
	if network == "tcp" {
		base = "http://" + addr
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	dial := func(ctx context.Context) (net.Conn, error) {
		return dialer.DialContext(ctx, network, addr)
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dial(ctx)
		},
		MaxIdleConns:    8,
		IdleConnTimeout: 30 * time.Second,
	}
	return &Client{base: base, dial: dial, http: &http.Client{Transport: transport}, now: time.Now, rootDirSem: make(chan struct{}, 1)}, nil
}

// APIVersion returns the negotiated API version, or "" before the first
// call.
func (c *Client) APIVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version
}

// Ping checks that the daemon answers, and negotiates the API version.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.negotiate(ctx)
	return err
}

// negotiate returns the API version to use, asking the daemon once.
// Concurrent first calls may each ask; they agree.
func (c *Client) negotiate(ctx context.Context) (string, error) {
	if v := c.APIVersion(); v != "" {
		return v, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/_ping", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("docker: pinging the daemon: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBytes))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("docker: pinging the daemon: HTTP %d", resp.StatusCode)
	}
	server := resp.Header.Get("API-Version")
	if server == "" {
		return "", errors.New("docker: the daemon reported no API version")
	}
	v := minVersion(server, MaxAPIVersion)
	c.mu.Lock()
	c.version = v
	c.mu.Unlock()
	return v, nil
}

// minVersion returns the older of two "major.minor" API versions.
func minVersion(a, b string) string {
	if compareVersions(a, b) < 0 {
		return a
	}
	return b
}

func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(as), len(bs)); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// newRequest builds a versioned API request; body, when not nil, is sent
// as JSON.
func (c *Client) newRequest(ctx context.Context, method, path string, query url.Values, body any) (*http.Request, error) {
	version, err := c.negotiate(ctx)
	if err != nil {
		return nil, err
	}
	u := c.base + "/v" + version + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// do sends a request and decodes a JSON response into out, when not nil.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	req, err := c.newRequest(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("docker: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if err := checkResponse(resp); err != nil {
		return err
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBytes))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
		return fmt.Errorf("docker: decoding %s %s: %w", method, path, err)
	}
	return nil
}

// checkResponse turns a non-2xx response into an *APIError.
func checkResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	var msg struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &msg) != nil || msg.Message == "" {
		msg.Message = strings.TrimSpace(string(body))
	}
	if msg.Message == "" {
		msg.Message = http.StatusText(resp.StatusCode)
	}
	return &APIError{StatusCode: resp.StatusCode, Message: msg.Message}
}
