package companion

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
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
// on the project network, reachable at targetAddr.
type dockerCompanion struct {
	fake   *dockertest.Fake
	bridge *DockerBridge
	http   *httptest.Server
}

func newDockerCompanion(t *testing.T, targetAddr string) *dockerCompanion {
	t.Helper()
	const project = "shop-cmpmyapp-a1b2c3" // the fake Dokploy's appName for cmp_myapp
	fake := dockertest.New(t)
	fake.AddImage(repeater.DefaultImage)
	fake.AddNetwork(docker.Network{ID: "net-proj", Name: project + "_default", Driver: "bridge", Scope: "local",
		Labels: map[string]string{"com.docker.compose.project": project}})
	fake.AddNetwork(docker.Network{ID: "net-closed", Name: "closed-net", Driver: "overlay", Scope: "swarm"})
	fake.AddContainer(dockertest.Container{
		ID: "pg", Name: project + "-postgres-1", Running: true,
		Labels:   map[string]string{"com.docker.compose.project": project, "com.docker.compose.service": "postgres"},
		Networks: map[string][]string{project + "_default": {"postgres"}},
	})
	// pg_main runs only on a non-attachable overlay.
	fake.AddContainer(dockertest.Container{
		ID: "pgmain", Name: "shop-pgmain-a1b2c3.1.x", Running: true,
		Labels:   map[string]string{"com.docker.swarm.service.name": "shop-pgmain-a1b2c3"},
		Networks: map[string][]string{"closed-net": nil},
	})
	fake.ExecHandler = dockertest.Socat(func(host string) (string, bool) {
		return targetAddr, host == "postgres"
	})

	client, err := docker.New(fake.Host())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	bridge := NewDockerBridge(client, repeater.Options{Grace: time.Millisecond, Log: log})
	srv := NewServer(NewAuthorizer(newFakeDokploy(t).start(), ""), bridge, log)
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
	return &dockerCompanion{fake: fake, bridge: bridge, http: hs}
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
	dc := newDockerCompanion(t, echoUpper(t))
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
			dc := newDockerCompanion(t, closedTCPAddr(t))
			resp := request(t, http.MethodGet, dc.http.URL+tunnel.Path+"?"+tt.query, ownerKey, upgradeHeaders())
			assertRejected(t, resp, tt.status, tt.code)
		})
	}
}

func TestDockerBridge_ExposedPorts(t *testing.T) {
	dc := newDockerCompanion(t, closedTCPAddr(t))
	dc.fake.AddContainer(dockertest.Container{
		ID: "redis", Name: "shop-redis-1", Running: true,
		Labels:       map[string]string{"com.docker.swarm.service.name": "shop-redis"},
		ExposedPorts: []string{"6379/tcp"},
	})
	got, err := dc.bridge.ExposedPorts(context.Background(), Target{
		Target:  tunnel.Target{ServiceType: dokploy.ServiceRedis, ServiceID: "redis_main"},
		Service: dokploy.ServiceDetails{AppName: "shop-redis"},
	})
	if err != nil || len(got) != 1 || got[0] != (repeater.Port{Number: 6379, Protocol: "tcp"}) {
		t.Errorf("ExposedPorts = %v, %v", got, err)
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
