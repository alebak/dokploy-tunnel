package dockerproxy_test

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/docker"
	"github.com/alebak/dokploy-tunnel/internal/docker/dockertest"
	"github.com/alebak/dokploy-tunnel/internal/dockerproxy"
)

// TestProxy_RefusesRepeaterLookalikes checks that a container carrying a
// repeater's label, name and image, which a tenant can copy, is still not
// a repeater unless it is confined like one.
func TestProxy_RefusesRepeaterLookalikes(t *testing.T) {
	tests := []struct {
		name   string
		change func(c *dockertest.Container)
	}{
		{"privileged", func(c *dockertest.Container) { c.Privileged = true }},
		{"on the host network", func(c *dockertest.Container) { c.NetworkMode = "host" }},
		{"without a network", func(c *dockertest.Container) { c.NetworkMode = "none" }},
		{"in another container's network", func(c *dockertest.Container) { c.NetworkMode = "container:app1" }},
		{"keeping capabilities", func(c *dockertest.Container) { c.CapDrop = []string{"NET_RAW"} }},
		{"adding a capability", func(c *dockertest.Container) { c.CapAdd = []string{"NET_ADMIN"} }},
		{"with a writable root", func(c *dockertest.Container) { c.ReadonlyRootfs = false }},
		{"as root", func(c *dockertest.Container) { c.User = "0" }},
		{"as the image's user", func(c *dockertest.Container) { c.User = "" }},
		{"with a bind", func(c *dockertest.Container) { c.Binds = []string{"/:/host"} }},
		{"with a mount", func(c *dockertest.Container) {
			c.Mounts = []docker.Mount{{Type: "volume", Name: "data", Source: "/var/lib/docker/volumes/data/_data", Destination: "/data"}}
		}},
		{"allowing new privileges", func(c *dockertest.Container) { c.SecurityOpt = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newChain(t)
			c.fake.ExecHandler = echo
			look := repeaterContainer("copy1", "doktunnel-repeater-111111111111")
			tt.change(&look)
			c.fake.AddContainer(look)

			for _, req := range []struct {
				method, path string
				query        url.Values
				body         any
			}{
				{http.MethodPost, "/v1.47/containers/copy1/start", nil, nil},
				{http.MethodPost, "/v1.47/containers/copy1/exec", nil, execBody(socatCmd(appIP + ":5432"))},
				{http.MethodDelete, "/v1.47/containers/copy1", url.Values{"force": {"1"}}, nil},
			} {
				resp := rawRequest(t, c, req.method, req.path, req.query, req.body)
				if resp.StatusCode != http.StatusForbidden {
					b, _ := io.ReadAll(resp.Body)
					t.Errorf("%s %s = %d %s, want 403", req.method, req.path, resp.StatusCode, b)
				}
			}
			if len(c.fake.Removed()) != 0 || len(c.fake.Execs()) != 0 {
				t.Errorf("the daemon acted on a lookalike: removed %q, execs %d", c.fake.Removed(), len(c.fake.Execs()))
			}
		})
	}

	// The fixture itself passes, so the cases above fail for their change.
	t.Run("a repeater", func(t *testing.T) {
		c := newChain(t)
		c.fake.AddContainer(repeaterContainer("copy1", "doktunnel-repeater-111111111111"))
		if resp := rawRequest(t, c, http.MethodPost, "/v1.47/containers/copy1/start", nil, nil); resp.StatusCode != http.StatusNoContent {
			t.Errorf("start = %d, want 204", resp.StatusCode)
		}
		if resp := rawRequest(t, c, http.MethodPost, "/v1.47/containers/copy1/exec", nil, execBody(socatCmd(appIP+":5432"))); resp.StatusCode != http.StatusCreated {
			t.Errorf("exec create = %d, want 201", resp.StatusCode)
		}
		if resp := rawRequest(t, c, http.MethodDelete, "/v1.47/containers/copy1", url.Values{"force": {"1"}}, nil); resp.StatusCode != http.StatusNoContent {
			t.Errorf("remove = %d, want 204", resp.StatusCode)
		}
	})
}

func TestProxy_CreateChecksTheNetwork(t *testing.T) {
	hostNet := docker.Network{ID: "net-host", Name: "host", Driver: "host", Scope: "local"}
	nullNet := docker.Network{ID: "net-none", Name: "none", Driver: "null", Scope: "local"}
	name := url.Values{"name": {"doktunnel-repeater-dddddddddddd"}}

	t.Run("refuses the host's and the null network by ID", func(t *testing.T) {
		for _, n := range []docker.Network{hostNet, nullNet} {
			c := newChain(t)
			c.fake.AddImage(testImage)
			c.fake.AddNetwork(n)
			resp := rawRequest(t, c, http.MethodPost, "/v1.47/containers/create", name,
				mutate(func(_, h map[string]any) { h["NetworkMode"] = n.ID }))
			if resp.StatusCode != http.StatusForbidden {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("create on network %s (%s) = %d %s, want 403", n.ID, n.Driver, resp.StatusCode, b)
			}
			if got := c.fake.Created(); len(got) != 0 {
				t.Errorf("the daemon created %+v", got)
			}
		}
	})

	t.Run("passes a missing network's 404 through", func(t *testing.T) {
		c := newChain(t)
		c.fake.AddImage(testImage)
		cfg := repeaterConfig()
		cfg.HostConfig.NetworkMode = "net-missing"
		_, err := c.client.CreateContainer(testContext(t), "doktunnel-repeater-dddddddddddd", cfg)
		if !docker.IsNotFound(err) || !strings.Contains(err.Error(), "net-missing") {
			t.Errorf("create on a missing network = %v, want the daemon's 404", err)
		}
		if got := c.fake.Created(); len(got) != 0 {
			t.Errorf("the daemon created %+v", got)
		}
	})

	t.Run("creates on the network's full ID", func(t *testing.T) {
		c := newChain(t)
		c.fake.AddImage(testImage)
		cfg := repeaterConfig()
		cfg.HostConfig.NetworkMode = appNetwork.Name
		if _, err := c.client.CreateContainer(testContext(t), "doktunnel-repeater-dddddddddddd", cfg); err != nil {
			t.Fatalf("CreateContainer: %v", err)
		}
		if got := c.fake.Created(); len(got) != 1 || got[0].HostConfig.NetworkMode != appNetwork.ID {
			t.Errorf("created %+v, want network mode %s", got, appNetwork.ID)
		}
	})
}

// shortTimeouts serves the proxy with timeouts short enough to test.
func shortTimeouts(p *dockerproxy.Proxy, log *slog.Logger) *http.Server {
	return dockerproxy.NewServer(p, log, 200*time.Millisecond, 300*time.Millisecond, 300*time.Millisecond)
}

// waitForClose reads conn until it ends, and fails if the proxy keeps it
// open for seconds.
func waitForClose(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := io.Copy(io.Discard, conn)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("the proxy kept the connection open")
	}
}

func TestServer_Timeouts(t *testing.T) {
	t.Run("closes a connection whose body stalls", func(t *testing.T) {
		c := newChainServing(t, shortTimeouts)
		conn, err := net.Dial("tcp", strings.TrimPrefix(c.base, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "POST /v1.47/containers/create?name=doktunnel-repeater-eeeeeeeeeeee HTTP/1.1\r\n"+
			"Host: docker\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{")
		waitForClose(t, conn)
		if got := c.fake.Created(); len(got) != 0 {
			t.Errorf("the daemon created %+v", got)
		}
	})

	t.Run("closes an idle connection", func(t *testing.T) {
		c := newChainServing(t, shortTimeouts)
		conn, err := net.Dial("tcp", strings.TrimPrefix(c.base, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "GET /_ping HTTP/1.1\r\nHost: docker\r\n\r\n")
		waitForClose(t, conn)
	})

	t.Run("lets a slow daemon answer", func(t *testing.T) {
		c := newChainServing(t, shortTimeouts)
		c.fake.Intercept = func(_ http.ResponseWriter, r *http.Request) bool {
			if strings.HasSuffix(r.URL.Path, "/info") {
				time.Sleep(time.Second)
			}
			return false
		}
		resp := rawRequest(t, c, http.MethodGet, "/v1.47/info", nil, nil)
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("info = %d %s, want 200", resp.StatusCode, b)
		}
	})

	t.Run("keeps an exec stream open past every timeout", func(t *testing.T) {
		c := newChainServing(t, shortTimeouts)
		c.fake.ExecHandler = echo
		ex, err := c.client.Exec(testContext(t), repeaterID, socatCmd(appIP+":5432"), nil)
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		defer ex.Close()
		for _, part := range []string{"slow ", "client"} {
			time.Sleep(time.Second)
			if _, err := io.WriteString(ex, part); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
		if err := ex.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}
		got, err := io.ReadAll(ex)
		if err != nil || string(got) != "slow client" {
			t.Errorf("read %q, %v", got, err)
		}
	})
}
