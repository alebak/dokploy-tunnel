package repeater

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/docker"
	"github.com/alebak/dokploy-tunnel/internal/docker/dockertest"
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newClient(t *testing.T, fake *dockertest.Fake) *docker.Client {
	t.Helper()
	c, err := docker.New(fake.Host())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Networks as Dokploy creates them.
var (
	dokployNetwork = docker.Network{ID: "net-dokploy", Name: "dokploy-network", Driver: "overlay", Scope: "swarm", Attachable: true}
	ingress        = docker.Network{ID: "net-ingress", Name: "ingress", Driver: "overlay", Scope: "swarm", Ingress: true}
	defaultBridge  = docker.Network{ID: "net-bridge", Name: "bridge", Driver: "bridge", Scope: "local"}
	// composeDefault is the network docker compose creates for project
	// myapp.
	composeDefault = docker.Network{ID: "net-myapp", Name: "myapp_default", Driver: "bridge", Scope: "local",
		Labels: map[string]string{"com.docker.compose.project": "myapp", "com.docker.compose.network": "default"}}
	// stackNetwork is the overlay docker stack deploy creates for stack
	// mystack.
	stackNetwork = docker.Network{ID: "net-mystack", Name: "mystack_default", Driver: "overlay", Scope: "swarm", Attachable: true,
		Labels: map[string]string{"com.docker.stack.namespace": "mystack"}}
	customAttachable = docker.Network{ID: "net-team", Name: "team-net", Driver: "overlay", Scope: "swarm", Attachable: true}
	customClosed     = docker.Network{ID: "net-closed", Name: "closed-net", Driver: "overlay", Scope: "swarm"}
)

func newFake(t *testing.T) *dockertest.Fake {
	t.Helper()
	fake := dockertest.New(t)
	for _, n := range []docker.Network{dokployNetwork, ingress, defaultBridge, composeDefault, stackNetwork, customAttachable, customClosed} {
		fake.AddNetwork(n)
	}
	return fake
}

func swarmLabels(service string) map[string]string {
	return map[string]string{"com.docker.swarm.service.name": service, "com.docker.swarm.task.id": "t1"}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name        string
		containers  []dockertest.Container
		services    []docker.Service
		target      Target
		wantHost    string
		wantNetwork string
		wantPorts   []Port
		wantErr     error
	}{
		{
			name: "application on dokploy-network",
			containers: []dockertest.Container{{
				ID: "c1", Name: "myapp-web.1.abc", Running: true, Labels: swarmLabels("myapp-web"),
				ExposedPorts: []string{"3000/tcp", "9090/udp"},
				Networks:     map[string][]string{"ingress": nil, "dokploy-network": {"c1short"}},
			}},
			target:      Target{Kind: KindSwarmService, AppName: "myapp-web"},
			wantHost:    "myapp-web",
			wantNetwork: "dokploy-network",
			wantPorts:   []Port{{3000, "tcp"}, {9090, "udp"}},
		},
		{
			name: "database on a custom network prefers it over dokploy-network",
			containers: []dockertest.Container{{
				ID: "c1", Name: "myapp-db.1.abc", Running: true, Labels: swarmLabels("myapp-db"),
				ExposedPorts: []string{"5432/tcp"},
				Networks:     map[string][]string{"dokploy-network": nil, "team-net": nil},
			}},
			target:      Target{Kind: KindSwarmService, AppName: "myapp-db"},
			wantHost:    "myapp-db",
			wantNetwork: "team-net",
			wantPorts:   []Port{{5432, "tcp"}},
		},
		{
			name: "compose service on its project network by service name",
			containers: []dockertest.Container{{
				ID: "c1", Name: "myapp-postgres-1", Running: true,
				Labels:       map[string]string{"com.docker.compose.project": "myapp", "com.docker.compose.service": "postgres"},
				ExposedPorts: []string{"5432/tcp"},
				Networks:     map[string][]string{"dokploy-network": {"postgres"}, "myapp_default": {"postgres", "myapp-postgres-1"}},
			}},
			target:      Target{Kind: KindCompose, AppName: "myapp", Service: "postgres"},
			wantHost:    "postgres",
			wantNetwork: "myapp_default",
			wantPorts:   []Port{{5432, "tcp"}},
		},
		{
			name: "compose service only on a shared network by container name",
			containers: []dockertest.Container{{
				ID: "c1", Name: "myapp-postgres-1", Running: true,
				Labels:   map[string]string{"com.docker.compose.project": "myapp", "com.docker.compose.service": "postgres"},
				Networks: map[string][]string{"dokploy-network": {"postgres"}},
			}},
			target:      Target{Kind: KindCompose, AppName: "myapp", Service: "postgres"},
			wantHost:    "myapp-postgres-1",
			wantNetwork: "dokploy-network",
		},
		{
			name: "compose service of another project is not matched",
			containers: []dockertest.Container{{
				ID: "c1", Name: "other-postgres-1", Running: true,
				Labels:   map[string]string{"com.docker.compose.project": "other", "com.docker.compose.service": "postgres"},
				Networks: map[string][]string{"dokploy-network": nil},
			}},
			target:  Target{Kind: KindCompose, AppName: "myapp", Service: "postgres"},
			wantErr: ErrTargetUnreachable,
		},
		{
			name: "stack service by its Swarm service name",
			containers: []dockertest.Container{{
				ID: "c1", Name: "mystack_postgres.1.abc", Running: true,
				Labels: map[string]string{"com.docker.stack.namespace": "mystack", "com.docker.swarm.service.name": "mystack_postgres"},
				Networks: map[string][]string{
					"dokploy-network": nil, "mystack_default": {"postgres"},
				},
			}},
			target:      Target{Kind: KindStack, AppName: "mystack", Service: "postgres"},
			wantHost:    "mystack_postgres",
			wantNetwork: "mystack_default",
		},
		{
			name: "Swarm service without a local container",
			services: []docker.Service{{ID: "s1", Spec: docker.ServiceSpec{Name: "myapp-web",
				TaskTemplate: docker.TaskTemplate{Networks: []docker.NetworkAttachment{{Target: "net-dokploy"}}}}}},
			target:      Target{Kind: KindSwarmService, AppName: "myapp-web"},
			wantHost:    "myapp-web",
			wantNetwork: "dokploy-network",
		},
		{
			name: "only non-attachable overlays",
			containers: []dockertest.Container{{
				ID: "c1", Name: "myapp-db.1.abc", Running: true, Labels: swarmLabels("myapp-db"),
				Networks: map[string][]string{"ingress": nil, "closed-net": nil},
			}},
			target:  Target{Kind: KindSwarmService, AppName: "myapp-db"},
			wantErr: ErrNetworkNotAttachable,
		},
		{
			name: "only the default bridge, which has no DNS",
			containers: []dockertest.Container{{
				ID: "c1", Name: "myapp-postgres-1", Running: true,
				Labels:   map[string]string{"com.docker.compose.project": "myapp", "com.docker.compose.service": "postgres"},
				Networks: map[string][]string{"bridge": nil},
			}},
			target:  Target{Kind: KindCompose, AppName: "myapp", Service: "postgres"},
			wantErr: ErrTargetUnreachable,
		},
		{
			name:    "nothing running",
			target:  Target{Kind: KindSwarmService, AppName: "myapp-web"},
			wantErr: ErrTargetUnreachable,
		},
		{
			name:    "compose stack without a service name",
			target:  Target{Kind: KindCompose, AppName: "myapp"},
			wantErr: ErrTargetUnreachable,
		},
		{
			name:    "app name unsafe for a socat address",
			target:  Target{Kind: KindSwarmService, AppName: "myapp,fork"},
			wantErr: ErrTargetUnreachable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFake(t)
			for _, c := range tt.containers {
				fake.AddContainer(c)
			}
			for _, s := range tt.services {
				fake.AddService(s)
			}
			got, err := Resolve(testContext(t), newClient(t, fake), tt.target)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Resolve error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.Host != tt.wantHost || got.Network.Name != tt.wantNetwork {
				t.Errorf("Resolve = host %q on %q, want %q on %q", got.Host, got.Network.Name, tt.wantHost, tt.wantNetwork)
			}
			if !slices.Equal(got.ExposedPorts, tt.wantPorts) {
				t.Errorf("ExposedPorts = %v, want %v", got.ExposedPorts, tt.wantPorts)
			}
		})
	}
}

func TestResolve_NotAttachableNamesTheNetworks(t *testing.T) {
	fake := newFake(t)
	fake.AddContainer(dockertest.Container{ID: "c1", Name: "db", Running: true, Labels: swarmLabels("myapp-db"),
		Networks: map[string][]string{"closed-net": nil}})
	_, err := Resolve(testContext(t), newClient(t, fake), Target{Kind: KindSwarmService, AppName: "myapp-db"})
	var nae *NotAttachableError
	if !errors.As(err, &nae) || !slices.Equal(nae.Networks, []string{"closed-net"}) {
		t.Errorf("error = %v, want a *NotAttachableError naming closed-net", err)
	}
}
