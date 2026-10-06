//go:build integration

// Integration tests of the proxy in front of a real Docker daemon. They
// create and remove containers, so they only run in the integration
// workflow (.github/workflows/integration.yml), and only when
// DOKTUNNEL_INTEGRATION=1 as a second guard.

package dockerproxy_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/docker"
	"github.com/alebak/dokploy-tunnel/internal/dockerproxy"
	"github.com/alebak/dokploy-tunnel/internal/repeater"
)

// realHost is the runner's Docker daemon. The tests set up and clean up
// through it directly, and go through the proxy for what they check.
const realHost = docker.DefaultHost

// realChain is a proxy in front of the real daemon.
type realChain struct {
	*chain
	// admin talks to the daemon directly.
	admin *docker.Client
	// prefix is the API version prefix raw requests use.
	prefix string
	// bridge is the ID of the daemon's default bridge network.
	bridge string
}

func newRealChain(t *testing.T) *realChain {
	t.Helper()
	if os.Getenv("DOKTUNNEL_INTEGRATION") != "1" {
		t.Skip("set DOKTUNNEL_INTEGRATION=1 to run against a real Docker daemon")
	}
	admin, err := docker.New(realHost)
	if err != nil {
		t.Fatal(err)
	}
	pullCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := admin.PullImage(pullCtx, testImage); err != nil {
		t.Fatalf("pulling the repeater image: %v", err)
	}
	bridge, err := admin.InspectNetwork(testContext(t), "bridge")
	if err != nil {
		t.Fatal(err)
	}

	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	p, err := dockerproxy.New(realHost, testImage, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewUnstartedServer(nil)
	srv.Config = dockerproxy.NewDefaultServer(p, log)
	srv.Start()
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("proxy logs:\n%s", logs)
		}
	})
	client, err := docker.New("tcp://" + srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Ping(testContext(t)); err != nil {
		t.Fatalf("the daemon is not reachable through the proxy: %v", err)
	}
	return &realChain{
		chain:  &chain{base: srv.URL, client: client, logs: logs},
		admin:  admin,
		prefix: "/v" + client.APIVersion(),
		bridge: bridge.ID,
	}
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// realRepeaterConfig is repeaterConfig on the network network.
func realRepeaterConfig(network string) docker.ContainerConfig {
	cfg := repeaterConfig()
	cfg.HostConfig.NetworkMode = network
	cfg.Labels[repeater.LabelNetwork] = network
	return cfg
}

// realMutate is mutate on the network network.
func realMutate(network string, f func(cfg, host map[string]any)) map[string]any {
	return mutate(func(cfg, host map[string]any) {
		host["NetworkMode"] = network
		f(cfg, host)
	})
}

// removeOnCleanup removes the container id directly when the test ends.
func (c *realChain) removeOnCleanup(t *testing.T, id string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := c.admin.RemoveContainer(ctx, id); err != nil && !docker.IsNotFound(err) {
			t.Errorf("removing container %s: %v", id, err)
		}
	})
}

// expectDenied sends a request through the proxy and checks that the proxy
// refused it with a Docker error.
func (c *realChain) expectDenied(t *testing.T, method, path string, query url.Values, body any) {
	t.Helper()
	resp := rawRequest(t, c.chain, method, c.prefix+path, query, body)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("%s %s = %d %s, want 403", method, path, resp.StatusCode, b)
	}
	var msg struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(b, &msg); err != nil || !strings.HasPrefix(msg.Message, "doktunnel-socket-proxy: ") {
		t.Fatalf("%s %s: 403 not from the proxy: %s", method, path, b)
	}
	t.Logf("denied: %s", msg.Message)
}

// expectAbsent checks that no container is named name.
func (c *realChain) expectAbsent(t *testing.T, name string) {
	t.Helper()
	if _, err := c.admin.InspectContainer(testContext(t), name); !docker.IsNotFound(err) {
		t.Errorf("container %s: inspect error %v, want it absent", name, err)
	}
}

// TestIntegration_RepeaterLifecycle runs a repeater's lifecycle through the
// proxy, and checks that its execs are limited to socat.
func TestIntegration_RepeaterLifecycle(t *testing.T) {
	c := newRealChain(t)
	ctx := testContext(t)
	name := "doktunnel-repeater-" + randomSuffix(t)
	id, err := c.client.CreateContainer(ctx, name, realRepeaterConfig(c.bridge))
	if err != nil {
		t.Fatalf("creating a repeater through the proxy: %v", err)
	}
	c.removeOnCleanup(t, id)
	if err := c.client.StartContainer(ctx, id); err != nil {
		t.Fatalf("starting a repeater through the proxy: %v", err)
	}

	for _, cmd := range [][]string{
		{"sh"},
		{"sh", "-c", "id"},
		{"socat", "-d", "-d", "STDIO", "EXEC:/bin/sh"},
		{"socat", "-d", "-d", "STDIO", "TCP:127.0.0.1:80,connect-timeout=15,fork"},
	} {
		t.Run("exec "+strings.Join(cmd, " "), func(t *testing.T) {
			c.expectDenied(t, http.MethodPost, "/containers/"+id+"/exec", nil, execBody(cmd))
		})
	}
	t.Run("remove with volumes", func(t *testing.T) {
		c.expectDenied(t, http.MethodDelete, "/containers/"+id, url.Values{"force": {"1"}, "v": {"1"}}, nil)
	})

	resp := rawRequest(t, c.chain, http.MethodPost, c.prefix+"/containers/"+id+"/exec", nil, execBody(socatCmd("127.0.0.1:80")))
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("creating a socat exec = %d %s, want 201", resp.StatusCode, b)
	}
	if err := c.client.RemoveContainer(ctx, id); err != nil {
		t.Fatalf("removing a repeater through the proxy: %v", err)
	}
	c.expectAbsent(t, name)
}

func TestIntegration_DeniesUnsafeCreates(t *testing.T) {
	c := newRealChain(t)
	host, err := c.admin.InspectNetwork(testContext(t), "host")
	if err != nil {
		t.Fatal(err)
	}
	null, err := c.admin.InspectNetwork(testContext(t), "none")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		body any
	}{
		{"privileged", realMutate(c.bridge, func(_, h map[string]any) { h["Privileged"] = true })},
		{"with binds", realMutate(c.bridge, func(_, h map[string]any) { h["Binds"] = []string{"/:/host"} })},
		{"with a mount", realMutate(c.bridge, func(_, h map[string]any) {
			h["Mounts"] = []any{map[string]any{"Type": "bind", "Source": "/var/run/docker.sock", "Target": "/docker.sock"}}
		})},
		{"adding capabilities", realMutate(c.bridge, func(_, h map[string]any) { h["CapAdd"] = []string{"SYS_ADMIN"} })},
		{"on the host network", realMutate("host", func(_, _ map[string]any) {})},
		{"on the host network by ID", realMutate(host.ID, func(_, _ map[string]any) {})},
		{"on the null network by ID", realMutate(null.ID, func(_, _ map[string]any) {})},
		{"in the host's PID namespace", realMutate(c.bridge, func(_, h map[string]any) { h["PidMode"] = "host" })},
		{"with another image", realMutate(c.bridge, func(cfg, _ map[string]any) { cfg["Image"] = "alpine:latest" })},
		{"with another command", realMutate(c.bridge, func(cfg, _ map[string]any) { cfg["Cmd"] = []string{"sh", "-c", "id"} })},
		{"as root", realMutate(c.bridge, func(cfg, _ map[string]any) { cfg["User"] = "0" })},
		{"with seccomp off", realMutate(c.bridge, func(_, h map[string]any) {
			h["SecurityOpt"] = []string{"no-new-privileges", "seccomp=unconfined"}
		})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name := "doktunnel-repeater-" + randomSuffix(t)
			c.expectDenied(t, http.MethodPost, "/containers/create", url.Values{"name": {name}}, tt.body)
			c.expectAbsent(t, name)
		})
	}
	t.Run("not named as a repeater", func(t *testing.T) {
		name := "doktunnel-it-" + randomSuffix(t)
		c.expectDenied(t, http.MethodPost, "/containers/create", url.Values{"name": {name}}, realRepeaterConfig(c.bridge))
		c.expectAbsent(t, name)
	})
}

// TestIntegration_DeniesOtherContainers checks that containers the
// companion did not create cannot be started, removed, attached to or
// exec'd in, even one that copies a repeater's name, labels and image.
func TestIntegration_DeniesOtherContainers(t *testing.T) {
	c := newRealChain(t)
	ctx := testContext(t)
	create := func(name string, labels map[string]string) string {
		t.Helper()
		id, err := c.admin.CreateContainer(ctx, name, docker.ContainerConfig{
			Image:      testImage,
			Entrypoint: []string{"sleep"},
			Cmd:        []string{"infinity"},
			Labels:     labels,
			HostConfig: docker.HostConfig{NetworkMode: c.bridge},
		})
		if err != nil {
			t.Fatalf("creating %s directly: %v", name, err)
		}
		c.removeOnCleanup(t, id)
		return id
	}
	plain := create("doktunnel-it-plain-"+randomSuffix(t), nil)
	lookalikeLabels := realRepeaterConfig(c.bridge).Labels
	lookalike := create("doktunnel-repeater-"+randomSuffix(t), lookalikeLabels)
	if err := c.admin.StartContainer(ctx, lookalike); err != nil {
		t.Fatalf("starting the lookalike directly: %v", err)
	}

	for _, ctr := range []struct{ kind, id string }{{"plain", plain}, {"lookalike", lookalike}} {
		t.Run("start "+ctr.kind, func(t *testing.T) {
			c.expectDenied(t, http.MethodPost, "/containers/"+ctr.id+"/start", nil, nil)
		})
		t.Run("remove "+ctr.kind, func(t *testing.T) {
			c.expectDenied(t, http.MethodDelete, "/containers/"+ctr.id, url.Values{"force": {"1"}}, nil)
		})
		t.Run("exec in "+ctr.kind, func(t *testing.T) {
			c.expectDenied(t, http.MethodPost, "/containers/"+ctr.id+"/exec", nil, execBody(socatCmd("127.0.0.1:80")))
		})
		t.Run("attach to "+ctr.kind, func(t *testing.T) {
			c.expectDenied(t, http.MethodPost, "/containers/"+ctr.id+"/attach",
				url.Values{"stream": {"1"}, "stdin": {"1"}, "stdout": {"1"}}, nil)
		})
		t.Run("stop "+ctr.kind, func(t *testing.T) {
			c.expectDenied(t, http.MethodPost, "/containers/"+ctr.id+"/stop", nil, nil)
		})
	}

	if ctr, err := c.admin.InspectContainer(ctx, plain); err != nil || ctr.State.Running {
		t.Errorf("plain container after the denials: running %v, %v; want created and not started", ctr.State.Running, err)
	}
	if ctr, err := c.admin.InspectContainer(ctx, lookalike); err != nil || !ctr.State.Running {
		t.Errorf("lookalike after the denials: running %v, %v; want still running", ctr.State.Running, err)
	}
}

func TestIntegration_DeniesOtherEndpoints(t *testing.T) {
	c := newRealChain(t)
	network := "doktunnel-it-denied-" + randomSuffix(t)
	tests := []struct {
		name   string
		method string
		path   string
		query  url.Values
		body   any
	}{
		{"network create", http.MethodPost, "/networks/create", nil, map[string]any{"Name": network, "Driver": "bridge"}},
		{"secrets", http.MethodGet, "/secrets", nil, nil},
		{"configs", http.MethodGet, "/configs", nil, nil},
		{"version", http.MethodGet, "/version", nil, nil},
		{"image list", http.MethodGet, "/images/json", nil, nil},
		{"pull another image", http.MethodPost, "/images/create", url.Values{"fromImage": {"alpine"}, "tag": {"latest"}}, nil},
		{"service create", http.MethodPost, "/services/create", nil, map[string]any{"Name": "doktunnel-it-denied"}},
		{"swarm inspect", http.MethodGet, "/swarm", nil, nil},
		{"events", http.MethodGet, "/events", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c.expectDenied(t, tt.method, tt.path, tt.query, tt.body)
		})
	}
	if _, err := c.admin.InspectNetwork(testContext(t), network); !docker.IsNotFound(err) {
		t.Errorf("network %s: inspect error %v, want it absent", network, err)
	}
}
