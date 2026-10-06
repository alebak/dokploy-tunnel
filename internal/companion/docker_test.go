package companion

import (
	"bufio"
	"cmp"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/alebak/dokploy-tunnel/internal/docker"
	"github.com/alebak/dokploy-tunnel/internal/docker/dockertest"
	"github.com/alebak/dokploy-tunnel/internal/dokploy"
	"github.com/alebak/dokploy-tunnel/internal/repeater"
	"github.com/alebak/dokploy-tunnel/internal/tunnel"
)

func TestRepeaterTarget(t *testing.T) {
	tests := []struct {
		name   string
		target Target
		want   repeater.Target
	}{
		{
			"database",
			Target{Target: tunnel.Target{ServiceType: dokploy.ServicePostgres, ServiceID: "pg_main", Port: 5432},
				Service: dokploy.ServiceDetails{AppName: "shop-pg"}},
			repeater.Target{Kind: repeater.KindSwarmService, AppName: "shop-pg", Port: 5432},
		},
		{
			"application",
			Target{Target: tunnel.Target{ServiceType: dokploy.ServiceApplication, ServiceID: "app_web", Port: 3000},
				Service: dokploy.ServiceDetails{AppName: "shop-web"}},
			repeater.Target{Kind: repeater.KindSwarmService, AppName: "shop-web", Port: 3000},
		},
		{
			"docker-compose service",
			Target{Target: tunnel.Target{ServiceType: dokploy.ServiceCompose, ServiceID: "cmp_myapp", ComposeService: "postgres", Port: 5432},
				Service: dokploy.ServiceDetails{AppName: "myapp", ComposeType: "docker-compose"}},
			repeater.Target{Kind: repeater.KindCompose, AppName: "myapp", Service: "postgres", Port: 5432},
		},
		{
			"stack service",
			Target{Target: tunnel.Target{ServiceType: dokploy.ServiceCompose, ServiceID: "cmp_myapp", ComposeService: "postgres", Port: 5432},
				Service: dokploy.ServiceDetails{AppName: "myapp", ComposeType: "stack"}},
			repeater.Target{Kind: repeater.KindStack, AppName: "myapp", Service: "postgres", Port: 5432},
		},
		{
			"compose stack without a service is left to the repeater to refuse",
			Target{Target: tunnel.Target{ServiceType: dokploy.ServiceCompose, ServiceID: "cmp_myapp", Port: 80},
				Service: dokploy.ServiceDetails{AppName: "myapp"}},
			repeater.Target{Kind: repeater.KindCompose, AppName: "myapp", Port: 80},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := repeaterTarget(tt.target); got != tt.want {
				t.Errorf("repeaterTarget = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// dockerCompanion is a companion whose bridge runs repeaters on a fake
// Docker daemon. Compose stack cmp_myapp has its postgres service running
// on the project network at pgIP, reachable at targetAddr.
type dockerCompanion struct {
	fake   *dockertest.Fake
	bridge *DockerBridge
	http   *httptest.Server
	// targetAddr is where socat in a repeater connects for any address
	// in reachable.
	targetAddr string
	reachable  map[string]bool
}

// pgIP is the address of cmp_myapp's postgres service on its network.
const pgIP = "172.20.0.5"

// addSwarmService adds a Swarm service with one running task at addr on
// network netID.
func addSwarmService(fake *dockertest.Fake, id, name, netID, addr string) {
	fake.AddService(docker.Service{ID: id, Spec: docker.ServiceSpec{Name: name}})
	task := docker.Task{ID: "task-" + id, ServiceID: id}
	att := docker.TaskNetwork{Addresses: []string{addr + "/24"}}
	att.Network.ID = netID
	task.NetworksAttachments = []docker.TaskNetwork{att}
	fake.AddTask(task)
}

func newDockerCompanion(t *testing.T, targetAddr string, opts repeater.Options) *dockerCompanion {
	t.Helper()
	const project = "shop-cmpmyapp-a1b2c3" // the fake Dokploy's appName for cmp_myapp
	fake := dockertest.New(t)
	fake.AddImage(repeater.DefaultImage)
	fake.AddNetwork(docker.Network{ID: "net-proj", Name: project + "_default", Driver: "bridge", Scope: "local",
		Labels: map[string]string{"com.docker.compose.project": project}})
	fake.AddNetwork(docker.Network{ID: "net-closed", Name: "closed-net", Driver: "overlay", Scope: "swarm"})
	fake.AddNetwork(docker.Network{ID: "net-team", Name: "team-net", Driver: "overlay", Scope: "swarm", Attachable: true})
	fake.AddContainer(dockertest.Container{
		ID: "pg", Name: project + "-postgres-1", Running: true,
		Labels: map[string]string{
			"com.docker.compose.project":             project,
			"com.docker.compose.service":             "postgres",
			"com.docker.compose.project.working_dir": repeater.DefaultComposeDir + "/" + project + "/code",
		},
		Networks:     map[string][]string{project + "_default": {"postgres"}},
		IPs:          map[string]string{project + "_default": pgIP},
		ExposedPorts: []string{"5432/tcp"},
	})
	// pg_main runs only on a non-attachable overlay.
	addSwarmService(fake, "svc-pgmain", "shop-pgmain-a1b2c3", "net-closed", "10.0.4.5")
	dc := &dockerCompanion{fake: fake, targetAddr: targetAddr, reachable: map[string]bool{pgIP: true}}
	fake.ExecHandler = dockertest.Socat(func(host string) (string, bool) {
		return dc.targetAddr, dc.reachable[host]
	})

	client, err := docker.New(fake.Host())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	opts.Grace, opts.Log = cmp.Or(opts.Grace, time.Millisecond), log
	bridge := NewDockerBridge(client, opts)
	srv := NewServer(NewAuthorizer(newFakeDokploy(t).start(), ""), bridge, log, Limits{})
	hs := httptest.NewServer(srv)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Drain(ctx)
		hs.Close()
		if err := bridge.Close(ctx); err != nil {
			t.Errorf("closing the bridge: %v", err)
		}
	})
	dc.bridge, dc.http = bridge, hs
	return dc
}

func echoUpper(t *testing.T) string {
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
					io.WriteString(conn, strings.ToUpper(sc.Text())+"\n")
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func TestDockerBridge_TunnelToAComposeService(t *testing.T) {
	dc := newDockerCompanion(t, echoUpper(t), repeater.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := strings.Replace(dc.http.URL, "http", "ws", 1) + tunnel.Path +
		"?serviceType=compose_service&serviceId=cmp_myapp/postgres&port=5432"
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{tunnel.HeaderAPIKey: {ownerKey}}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	if err := c.Write(ctx, websocket.MessageBinary, []byte("select 1\n")); err != nil {
		t.Fatal(err)
	}
	_, msg, err := c.Read(ctx)
	if err != nil || string(msg) != "SELECT 1\n" {
		t.Fatalf("read %q, %v", msg, err)
	}
	c.Close(websocket.StatusNormalClosure, "")

	// The repeater goes once the last tunnel is gone.
	deadline := time.Now().Add(5 * time.Second)
	for len(dc.fake.ContainerIDs()) != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("repeater not removed: containers %q", dc.fake.ContainerIDs())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDockerBridge_Rejections(t *testing.T) {
	tests := []struct {
		name   string
		query  string
		status int
		code   tunnel.Code
	}{
		{"network not attachable", "serviceType=postgres&serviceId=pg_main&port=5432", http.StatusConflict, tunnel.CodeNetworkNotAttachable},
		{"nothing running", "serviceType=application&serviceId=app_web&port=3000", http.StatusBadGateway, tunnel.CodeTargetUnreachable},
		{"connection refused", "serviceType=compose_service&serviceId=cmp_myapp/postgres&port=5432", http.StatusBadGateway, tunnel.CodeTargetUnreachable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dc := newDockerCompanion(t, closedTCPAddr(t), repeater.Options{})
			resp := request(t, http.MethodGet, dc.http.URL+tunnel.Path+"?"+tt.query, ownerKey, upgradeHeaders())
			assertRejected(t, resp, tt.status, tt.code)
		})
	}
}

func TestDockerBridge_ExposedPorts(t *testing.T) {
	dc := newDockerCompanion(t, closedTCPAddr(t), repeater.Options{})
	dc.fake.AddService(docker.Service{ID: "svc-redis", Spec: docker.ServiceSpec{Name: "shop-redis"}})
	task := docker.Task{ID: "task-redis", ServiceID: "svc-redis"}
	task.Status.ContainerStatus.ContainerID = "redis"
	dc.fake.AddTask(task)
	dc.fake.AddContainer(dockertest.Container{
		ID: "redis", Name: "shop-redis.1.x", Running: true,
		Labels:       map[string]string{"com.docker.swarm.service.name": "shop-redis"},
		ExposedPorts: []string{"16379/tcp", "6379/udp", "6379/tcp", "8000-8010/tcp"},
	})
	got, err := dc.bridge.ExposedPorts(context.Background(), Target{
		Target:  tunnel.Target{ServiceType: dokploy.ServiceRedis, ServiceID: "redis_main"},
		Service: dokploy.ServiceDetails{AppName: "shop-redis"},
	})
	want := []tunnel.Port{{Port: 6379, Protocol: "tcp"}, {Port: 16379, Protocol: "tcp"}}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("ExposedPorts = %v, %v; want %v", got, err, want)
	}
}

func TestTCPPorts(t *testing.T) {
	got := tcpPorts([]repeater.Port{
		{Number: 80, Protocol: "tcp"}, {Number: 53, Protocol: "udp"}, {Number: 22, Protocol: "tcp"},
		{Number: 80, Protocol: "tcp"}, {Number: 9000, Protocol: "sctp"},
	})
	want := []tunnel.Port{{Port: 22, Protocol: "tcp"}, {Port: 80, Protocol: "tcp"}}
	if !slices.Equal(got, want) {
		t.Errorf("tcpPorts = %v, want %v", got, want)
	}
}

func TestDockerBridge_PortsEndpoint(t *testing.T) {
	tests := []struct {
		name   string
		query  string
		status int
		want   string
	}{
		{"compose service", "serviceType=compose_service&serviceId=cmp_myapp/postgres", 200,
			`{"ports":[{"port":5432,"protocol":"tcp"}]}`},
		// pg_main's task container is not on this node: its ports are unknown.
		{"Swarm task on another node", "serviceType=postgres&serviceId=pg_main", 200, `{"ports":[]}`},
		{"nothing running", "serviceType=application&serviceId=app_web", 502, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dc := newDockerCompanion(t, closedTCPAddr(t), repeater.Options{})
			before := len(dc.fake.ContainerIDs())
			resp := request(t, http.MethodGet, dc.http.URL+tunnel.PortsPath+"?"+tt.query, ownerKey, nil)
			if tt.status != http.StatusOK {
				assertRejected(t, resp, tt.status, tunnel.CodeTargetUnreachable)
			} else {
				raw, err := io.ReadAll(resp.Body)
				if err != nil || resp.StatusCode != tt.status || strings.TrimSpace(string(raw)) != tt.want {
					t.Errorf("HTTP %d %s, %v; want %d %s", resp.StatusCode, raw, err, tt.status, tt.want)
				}
			}
			if created, execs, pulls := dc.fake.Created(), dc.fake.Execs(), dc.fake.Pulls(); len(created)+len(execs)+len(pulls) != 0 {
				t.Errorf("listing ports created %v, ran %v, pulled %v", created, execs, pulls)
			}
			if after := len(dc.fake.ContainerIDs()); after != before {
				t.Errorf("containers went from %d to %d", before, after)
			}
		})
	}
}

func TestDockerBridge_RepeaterCapIsTooManyTunnels(t *testing.T) {
	dc := newDockerCompanion(t, echoUpper(t), repeater.Options{MaxRepeaters: 1})
	// app_web is reachable too, on its own network: a second repeater.
	addSwarmService(dc.fake, "svc-appweb", "shop-appweb-a1b2c3", "net-team", "10.0.2.9")
	dc.reachable["10.0.2.9"] = true

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := strings.Replace(dc.http.URL, "http", "ws", 1) + tunnel.Path +
		"?serviceType=compose_service&serviceId=cmp_myapp/postgres&port=5432"
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{tunnel.HeaderAPIKey: {ownerKey}}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	resp := request(t, http.MethodGet, dc.http.URL+tunnel.Path+"?serviceType=application&serviceId=app_web&port=3000", ownerKey, upgradeHeaders())
	assertRejected(t, resp, http.StatusTooManyRequests, codeTooManyTunnels)
}

func TestExposedDockerHost(t *testing.T) {
	for host, want := range map[string]bool{
		"unix:///var/run/docker.sock": false,
		"tcp://127.0.0.1:2375":        false,
		"tcp://[::1]:2375":            false,
		"tcp://localhost:2375":        false,
		"tcp://docker-proxy:2375":     true,
		"tcp://10.0.0.5:2375":         true,
	} {
		if got := exposedDockerHost(host); got != want {
			t.Errorf("exposedDockerHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func closedTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}
