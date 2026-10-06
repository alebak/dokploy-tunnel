package dockerproxy_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/docker"
	"github.com/alebak/dokploy-tunnel/internal/docker/dockertest"
	"github.com/alebak/dokploy-tunnel/internal/dockerproxy"
	"github.com/alebak/dokploy-tunnel/internal/repeater"
)

// testImage is the repeater image the proxy is configured with; pinned by
// digest like the default, so pulls go by digest.
const testImage = repeater.DefaultImage

// Objects of the fake daemon.
var (
	appNetwork  = docker.Network{ID: "net-app", Name: "myapp_default", Driver: "bridge", Scope: "local"}
	swarmClosed = docker.Network{ID: "net-closed", Name: "closed", Driver: "overlay", Scope: "swarm"}
)

const (
	// repeaterID is a repeater the companion created earlier.
	repeaterID = "rep1"
	// lookalikeID carries the repeater label and name but runs another
	// image.
	lookalikeID = "look1"
	// appID is a tenant's container.
	appID = "app1"
	// appIP is appID's address on appNetwork.
	appIP = "172.20.0.5"
)

// syncBuffer is a bytes.Buffer safe for concurrent use, for logs.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// chain is a docker.Client talking to a fake daemon through the proxy.
type chain struct {
	fake *dockertest.Fake
	// base is the proxy's URL, for raw requests.
	base   string
	client *docker.Client
	logs   *syncBuffer
}

func newChain(t *testing.T) *chain {
	t.Helper()
	fake := dockertest.New(t)
	fake.AddNetwork(appNetwork)
	fake.AddNetwork(swarmClosed)
	fake.AddContainer(dockertest.Container{
		ID: appID, Name: "myapp-postgres-1", Image: "postgres:17", Running: true,
		Networks: map[string][]string{appNetwork.Name: {"postgres"}},
		IPs:      map[string]string{appNetwork.Name: appIP},
	})
	fake.AddContainer(dockertest.Container{
		ID: repeaterID, Name: "doktunnel-repeater-0123456789ab", Image: testImage, Running: true,
		Labels:   repeaterLabels(),
		Networks: map[string][]string{appNetwork.Name: nil},
	})
	fake.AddContainer(dockertest.Container{
		ID: lookalikeID, Name: "doktunnel-repeater-ba9876543210", Image: "busybox", Running: true,
		Labels:   repeaterLabels(),
		Networks: map[string][]string{appNetwork.Name: nil},
	})

	logs := &syncBuffer{}
	p, err := dockerproxy.New(fake.Host(), testImage, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	client, err := docker.New("tcp://" + srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return &chain{fake: fake, base: srv.URL, client: client, logs: logs}
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// repeaterLabels are the labels the companion puts on a repeater.
func repeaterLabels() map[string]string {
	return map[string]string{
		repeater.LabelRepeater:  "1",
		repeater.LabelOwner:     "owner-1",
		repeater.LabelTarget:    "myapp/postgres",
		repeater.LabelNetwork:   appNetwork.Name,
		repeater.LabelCreatedAt: "2026-10-06T12:00:00Z",
		repeater.LabelOwnership: strings.Repeat("ab", 32),
	}
}

func ptr[T any](v T) *T { return &v }

// repeaterConfig is the container the companion creates for a repeater.
func repeaterConfig() docker.ContainerConfig {
	return docker.ContainerConfig{
		Image:      testImage,
		Entrypoint: []string{"sleep"},
		Cmd:        []string{"infinity"},
		User:       "65534:65534",
		Labels:     repeaterLabels(),
		HostConfig: docker.HostConfig{
			NetworkMode:    appNetwork.ID,
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			Init:           ptr(true),
			PidsLimit:      ptr(int64(256)),
			Memory:         64 << 20,
		},
	}
}

// socatCmd is the command the companion runs in a repeater to reach addr.
func socatCmd(addr string) []string {
	return []string{"socat", "-d", "-d", "STDIO", "TCP:" + addr + ",connect-timeout=15"}
}

// echo is an exec handler that copies stdin to stdout, then exits.
func echo(_ string, _ []string, stdin io.Reader, stdout, _ io.Writer) int {
	_, _ = io.Copy(stdout, stdin)
	return 0
}

func TestProxy_AllowsTheCompanionsReads(t *testing.T) {
	c := newChain(t)
	ctx := testContext(t)
	c.fake.AddVolume(docker.Volume{Name: "pgdata", Driver: "local"})
	c.fake.AddService(docker.Service{ID: "svc1", Spec: docker.ServiceSpec{Name: "myapp"}})
	c.fake.AddTask(docker.Task{ID: "task1", ServiceID: "svc1"})

	if err := c.client.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := c.client.APIVersion(); got != docker.MaxAPIVersion {
		t.Errorf("API version %q, want %q: the daemon's API-Version header must pass through", got, docker.MaxAPIVersion)
	}
	if info, err := c.client.Info(ctx); err != nil || info.DockerRootDir != "/var/lib/docker" {
		t.Errorf("Info = %+v, %v", info, err)
	}
	list, err := c.client.ListContainers(ctx, docker.ListOptions{All: true, Labels: []string{repeater.LabelRepeater + "=1"}})
	if err != nil || len(list) != 2 {
		t.Errorf("ListContainers = %d containers, %v; want 2", len(list), err)
	}
	if ctr, err := c.client.InspectContainer(ctx, appID); err != nil || ctr.ID != appID {
		t.Errorf("InspectContainer = %q, %v", ctr.ID, err)
	}
	if n, err := c.client.InspectNetwork(ctx, appNetwork.Name); err != nil || n.ID != appNetwork.ID {
		t.Errorf("InspectNetwork = %q, %v", n.ID, err)
	}
	if v, err := c.client.InspectVolume(ctx, "pgdata"); err != nil || v.Driver != "local" {
		t.Errorf("InspectVolume = %+v, %v", v, err)
	}
	if s, err := c.client.InspectService(ctx, "myapp"); err != nil || s.ID != "svc1" {
		t.Errorf("InspectService = %q, %v", s.ID, err)
	}
	tasks, err := c.client.ListTasks(ctx, docker.TaskListOptions{Service: "svc1", DesiredState: "running"})
	if err != nil || len(tasks) != 1 {
		t.Errorf("ListTasks = %d tasks, %v; want 1", len(tasks), err)
	}
	if task, err := c.client.InspectTask(ctx, "task1"); err != nil || task.ServiceID != "svc1" {
		t.Errorf("InspectTask = %+v, %v", task, err)
	}

	resp := rawRequest(t, c, http.MethodHead, "/_ping", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("HEAD /_ping = %d, want 200", resp.StatusCode)
	}
}

func TestProxy_RepeaterLifecycle(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "upgraded exec"
		if legacy {
			name = "exec answered with 200"
		}
		t.Run(name, func(t *testing.T) {
			c := newChain(t)
			ctx := testContext(t)
			c.fake.ExecHandler = echo
			c.fake.LegacyExecStart = legacy

			// The image is missing: the daemon's 404 must reach the client
			// unchanged, which then pulls and retries, as the companion does.
			_, err := c.client.CreateContainer(ctx, "doktunnel-repeater-aaaaaaaaaaaa", repeaterConfig())
			if !docker.IsNotFound(err) || !strings.Contains(strings.ToLower(err.Error()), "no such image") {
				t.Fatalf("first create = %v, want the daemon's 404 no such image", err)
			}
			if err := c.client.PullImage(ctx, testImage); err != nil {
				t.Fatalf("PullImage: %v", err)
			}
			if got := c.fake.Pulls(); len(got) != 1 || got[0] != "alpine/socat@"+strings.SplitN(testImage, "@", 2)[1] {
				t.Errorf("pulls = %q", got)
			}
			id, err := c.client.CreateContainer(ctx, "doktunnel-repeater-aaaaaaaaaaaa", repeaterConfig())
			if err != nil {
				t.Fatalf("CreateContainer: %v", err)
			}
			created := c.fake.Created()
			if len(created) != 1 || created[0].HostConfig.NetworkMode != appNetwork.ID || created[0].Image != testImage {
				t.Errorf("created %+v", created)
			}
			if err := c.client.StartContainer(ctx, id); err != nil {
				t.Fatalf("StartContainer: %v", err)
			}

			ex, err := c.client.Exec(ctx, id, socatCmd(appIP+":5432"), nil)
			if err != nil {
				t.Fatalf("Exec: %v", err)
			}
			defer ex.Close()
			if _, err := io.WriteString(ex, "hello through the proxy"); err != nil {
				t.Fatalf("write: %v", err)
			}
			if err := ex.CloseWrite(); err != nil {
				t.Fatalf("CloseWrite: %v", err)
			}
			got, err := io.ReadAll(ex)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != "hello through the proxy" {
				t.Errorf("read %q", got)
			}

			if err := c.client.RemoveContainer(ctx, id); err != nil {
				t.Fatalf("RemoveContainer: %v", err)
			}
			if got := c.fake.Removed(); len(got) != 1 || got[0] != id {
				t.Errorf("removed %q, want [%s]", got, id)
			}
			if n := c.fake.VolumeRemovals(); n != 0 {
				t.Errorf("%d removals asked to remove volumes", n)
			}
		})
	}
}

// upperServer answers each line with it upper-cased, and "BYE" once the
// client half-closes.
func upperServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				sc := bufio.NewScanner(conn)
				for sc.Scan() {
					if _, err := io.WriteString(conn, strings.ToUpper(sc.Text())+"\n"); err != nil {
						return
					}
				}
				_, _ = io.WriteString(conn, "BYE\n")
			}()
		}
	}()
	return ln.Addr().String()
}

// TestProxy_RunsTheCompanionsRepeater drives the companion's own repeater
// code through the proxy, so the allow-list cannot drift from the requests
// it really makes.
func TestProxy_RunsTheCompanionsRepeater(t *testing.T) {
	c := newChain(t)
	ctx := testContext(t)
	c.fake.AddImage(testImage)
	c.fake.AddContainer(dockertest.Container{
		ID: "pg1", Name: "web-postgres-1", Image: "postgres:17", Running: true,
		Labels: map[string]string{
			"com.docker.compose.project":              "web",
			"com.docker.compose.service":              "postgres",
			"com.docker.compose.project.working_dir":  repeater.DefaultComposeDir + "/web/code",
			"com.docker.compose.project.config_files": repeater.DefaultComposeDir + "/web/code/docker-compose.yml",
		},
		ExposedPorts: []string{"5432/tcp"},
		Networks:     map[string][]string{appNetwork.Name: {"postgres"}},
		IPs:          map[string]string{appNetwork.Name: "172.20.0.9"},
	})
	target := upperServer(t)
	c.fake.ExecHandler = dockertest.Socat(func(host string) (string, bool) {
		return target, host == "172.20.0.9"
	})

	r := repeater.New(c.client, repeater.Options{
		Image: testImage, Key: []byte("0123456789abcdef0123456789abcdef"),
		Log: slog.New(slog.DiscardHandler),
	})
	s, err := r.Open(ctx, repeater.Target{Kind: repeater.KindCompose, AppName: "web", Service: "postgres", Port: 5432})
	if err != nil {
		t.Fatalf("Open through the proxy: %v\nproxy log:\n%s", err, c.logs)
	}
	if _, err := io.WriteString(s, "select 1\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	got, err := io.ReadAll(s)
	s.Close()
	if err != nil || string(got) != "SELECT 1\nBYE\n" {
		t.Errorf("read %q, %v", got, err)
	}
	if _, err := r.Reap(ctx); err != nil {
		t.Errorf("Reap through the proxy: %v", err)
	}
	if err := r.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
	if len(c.fake.Removed()) != 1 {
		t.Errorf("removed %q, want the one repeater", c.fake.Removed())
	}
	if strings.Contains(c.logs.String(), "denied") {
		t.Errorf("the proxy denied a companion request:\n%s", c.logs)
	}
}

func TestProxy_PassesDaemonErrorsThrough(t *testing.T) {
	c := newChain(t)
	ctx := testContext(t)
	c.fake.AddImage(testImage)

	if _, err := c.client.InspectContainer(ctx, "missing"); !docker.IsNotFound(err) {
		t.Errorf("inspecting a missing container = %v, want 404", err)
	}
	if err := c.client.StartContainer(ctx, "missing"); !docker.IsNotFound(err) {
		t.Errorf("starting a missing container = %v, want 404", err)
	}
	if err := c.client.RemoveContainer(ctx, "missing"); !docker.IsNotFound(err) {
		t.Errorf("removing a missing container = %v, want 404", err)
	}

	cfg := repeaterConfig()
	cfg.HostConfig.NetworkMode = swarmClosed.ID
	_, err := c.client.CreateContainer(ctx, "doktunnel-repeater-bbbbbbbbbbbb", cfg)
	var apiErr *docker.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden || !strings.Contains(apiErr.Message, "attachable") {
		t.Errorf("create on a closed overlay = %v, want the daemon's 403 about attachable", err)
	}

	c.fake.StopContainer(repeaterID)
	if _, err := c.client.Exec(ctx, repeaterID, socatCmd(appIP+":5432"), nil); !docker.IsConflict(err) {
		t.Errorf("exec in a stopped repeater = %v, want 409", err)
	}
}

// rawRequest sends a request to the proxy as is.
func rawRequest(t *testing.T, c *chain, method, path string, query url.Values, body any) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		if s, ok := body.(string); ok {
			r = strings.NewReader(s)
		} else {
			b, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			r = bytes.NewReader(b)
		}
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(testContext(t), method, u, r)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// mutate returns the repeater's create body, as a JSON object, changed by
// f.
func mutate(f func(cfg map[string]any, host map[string]any)) map[string]any {
	b, _ := json.Marshal(repeaterConfig())
	var cfg map[string]any
	_ = json.Unmarshal(b, &cfg)
	f(cfg, cfg["HostConfig"].(map[string]any))
	return cfg
}

func execBody(cmd []string) map[string]any {
	return map[string]any{"AttachStdin": true, "AttachStdout": true, "AttachStderr": true, "Tty": false, "Cmd": cmd}
}

func TestProxy_Denies(t *testing.T) {
	repeaterName := url.Values{"name": {"doktunnel-repeater-cccccccccccc"}}
	tests := []struct {
		name   string
		method string
		path   string
		query  url.Values
		body   any
	}{
		{"container logs", http.MethodGet, "/v1.47/containers/app1/logs", nil, nil},
		{"container kill", http.MethodPost, "/v1.47/containers/app1/kill", nil, nil},
		{"container stop", http.MethodPost, "/containers/app1/stop", nil, nil},
		{"archive upload", http.MethodPut, "/v1.47/containers/rep1/archive", url.Values{"path": {"/"}}, nil},
		{"image list", http.MethodGet, "/v1.47/images/json", nil, nil},
		{"network removal", http.MethodDelete, "/v1.47/networks/net-app", nil, nil},
		{"network connect", http.MethodPost, "/v1.47/networks/net-app/connect", nil, map[string]string{"Container": "app1"}},
		{"build", http.MethodPost, "/v1.47/build", nil, nil},
		{"version", http.MethodGet, "/version", nil, nil},
		{"exec inspect", http.MethodGet, "/v1.47/exec/abc/json", nil, nil},
		{"wrong method on create", http.MethodGet, "/v1.47/containers/create", nil, nil},
		{"path traversal", http.MethodGet, "/v1.47/containers/../../x/json", nil, nil},
		{"escaped slash", http.MethodGet, "/v1.47/containers/a%2Fb/json", nil, nil},
		{"bad version prefix", http.MethodGet, "/v1/_ping", nil, nil},
		{"query on ping", http.MethodGet, "/_ping", url.Values{"x": {"1"}}, nil},
		{"query on info", http.MethodGet, "/v1.47/info", url.Values{"x": {"1"}}, nil},
		{"list with size", http.MethodGet, "/v1.47/containers/json", url.Values{"size": {"1"}}, nil},
		{"list with status filter", http.MethodGet, "/v1.47/containers/json", url.Values{"filters": {`{"status":["running"]}`}}, nil},
		{"list with malformed filter", http.MethodGet, "/v1.47/containers/json", url.Values{"filters": {`label=a`}}, nil},
		{"list with repeated all", http.MethodGet, "/v1.47/containers/json", url.Values{"all": {"1", "0"}}, nil},
		{"tasks by name", http.MethodGet, "/v1.47/tasks", url.Values{"filters": {`{"name":["x"]}`}}, nil},
		{"verbose network", http.MethodGet, "/v1.47/networks/net-app", url.Values{"verbose": {"true"}}, nil},
		{"inspect with size", http.MethodGet, "/v1.47/containers/app1/json", url.Values{"size": {"1"}}, nil},

		{"create privileged", http.MethodPost, "/v1.47/containers/create", repeaterName,
			mutate(func(_, h map[string]any) { h["Privileged"] = true })},
		{"create with binds", http.MethodPost, "/v1.47/containers/create", repeaterName,
			mutate(func(_, h map[string]any) { h["Binds"] = []string{"/:/host"} })},
		{"create adding capabilities", http.MethodPost, "/v1.47/containers/create", repeaterName,
			mutate(func(_, h map[string]any) { h["CapAdd"] = []string{"SYS_ADMIN"} })},
		{"create with devices", http.MethodPost, "/v1.47/containers/create", repeaterName,
			mutate(func(_, h map[string]any) { h["Devices"] = []any{} })},
		{"create with env", http.MethodPost, "/v1.47/containers/create", repeaterName,
			mutate(func(c, _ map[string]any) { c["Env"] = []string{"A=b"} })},
		{"create with another image", http.MethodPost, "/v1.47/containers/create", repeaterName,
			mutate(func(c, _ map[string]any) { c["Image"] = "alpine:latest" })},
		{"create with another command", http.MethodPost, "/v1.47/containers/create", repeaterName,
			mutate(func(c, _ map[string]any) { c["Cmd"] = []string{"sh", "-c", "id"} })},
		{"create with another entrypoint", http.MethodPost, "/v1.47/containers/create", repeaterName,
			mutate(func(c, _ map[string]any) { c["Entrypoint"] = []string{"socat"} })},
		{"create as root", http.MethodPost, "/v1.47/containers/create", repeaterName,
			mutate(func(c, _ map[string]any) { c["User"] = "0" })},
		{"create without the repeater label", http.MethodPost, "/v1.47/containers/create", repeaterName,
			mutate(func(c, _ map[string]any) { delete(c["Labels"].(map[string]any), repeater.LabelRepeater) })},
		{"create without the ownership label", http.MethodPost, "/v1.47/containers/create", repeaterName,
			mutate(func(c, _ map[string]any) { delete(c["Labels"].(map[string]any), repeater.LabelOwnership) })},
		{"create with a Traefik label", http.MethodPost, "/v1.47/containers/create", repeaterName,
			mutate(func(c, _ map[string]any) { c["Labels"].(map[string]any)["traefik.enable"] = "true" })},
		{"create on the host network", http.MethodPost, "/v1.47/containers/create", repeaterName,
			mutate(func(_, h map[string]any) { h["NetworkMode"] = "host" })},
		{"create in another container's network", http.MethodPost, "/v1.47/containers/create", repeaterName,
			mutate(func(_, h map[string]any) { h["NetworkMode"] = "container:app1" })},
		{"create without a network", http.MethodPost, "/v1.47/containers/create", repeaterName,
			mutate(func(_, h map[string]any) { delete(h, "NetworkMode") })},
		{"create with a writable root", http.MethodPost, "/v1.47/containers/create", repeaterName,
			mutate(func(_, h map[string]any) { delete(h, "ReadonlyRootfs") })},
		{"create keeping capabilities", http.MethodPost, "/v1.47/containers/create", repeaterName,
			mutate(func(_, h map[string]any) { h["CapDrop"] = []string{"NET_RAW"} })},
		{"create with seccomp off", http.MethodPost, "/v1.47/containers/create", repeaterName,
			mutate(func(_, h map[string]any) { h["SecurityOpt"] = []string{"no-new-privileges", "seccomp=unconfined"} })},
		{"create without init", http.MethodPost, "/v1.47/containers/create", repeaterName,
			mutate(func(_, h map[string]any) { h["Init"] = false })},
		{"create with more processes", http.MethodPost, "/v1.47/containers/create", repeaterName,
			mutate(func(_, h map[string]any) { h["PidsLimit"] = 100000 })},
		{"create without a memory limit", http.MethodPost, "/v1.47/containers/create", repeaterName,
			mutate(func(_, h map[string]any) { delete(h, "Memory") })},
		{"create with more memory", http.MethodPost, "/v1.47/containers/create", repeaterName,
			mutate(func(_, h map[string]any) { h["Memory"] = 1 << 30 })},
		{"create with a trailing object", http.MethodPost, "/v1.47/containers/create", repeaterName,
			`{"Image":"x"} {"Image":"y"}`},
		{"create with another name", http.MethodPost, "/v1.47/containers/create", url.Values{"name": {"web"}}, repeaterConfig()},
		{"create without a name", http.MethodPost, "/v1.47/containers/create", nil, repeaterConfig()},
		{"create on another platform", http.MethodPost, "/v1.47/containers/create",
			url.Values{"name": {"doktunnel-repeater-cccccccccccc"}, "platform": {"linux/arm64"}}, repeaterConfig()},

		{"pull another image", http.MethodPost, "/v1.47/images/create", url.Values{"fromImage": {"alpine"}, "tag": {"latest"}}, nil},
		{"pull the repeater by tag", http.MethodPost, "/v1.47/images/create", url.Values{"fromImage": {"alpine/socat"}, "tag": {"1.8.1.1"}}, nil},
		{"import an image", http.MethodPost, "/v1.47/images/create", url.Values{"fromSrc": {"http://example.com/x.tar"}}, nil},

		{"start a tenant container", http.MethodPost, "/v1.47/containers/app1/start", nil, nil},
		{"start a lookalike", http.MethodPost, "/v1.47/containers/look1/start", nil, nil},
		{"remove a tenant container", http.MethodDelete, "/v1.47/containers/app1", url.Values{"force": {"1"}}, nil},
		{"remove a lookalike", http.MethodDelete, "/v1.47/containers/look1", url.Values{"force": {"1"}}, nil},
		{"remove a repeater with its volumes", http.MethodDelete, "/v1.47/containers/rep1", url.Values{"force": {"1"}, "v": {"1"}}, nil},
		{"remove a repeater's links", http.MethodDelete, "/v1.47/containers/rep1", url.Values{"link": {"1"}}, nil},

		{"exec a shell", http.MethodPost, "/v1.47/containers/rep1/exec", nil, execBody([]string{"sh"})},
		{"exec socat with extra options", http.MethodPost, "/v1.47/containers/rep1/exec", nil,
			execBody([]string{"socat", "-d", "-d", "STDIO", "TCP:172.20.0.5:5432,connect-timeout=15,fork"})},
		{"exec socat to a name", http.MethodPost, "/v1.47/containers/rep1/exec", nil, execBody(socatCmd("postgres:5432"))},
		{"exec socat with a zone", http.MethodPost, "/v1.47/containers/rep1/exec", nil, execBody(socatCmd("[fe80::1%eth0]:5432"))},
		{"exec socat to port 0", http.MethodPost, "/v1.47/containers/rep1/exec", nil, execBody(socatCmd(appIP + ":0"))},
		{"exec socat to EXEC", http.MethodPost, "/v1.47/containers/rep1/exec", nil,
			execBody([]string{"socat", "-d", "-d", "STDIO", "EXEC:/bin/sh"})},
		{"exec with a TTY", http.MethodPost, "/v1.47/containers/rep1/exec", nil,
			map[string]any{"AttachStdin": true, "AttachStdout": true, "AttachStderr": true, "Tty": true, "Cmd": socatCmd(appIP + ":5432")}},
		{"exec as root", http.MethodPost, "/v1.47/containers/rep1/exec", nil,
			map[string]any{"AttachStdin": true, "AttachStdout": true, "AttachStderr": true, "Cmd": socatCmd(appIP + ":5432"), "User": "0"}},
		{"exec privileged", http.MethodPost, "/v1.47/containers/rep1/exec", nil,
			map[string]any{"AttachStdin": true, "AttachStdout": true, "AttachStderr": true, "Cmd": socatCmd(appIP + ":5432"), "Privileged": true}},
		{"exec in a tenant container", http.MethodPost, "/v1.47/containers/app1/exec", nil, execBody(socatCmd(appIP + ":5432"))},
		{"exec in a lookalike", http.MethodPost, "/v1.47/containers/look1/exec", nil, execBody(socatCmd(appIP + ":5432"))},
		{"start an exec without an upgrade", http.MethodPost, "/v1.47/exec/0123abcd/start", nil, map[string]bool{"Detach": false, "Tty": false}},
		{"list with labels and size", http.MethodGet, "/v1.47/containers/json", url.Values{"filters": {`{"label":["a=b"]}`}, "size": {"1"}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newChain(t)
			c.fake.AddImage(testImage)
			c.fake.ExecHandler = echo
			resp := rawRequest(t, c, tt.method, tt.path, tt.query, tt.body)
			if resp.StatusCode != http.StatusForbidden {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("%s %s = %d %s, want 403", tt.method, tt.path, resp.StatusCode, b)
			}
			var msg struct {
				Message string `json:"message"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil || msg.Message == "" {
				t.Errorf("body is not a Docker error: %v", err)
			}
			t.Logf("denied: %s", msg.Message)
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type %q", ct)
			}
			if n := len(c.fake.Created()) + len(c.fake.Removed()) + len(c.fake.Execs()) + len(c.fake.Pulls()); n != 0 {
				t.Errorf("the daemon acted on a denied request: created %d, removed %q, execs %d, pulls %q",
					len(c.fake.Created()), c.fake.Removed(), len(c.fake.Execs()), c.fake.Pulls())
			}
			if !strings.Contains(c.logs.String(), "denied") {
				t.Errorf("the denial was not logged:\n%s", c.logs)
			}
		})
	}
}

func TestProxy_ExecStartIsValidated(t *testing.T) {
	c := newChain(t)
	c.fake.ExecHandler = echo
	resp := rawRequest(t, c, http.MethodPost, "/v1.47/containers/rep1/exec", nil, execBody(socatCmd(appIP+":5432")))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("exec create = %d, want 201", resp.StatusCode)
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil || created.ID == "" {
		t.Fatalf("exec create body: %v", err)
	}

	for _, body := range []any{
		map[string]bool{"Detach": true, "Tty": false},
		map[string]bool{"Detach": false, "Tty": true},
		map[string]any{"Detach": false, "Tty": false, "ConsoleSize": []int{1, 1}},
	} {
		resp := rawRequest(t, c, http.MethodPost, "/v1.47/exec/"+created.ID+"/start", nil, body)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("exec start with %v = %d, want 403", body, resp.StatusCode)
		}
	}
	// Without an upgrade the stream could not carry stdin.
	resp = rawRequest(t, c, http.MethodPost, "/v1.47/exec/"+created.ID+"/start", nil, map[string]bool{"Detach": false, "Tty": false})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("exec start without an upgrade = %d, want 403", resp.StatusCode)
	}
}

func TestProxy_ExecCanStartOnlyOnce(t *testing.T) {
	c := newChain(t)
	ctx := testContext(t)
	var mu sync.Mutex
	var ids []string
	c.fake.Intercept = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/start") && strings.Contains(r.URL.Path, "/exec/") {
			mu.Lock()
			ids = append(ids, strings.Split(r.URL.Path, "/")[3])
			mu.Unlock()
		}
		return false
	}
	ex, err := c.client.Exec(ctx, repeaterID, socatCmd(appIP+":5432"), nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	_, _ = io.ReadAll(ex)
	ex.Close()

	mu.Lock()
	started := ids[0]
	mu.Unlock()
	for _, id := range []string{started, "0123abcd"} {
		if status := upgradedExecStart(t, c, id); status != http.StatusForbidden {
			t.Errorf("start of exec %s = %d, want 403", id, status)
		}
	}
	if !strings.Contains(c.logs.String(), "was not created through the proxy, or was already started") {
		t.Errorf("the denials were not logged:\n%s", c.logs)
	}
}

// upgradedExecStart starts exec id through the proxy as the client does,
// and returns the status of the answer.
func upgradedExecStart(t *testing.T, c *chain, id string) int {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(c.base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body := `{"Detach":false,"Tty":false}`
	_, _ = io.WriteString(conn, "POST /v1.47/exec/"+id+"/start HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
