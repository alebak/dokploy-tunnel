package repeater

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
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

// newResolver resolves on fake with Dokploy's compose directory.
func newResolver(t *testing.T, fake *dockertest.Fake) resolver {
	t.Helper()
	return resolver{docker: newClient(t, fake), composeDir: DefaultComposeDir}
}

// composeLabels are the labels docker compose sets on the containers of
// a service Dokploy deployed for project.
func composeLabels(project, service string) map[string]string {
	return map[string]string{
		"com.docker.compose.project":              project,
		"com.docker.compose.service":              service,
		"com.docker.compose.project.working_dir":  DefaultComposeDir + "/" + project + "/code",
		"com.docker.compose.project.config_files": DefaultComposeDir + "/" + project + "/code/docker-compose.yml",
	}
}

// with returns labels with extra added.
func with(labels map[string]string, extra map[string]string) map[string]string {
	m := maps.Clone(labels)
	maps.Copy(m, extra)
	return m
}

// swarmLabels are the labels Swarm sets on a task's container.
func swarmLabels(service string) map[string]string {
	return map[string]string{"com.docker.swarm.service.name": service, "com.docker.swarm.task.id": "t1"}
}

// swarmService is a Swarm service with one running task on the given
// networks, as "<network ID>=<address/prefix>" pairs.
type swarmService struct {
	service docker.Service
	// container is the task's container ID; it is local only when the
	// test adds the container.
	container string
	addresses map[string]string
}

func (s swarmService) add(fake *dockertest.Fake) {
	fake.AddService(s.service)
	task := docker.Task{ID: "task-" + s.service.ID, ServiceID: s.service.ID}
	task.Status.ContainerStatus.ContainerID = s.container
	for _, id := range slices.Sorted(maps.Keys(s.addresses)) {
		att := docker.TaskNetwork{Addresses: []string{s.addresses[id]}}
		att.Network.ID = id
		task.NetworksAttachments = append(task.NetworksAttachments, att)
	}
	fake.AddTask(task)
}

func service(id, name string) docker.Service {
	return docker.Service{ID: id, Spec: docker.ServiceSpec{Name: name}}
}

func TestResolve(t *testing.T) {
	stackService := service("s1", "mystack_postgres")
	stackService.Spec.Labels = map[string]string{"com.docker.stack.namespace": "mystack"}
	tests := []struct {
		name        string
		containers  []dockertest.Container
		services    []swarmService
		target      Target
		wantAddr    string
		wantNetwork string
		wantPorts   []Port
		wantErr     error
	}{
		{
			name: "application on dokploy-network",
			services: []swarmService{{service: service("s1", "myapp-web"), container: "c1",
				addresses: map[string]string{"net-ingress": "10.0.0.5/24", "net-dokploy": "10.0.1.7/24"}}},
			containers: []dockertest.Container{{
				ID: "c1", Name: "myapp-web.1.abc", Running: true, Labels: swarmLabels("myapp-web"),
				ExposedPorts: []string{"3000/tcp", "9090/udp"},
				Networks:     map[string][]string{"ingress": nil, "dokploy-network": nil},
			}},
			target:      Target{Kind: KindSwarmService, AppName: "myapp-web"},
			wantAddr:    "10.0.1.7",
			wantNetwork: "dokploy-network",
			wantPorts:   []Port{{3000, "tcp"}, {9090, "udp"}},
		},
		{
			name: "database on a custom network prefers it over dokploy-network",
			services: []swarmService{{service: service("s1", "myapp-db"),
				addresses: map[string]string{"net-dokploy": "10.0.1.8/24", "net-team": "10.0.2.8/24"}}},
			target:      Target{Kind: KindSwarmService, AppName: "myapp-db"},
			wantAddr:    "10.0.2.8",
			wantNetwork: "team-net",
		},
		{
			name: "Swarm task on another node",
			services: []swarmService{{service: service("s1", "myapp-web"), container: "elsewhere",
				addresses: map[string]string{"net-dokploy": "10.0.1.9/24"}}},
			target:      Target{Kind: KindSwarmService, AppName: "myapp-web"},
			wantAddr:    "10.0.1.9",
			wantNetwork: "dokploy-network",
		},
		{
			name: "compose service on its project network",
			containers: []dockertest.Container{{
				ID: "c1", Name: "myapp-postgres-1", Running: true, Labels: composeLabels("myapp", "postgres"),
				ExposedPorts: []string{"5432/tcp"},
				Networks:     map[string][]string{"dokploy-network": {"postgres"}, "myapp_default": {"postgres"}},
				IPs:          map[string]string{"dokploy-network": "10.0.1.3", "myapp_default": "172.20.0.3"},
			}},
			target:      Target{Kind: KindCompose, AppName: "myapp", Service: "postgres"},
			wantAddr:    "172.20.0.3",
			wantNetwork: "myapp_default",
			wantPorts:   []Port{{5432, "tcp"}},
		},
		{
			name: "compose service only on a shared network",
			containers: []dockertest.Container{{
				ID: "c1", Name: "myapp-postgres-1", Running: true, Labels: composeLabels("myapp", "postgres"),
				Networks: map[string][]string{"dokploy-network": {"postgres"}},
				IPs:      map[string]string{"dokploy-network": "10.0.1.3"},
			}},
			target:      Target{Kind: KindCompose, AppName: "myapp", Service: "postgres"},
			wantAddr:    "10.0.1.3",
			wantNetwork: "dokploy-network",
		},
		{
			name: "compose replicas that agree",
			containers: []dockertest.Container{
				{ID: "c2", Name: "myapp-postgres-2", Running: true, Labels: composeLabels("myapp", "postgres"),
					Networks: map[string][]string{"myapp_default": nil}, IPs: map[string]string{"myapp_default": "172.20.0.4"}},
				{ID: "c1", Name: "myapp-postgres-1", Running: true, Labels: composeLabels("myapp", "postgres"),
					Networks: map[string][]string{"myapp_default": nil}, IPs: map[string]string{"myapp_default": "172.20.0.3"}},
			},
			target:      Target{Kind: KindCompose, AppName: "myapp", Service: "postgres"},
			wantAddr:    "172.20.0.3",
			wantNetwork: "myapp_default",
		},
		{
			name: "compose service of another project is not matched",
			containers: []dockertest.Container{{
				ID: "c1", Name: "other-postgres-1", Running: true, Labels: composeLabels("other", "postgres"),
				Networks: map[string][]string{"dokploy-network": nil},
			}},
			target:  Target{Kind: KindCompose, AppName: "myapp", Service: "postgres"},
			wantErr: ErrTargetUnreachable,
		},
		{
			name: "stack service by its Swarm service",
			services: []swarmService{{service: stackService,
				addresses: map[string]string{"net-dokploy": "10.0.1.4/24", "net-mystack": "10.0.3.4/24"}}},
			target:      Target{Kind: KindStack, AppName: "mystack", Service: "postgres"},
			wantAddr:    "10.0.3.4",
			wantNetwork: "mystack_default",
		},
		{
			name: "only non-attachable overlays",
			services: []swarmService{{service: service("s1", "myapp-db"),
				addresses: map[string]string{"net-ingress": "10.0.0.5/24", "net-closed": "10.0.4.5/24"}}},
			target:  Target{Kind: KindSwarmService, AppName: "myapp-db"},
			wantErr: ErrNetworkNotAttachable,
		},
		{
			name: "only the default bridge",
			containers: []dockertest.Container{{
				ID: "c1", Name: "myapp-postgres-1", Running: true, Labels: composeLabels("myapp", "postgres"),
				Networks: map[string][]string{"bridge": nil},
			}},
			target:  Target{Kind: KindCompose, AppName: "myapp", Service: "postgres"},
			wantErr: ErrTargetUnreachable,
		},
		{
			name:    "no Swarm service",
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
				s.add(fake)
			}
			got, err := newResolver(t, fake).resolve(testContext(t), tt.target)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("resolve error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.Addr.String() != tt.wantAddr || got.Network.Name != tt.wantNetwork {
				t.Errorf("resolve = %s on %q, want %s on %q", got.Addr, got.Network.Name, tt.wantAddr, tt.wantNetwork)
			}
			if !slices.Equal(got.ExposedPorts, tt.wantPorts) {
				t.Errorf("ExposedPorts = %v, want %v", got.ExposedPorts, tt.wantPorts)
			}
		})
	}
}

func TestResolve_NotAttachableNamesTheNetworks(t *testing.T) {
	fake := newFake(t)
	swarmService{service: service("s1", "myapp-db"), addresses: map[string]string{"net-closed": "10.0.4.5/24"}}.add(fake)
	_, err := newResolver(t, fake).resolve(testContext(t), Target{Kind: KindSwarmService, AppName: "myapp-db"})
	var nae *NotAttachableError
	if !errors.As(err, &nae) || !slices.Equal(nae.Networks, []string{"closed-net"}) {
		t.Errorf("error = %v, want a *NotAttachableError naming closed-net", err)
	}
}

// TestResolve_ComposeIgnoresSpoofedLabels covers containers another tenant
// could run with a victim's Compose labels, named to sort first.
func TestResolve_ComposeIgnoresSpoofedLabels(t *testing.T) {
	victim := dockertest.Container{ID: "victim", Name: "myapp-postgres-1", Running: true, Labels: composeLabels("myapp", "postgres"),
		Networks: map[string][]string{"myapp_default": {"postgres"}}, IPs: map[string]string{"myapp_default": "172.20.0.3"}}
	target := Target{Kind: KindCompose, AppName: "myapp", Service: "postgres"}
	tests := []struct {
		name     string
		attacker dockertest.Container
		victim   bool
		wantErr  bool
	}{
		{
			// A stack file may set any container label; Swarm adds its own.
			name: "Swarm task with copied Compose labels",
			attacker: dockertest.Container{Labels: with(composeLabels("myapp", "postgres"), map[string]string{
				"com.docker.swarm.service.name": "evil_db", "com.docker.stack.namespace": "evil"})},
			victim: true,
		},
		{
			name: "container deployed from another directory",
			attacker: dockertest.Container{Labels: with(composeLabels("myapp", "postgres"), map[string]string{
				"com.docker.compose.project.working_dir": "/home/evil/myapp"})},
			victim: true,
		},
		{
			name: "directory that only shares a prefix",
			attacker: dockertest.Container{Labels: with(composeLabels("myapp", "postgres"), map[string]string{
				"com.docker.compose.project.working_dir": DefaultComposeDir + "/myapp-evil/code"})},
			victim: true,
		},
		{
			name: "directory escaping with dot-dot",
			attacker: dockertest.Container{Labels: with(composeLabels("myapp", "postgres"), map[string]string{
				"com.docker.compose.project.working_dir": DefaultComposeDir + "/myapp/../evil"})},
			victim: true,
		},
		{
			name: "no project directory",
			attacker: dockertest.Container{Labels: map[string]string{
				"com.docker.compose.project": "myapp", "com.docker.compose.service": "postgres"}},
			victim: true,
		},
		{
			name:     "only a container from another directory",
			attacker: dockertest.Container{Labels: with(composeLabels("myapp", "postgres"), map[string]string{"com.docker.compose.project.working_dir": "/srv/x"})},
			wantErr:  true,
		},
		{
			name:     "only a Swarm task with copied labels",
			attacker: dockertest.Container{Labels: with(composeLabels("myapp", "postgres"), swarmLabels("evil_db"))},
			wantErr:  true,
		},
		{
			// Both pass every check yet are on different networks: refuse
			// rather than guess.
			name:     "matching containers that disagree",
			attacker: dockertest.Container{Labels: composeLabels("myapp", "postgres")},
			victim:   true,
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFake(t)
			if tt.victim {
				fake.AddContainer(victim)
			}
			a := tt.attacker
			a.ID, a.Name, a.Running = "0-attacker", "aaa-attacker", true
			a.Networks = map[string][]string{"team-net": nil}
			fake.AddContainer(a)

			got, err := newResolver(t, fake).resolve(testContext(t), target)
			if tt.wantErr {
				if !errors.Is(err, ErrTargetUnreachable) {
					t.Fatalf("resolve = %+v, %v; want %v", got, err, ErrTargetUnreachable)
				}
				return
			}
			if err != nil || got.ContainerID != "victim" || got.Addr.String() != "172.20.0.3" {
				t.Fatalf("resolve = container %q at %s, %v; want the victim's own container", got.ContainerID, got.Addr, err)
			}
		})
	}
}

// TestResolve_ComposeRefusesSwarmLabelFallback: a Compose container's
// Swarm service label never redirects the tunnel.
func TestResolve_ComposeRefusesSwarmLabelFallback(t *testing.T) {
	fake := newFake(t)
	fake.AddContainer(dockertest.Container{ID: "c1", Name: "myapp-postgres-1", Running: true,
		Labels:   with(composeLabels("myapp", "postgres"), map[string]string{"com.docker.swarm.service.name": "dokploy-postgres"}),
		Networks: map[string][]string{"dokploy-network": nil}})
	got, err := newResolver(t, fake).resolve(testContext(t), Target{Kind: KindCompose, AppName: "myapp", Service: "postgres"})
	if !errors.Is(err, ErrTargetUnreachable) {
		t.Fatalf("resolve = %+v, %v; want a Compose candidate with Swarm labels refused", got, err)
	}
}

// TestResolve_SwarmUsesOnlyTheSwarmAPI: containers labelled with a
// service's name are never trusted for Swarm targets.
func TestResolve_SwarmUsesOnlyTheSwarmAPI(t *testing.T) {
	spoof := dockertest.Container{ID: "0-spoof", Name: "aaa-spoof", Running: true, Labels: swarmLabels("myapp-web"),
		Networks: map[string][]string{"team-net": nil}, IPs: map[string]string{"team-net": "10.0.2.66"}}
	target := Target{Kind: KindSwarmService, AppName: "myapp-web"}

	t.Run("labelled container without a service", func(t *testing.T) {
		fake := newFake(t)
		fake.AddContainer(spoof)
		if _, err := newResolver(t, fake).resolve(testContext(t), target); !errors.Is(err, ErrTargetUnreachable) {
			t.Fatalf("resolve error = %v, want %v", err, ErrTargetUnreachable)
		}
	})
	t.Run("labelled container next to the real task", func(t *testing.T) {
		fake := newFake(t)
		fake.AddContainer(spoof)
		fake.AddContainer(dockertest.Container{ID: "task-ctr", Name: "myapp-web.1.x", Running: true, Labels: swarmLabels("myapp-web"),
			Networks: map[string][]string{"dokploy-network": nil}})
		swarmService{service: service("s1", "myapp-web"), container: "task-ctr",
			addresses: map[string]string{"net-dokploy": "10.0.1.7/24"}}.add(fake)
		got, err := newResolver(t, fake).resolve(testContext(t), target)
		if err != nil || got.ContainerID != "task-ctr" || got.Addr.String() != "10.0.1.7" {
			t.Fatalf("resolve = container %q at %s, %v; want the task's", got.ContainerID, got.Addr, err)
		}
	})
	t.Run("service found by ID, not by name", func(t *testing.T) {
		fake := newFake(t)
		swarmService{service: service("myapp-web", "evil"), addresses: map[string]string{"net-dokploy": "10.0.1.7/24"}}.add(fake)
		if _, err := newResolver(t, fake).resolve(testContext(t), target); !errors.Is(err, ErrTargetUnreachable) {
			t.Fatalf("resolve error = %v, want %v", err, ErrTargetUnreachable)
		}
	})
	t.Run("stack service outside the stack", func(t *testing.T) {
		fake := newFake(t)
		svc := service("s1", "mystack_postgres")
		svc.Spec.Labels = map[string]string{"com.docker.stack.namespace": "other"}
		swarmService{service: svc, addresses: map[string]string{"net-mystack": "10.0.3.4/24"}}.add(fake)
		_, err := newResolver(t, fake).resolve(testContext(t), Target{Kind: KindStack, AppName: "mystack", Service: "postgres"})
		if !errors.Is(err, ErrTargetUnreachable) {
			t.Fatalf("resolve error = %v, want %v", err, ErrTargetUnreachable)
		}
	})
	t.Run("task not running yet", func(t *testing.T) {
		fake := newFake(t)
		fake.AddService(service("s1", "myapp-web"))
		task := docker.Task{ID: "t1", ServiceID: "s1"}
		task.Status.State = "starting"
		fake.AddTask(task)
		if _, err := newResolver(t, fake).resolve(testContext(t), target); !errors.Is(err, ErrTargetUnreachable) {
			t.Fatalf("resolve error = %v, want %v", err, ErrTargetUnreachable)
		}
	})
}

func TestReservedAppName(t *testing.T) {
	for name, want := range map[string]bool{
		"dokploy": true, "dokploy-postgres": true, "dokploy-redis": true, "dokploy-traefik": true, "dokploy-x": true,
		"dokployapp": false, "my-dokploy": false, "shop-pg": false,
	} {
		if got := reservedAppName(name); got != want {
			t.Errorf("reservedAppName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestResolve_RefusesReservedNamesBeforeDocker(t *testing.T) {
	for _, target := range []Target{
		{Kind: KindSwarmService, AppName: "dokploy"},
		{Kind: KindSwarmService, AppName: "dokploy-postgres"},
		{Kind: KindStack, AppName: "dokploy-x", Service: "db"},
		{Kind: KindCompose, AppName: "dokploy-traefik", Service: "traefik"},
	} {
		t.Run(target.String(), func(t *testing.T) {
			fake := newFake(t)
			_, err := newResolver(t, fake).resolve(testContext(t), target)
			if !errors.Is(err, ErrTargetUnreachable) || !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("resolve error = %v, want the reserved name refused", err)
			}
			if v := fake.Versions(); len(v) != 0 {
				t.Errorf("Docker was asked about a reserved name (API versions used: %v)", v)
			}
		})
	}
}

func TestResolve_RefusesHostLevelTargets(t *testing.T) {
	socket := []docker.Mount{{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock"}}
	tests := []struct {
		name string
		ctr  dockertest.Container
	}{
		{"privileged", dockertest.Container{Privileged: true}},
		{"host network", dockertest.Container{NetworkMode: "host"}},
		{"Docker socket mount", dockertest.Container{Mounts: socket}},
		{"Docker socket at another path", dockertest.Container{Mounts: []docker.Mount{{Type: "bind", Source: "/run/user/1000/docker.sock", Destination: "/sock/docker.sock"}}}},
		{"Docker socket bind", dockertest.Container{Binds: []string{"/run/docker.sock:/var/run/docker.sock:ro"}}},
		{"host root bind", dockertest.Container{Binds: []string{"/:/host:ro"}}},
		{"run directory mount", dockertest.Container{Mounts: []docker.Mount{{Type: "bind", Source: "/run", Destination: "/host-run"}}}},
		{"containerd socket bind", dockertest.Container{Binds: []string{"/run/containerd/containerd.sock:/c.sock"}}},
		{"seccomp unconfined", dockertest.Container{SecurityOpt: []string{"seccomp=unconfined"}}},
		{"AppArmor unconfined", dockertest.Container{SecurityOpt: []string{"apparmor=unconfined"}}},
		{"SELinux labeling disabled", dockertest.Container{SecurityOpt: []string{"label=disable"}}},
		{"system paths unconfined", dockertest.Container{SecurityOpt: []string{"systempaths=unconfined"}}},
		{"system paths unmasked", dockertest.Container{MaskedPaths: []string{}, ReadonlyPaths: []string{}}},
	}
	for _, tt := range tests {
		t.Run("compose "+tt.name, func(t *testing.T) {
			fake := newFake(t)
			c := tt.ctr
			c.ID, c.Name, c.Running, c.Labels = "c1", "myapp-postgres-1", true, composeLabels("myapp", "postgres")
			c.Networks = map[string][]string{"myapp_default": nil}
			fake.AddContainer(c)
			_, err := newResolver(t, fake).resolve(testContext(t), Target{Kind: KindCompose, AppName: "myapp", Service: "postgres"})
			if !errors.Is(err, ErrTargetUnreachable) || !strings.Contains(err.Error(), "refusing") {
				t.Fatalf("resolve error = %v, want the target refused", err)
			}
		})
		t.Run("Swarm "+tt.name, func(t *testing.T) {
			fake := newFake(t)
			c := tt.ctr
			c.ID, c.Name, c.Running, c.Labels = "c1", "myapp-web.1.x", true, swarmLabels("myapp-web")
			c.Networks = map[string][]string{"dokploy-network": nil}
			fake.AddContainer(c)
			swarmService{service: service("s1", "myapp-web"), container: "c1",
				addresses: map[string]string{"net-dokploy": "10.0.1.7/24"}}.add(fake)
			_, err := newResolver(t, fake).resolve(testContext(t), Target{Kind: KindSwarmService, AppName: "myapp-web"})
			if !errors.Is(err, ErrTargetUnreachable) || !strings.Contains(err.Error(), "refusing") {
				t.Fatalf("resolve error = %v, want the target refused", err)
			}
		})
	}
	t.Run("Swarm service spec mounting the socket", func(t *testing.T) {
		fake := newFake(t)
		svc := service("s1", "myapp-web")
		svc.Spec.TaskTemplate.ContainerSpec.Mounts = []docker.ServiceMount{{Type: "bind", Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"}}
		swarmService{service: svc, container: "remote", addresses: map[string]string{"net-dokploy": "10.0.1.7/24"}}.add(fake)
		_, err := newResolver(t, fake).resolve(testContext(t), Target{Kind: KindSwarmService, AppName: "myapp-web"})
		if !errors.Is(err, ErrTargetUnreachable) || !strings.Contains(err.Error(), "daemon socket") {
			t.Fatalf("resolve error = %v, want the service refused", err)
		}
	})
	// A task on another node cannot be inspected, so the service's spec
	// alone must refuse it.
	specs := []struct {
		name string
		spec docker.ContainerSpec
	}{
		{"mounting /run", docker.ContainerSpec{Mounts: []docker.ServiceMount{{Type: "bind", Source: "/run", Target: "/host-run"}}}},
		{"mounting the host root", docker.ContainerSpec{Mounts: []docker.ServiceMount{{Type: "bind", Source: "/", Target: "/host"}}}},
		{"adding SYS_ADMIN", docker.ContainerSpec{CapabilityAdd: []string{"CAP_SYS_ADMIN"}}},
		{"adding all capabilities", docker.ContainerSpec{CapabilityAdd: []string{"all"}}},
		{"binding the Docker data root", docker.ContainerSpec{Mounts: []docker.ServiceMount{{Type: "bind", Source: "/var/lib/docker", Target: "/data"}}}},
		{"binding the containerd data root", docker.ContainerSpec{Mounts: []docker.ServiceMount{{Type: "bind", Source: "/var/lib/containerd/io.containerd.content.v1.content", Target: "/data"}}}},
		{"creating a bind volume of /run", docker.ContainerSpec{Mounts: []docker.ServiceMount{bindVolumeMount("hostrun", "local", "bind", "/run")}}},
		{"creating a bind volume with the default driver", docker.ContainerSpec{Mounts: []docker.ServiceMount{bindVolumeMount("hostroot", "", "rbind,ro", "/")}}},
		{"creating a bind volume of the Docker data root", docker.ContainerSpec{Mounts: []docker.ServiceMount{bindVolumeMount("dockerdata", "local", "bind", "/var/lib/docker")}}},
		{"creating a bind volume with a relative device", docker.ContainerSpec{Mounts: []docker.ServiceMount{bindVolumeMount("rel", "local", "bind", "run")}}},
		{"setting a host-wide sysctl", docker.ContainerSpec{Sysctls: map[string]string{"kernel.panic": "1"}}},
		{"setting a vm sysctl", docker.ContainerSpec{Sysctls: map[string]string{"vm.overcommit_memory": "1"}}},
		{"running without seccomp", docker.ContainerSpec{Privileges: &docker.Privileges{Seccomp: &docker.SeccompOpts{Mode: "unconfined"}}}},
		{"running without AppArmor", docker.ContainerSpec{Privileges: &docker.Privileges{AppArmor: &docker.AppArmorOpts{Mode: "disabled"}}}},
		{"disabling SELinux labeling", docker.ContainerSpec{Privileges: &docker.Privileges{SELinuxContext: &docker.SELinuxContext{Disable: true}}}},
	}
	for _, tt := range specs {
		t.Run("Swarm service spec "+tt.name, func(t *testing.T) {
			fake := newFake(t)
			svc := service("s1", "myapp-web")
			svc.Spec.TaskTemplate.ContainerSpec = tt.spec
			swarmService{service: svc, container: "remote", addresses: map[string]string{"net-dokploy": "10.0.1.7/24"}}.add(fake)
			_, err := newResolver(t, fake).resolve(testContext(t), Target{Kind: KindSwarmService, AppName: "myapp-web"})
			if !errors.Is(err, ErrTargetUnreachable) || !strings.Contains(err.Error(), "refusing") {
				t.Fatalf("resolve error = %v, want the service refused", err)
			}
		})
	}
	t.Run("Swarm service spec with harmless capabilities", func(t *testing.T) {
		fake := newFake(t)
		svc := service("s1", "myapp-web")
		svc.Spec.TaskTemplate.ContainerSpec.CapabilityAdd = []string{"NET_BIND_SERVICE", "CAP_CHOWN"}
		svc.Spec.TaskTemplate.ContainerSpec.Mounts = []docker.ServiceMount{
			{Type: "bind", Source: "/srv/app", Target: "/data"},
			{Type: "bind", Source: "/var/lib/app", Target: "/state"},
			{Type: "volume", Source: "pgdata", Target: "/var/lib/postgresql/data"},
			bindVolumeMount("appdata", "local", "bind", "/srv/appdata"),
			{Type: "volume", Source: "nfsdata", Target: "/nfs", VolumeOptions: &docker.ServiceVolumeOptions{DriverConfig: &docker.VolumeDriverConfig{
				Name: "local", Options: map[string]string{"type": "nfs", "o": "addr=10.0.0.2,rw", "device": ":/export"}}}},
		}
		svc.Spec.TaskTemplate.ContainerSpec.Sysctls = map[string]string{"net.core.somaxconn": "1024", "kernel.shmmax": "68719476736", "fs.mqueue.msg_max": "64"}
		svc.Spec.TaskTemplate.ContainerSpec.Privileges = &docker.Privileges{
			Seccomp:        &docker.SeccompOpts{Mode: "custom"},
			AppArmor:       &docker.AppArmorOpts{Mode: "default"},
			SELinuxContext: &docker.SELinuxContext{},
		}
		swarmService{service: svc, container: "remote", addresses: map[string]string{"net-dokploy": "10.0.1.7/24"}}.add(fake)
		_, err := newResolver(t, fake).resolve(testContext(t), Target{Kind: KindSwarmService, AppName: "myapp-web"})
		if err != nil && strings.Contains(err.Error(), "refusing") {
			t.Fatalf("resolve error = %v, want the service accepted", err)
		}
	})
}

// bindVolumeMount is a Swarm volume mount that creates volume name with
// driver and the local driver's bind options.
func bindVolumeMount(name, driver, o, device string) docker.ServiceMount {
	return docker.ServiceMount{Type: "volume", Source: name, Target: "/data", VolumeOptions: &docker.ServiceVolumeOptions{
		DriverConfig: &docker.VolumeDriverConfig{Name: driver, Options: map[string]string{"type": "none", "o": o, "device": device}}}}
}

// TestResolve_JudgesTheDaemonDataRoot covers a daemon configured with a
// custom data-root: what lives there is as host-reaching as
// /var/lib/docker.
func TestResolve_JudgesTheDaemonDataRoot(t *testing.T) {
	const root = "/data/docker"
	tests := []struct {
		name   string
		ctr    dockertest.Container
		volume *docker.Volume
		spec   docker.ContainerSpec
		unsafe bool
	}{
		{name: "bind of the data root", ctr: dockertest.Container{Binds: []string{root + ":/d"}}, unsafe: true},
		{name: "bind of a directory holding it", ctr: dockertest.Container{Binds: []string{"/data:/d:ro"}}, unsafe: true},
		{name: "mount of another volume's data", ctr: dockertest.Container{Mounts: []docker.Mount{
			{Type: "bind", Source: root + "/volumes/other/_data", Destination: "/other"}}}, unsafe: true},
		{name: "bind volume of the data root", volume: &docker.Volume{Name: "vol", Driver: "local", Mountpoint: root + "/volumes/vol/_data",
			Options: map[string]string{"type": "none", "o": "bind", "device": root + "/containers"}}, unsafe: true},
		{name: "spec binding the data root", spec: docker.ContainerSpec{Mounts: []docker.ServiceMount{{Type: "bind", Source: root, Target: "/d"}}}, unsafe: true},
		{name: "spec creating a bind volume of the data root", spec: docker.ContainerSpec{Mounts: []docker.ServiceMount{bindVolumeMount("v", "local", "bind", root+"/overlay2")}}, unsafe: true},
		{name: "bind next to the data root", ctr: dockertest.Container{Binds: []string{"/data/app:/app", "/data/docker-backup:/backup"}}},
		{name: "plain volume under the data root", volume: &docker.Volume{Name: "vol", Driver: "local", Mountpoint: root + "/volumes/vol/_data"}},
		{name: "spec binding next to the data root", spec: docker.ContainerSpec{Mounts: []docker.ServiceMount{{Type: "bind", Source: "/data/app", Target: "/app"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFake(t)
			fake.SetDockerRootDir(root)
			c := tt.ctr
			c.ID, c.Name, c.Running, c.Labels = "c1", "myapp-web.1.x", true, swarmLabels("myapp-web")
			c.Networks = map[string][]string{"dokploy-network": nil}
			if tt.volume != nil {
				fake.AddVolume(*tt.volume)
				c.Mounts = append(c.Mounts, docker.Mount{Type: "volume", Name: tt.volume.Name, Driver: "local", Source: tt.volume.Mountpoint, Destination: "/vol"})
			}
			fake.AddContainer(c)
			svc := service("s1", "myapp-web")
			svc.Spec.TaskTemplate.ContainerSpec = tt.spec
			swarmService{service: svc, container: "c1", addresses: map[string]string{"net-dokploy": "10.0.1.7/24"}}.add(fake)
			_, err := newResolver(t, fake).resolve(testContext(t), Target{Kind: KindSwarmService, AppName: "myapp-web"})
			refused := errors.Is(err, ErrTargetUnreachable) && strings.Contains(err.Error(), "refusing")
			if refused != tt.unsafe || (!tt.unsafe && err != nil) {
				t.Fatalf("resolve error = %v, want refused = %v", err, tt.unsafe)
			}
		})
	}
}

// TestResolve_FailsClosedWithoutTheDataRoot: a target that cannot be
// judged is not forwarded to.
func TestResolve_FailsClosedWithoutTheDataRoot(t *testing.T) {
	targets := map[string]Target{
		"compose": {Kind: KindCompose, AppName: "myapp", Service: "postgres"},
		"Swarm":   {Kind: KindSwarmService, AppName: "myapp-web"},
	}
	for name, target := range targets {
		t.Run(name, func(t *testing.T) {
			fake := newFake(t)
			fake.FailInfo(&docker.APIError{StatusCode: 500, Message: "info unavailable"})
			fake.AddContainer(dockertest.Container{ID: "c1", Name: "myapp-postgres-1", Running: true, Labels: composeLabels("myapp", "postgres"),
				Networks: map[string][]string{"myapp_default": nil}})
			swarmService{service: service("s1", "myapp-web"), container: "remote", addresses: map[string]string{"net-dokploy": "10.0.1.7/24"}}.add(fake)
			_, err := newResolver(t, fake).resolve(testContext(t), target)
			if !errors.Is(err, ErrTargetUnreachable) || !strings.Contains(err.Error(), "data root") {
				t.Fatalf("resolve error = %v, want the target unreachable for want of the data root", err)
			}
		})
	}
}

// TestResolve_ReadsTheDataRootOnce: concurrent and repeated resolutions
// share one read of /info.
func TestResolve_ReadsTheDataRootOnce(t *testing.T) {
	fake := newFake(t)
	swarmService{service: service("s1", "myapp-web"), container: "remote", addresses: map[string]string{"net-dokploy": "10.0.1.7/24"}}.add(fake)
	rs := newResolver(t, fake)
	ctx := testContext(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, err := rs.resolve(ctx, Target{Kind: KindSwarmService, AppName: "myapp-web"})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
	}
	if n := fake.InfoRequests(); n != 1 {
		t.Errorf("/info asked %d times, want 1", n)
	}
}

// TestResolve_AcceptsSafeSecurityProfiles: default and custom profiles,
// and options that only tighten the container, are not refused.
func TestResolve_AcceptsSafeSecurityProfiles(t *testing.T) {
	tests := []struct {
		name string
		ctr  dockertest.Container
	}{
		{"no-new-privileges", dockertest.Container{SecurityOpt: []string{"no-new-privileges"}}},
		{"no-new-privileges with a value", dockertest.Container{SecurityOpt: []string{"no-new-privileges=true", "no-new-privileges:true"}}},
		{"AppArmor default profile", dockertest.Container{SecurityOpt: []string{"apparmor=docker-default"}}},
		{"custom seccomp profile", dockertest.Container{SecurityOpt: []string{`seccomp={"defaultAction":"SCMP_ACT_ERRNO"}`}}},
		{"SELinux type and level", dockertest.Container{SecurityOpt: []string{"label=type:svirt_apache_t", "label:level:s0:c100,c200"}}},
		{"default masked paths", dockertest.Container{MaskedPaths: []string{"/proc/kcore", "/sys/firmware"}, ReadonlyPaths: []string{"/proc/sys"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFake(t)
			c := tt.ctr
			c.ID, c.Name, c.Running, c.Labels = "c1", "myapp-postgres-1", true, composeLabels("myapp", "postgres")
			c.Networks = map[string][]string{"myapp_default": nil}
			fake.AddContainer(c)
			if _, err := newResolver(t, fake).resolve(testContext(t), Target{Kind: KindCompose, AppName: "myapp", Service: "postgres"}); err != nil {
				t.Fatalf("resolve: %v; want the target accepted", err)
			}
		})
	}
}

// A named volume of the local driver with bind options reports a source
// under /var/lib/docker/volumes, so the volume itself must be inspected.
func TestResolve_JudgesNamedVolumes(t *testing.T) {
	bindVolume := func(name, o, device string) *docker.Volume {
		return &docker.Volume{Name: name, Driver: "local", Mountpoint: "/var/lib/docker/volumes/" + name + "/_data",
			Options: map[string]string{"type": "none", "o": o, "device": device}}
	}
	tests := []struct {
		name   string
		volume *docker.Volume
		unsafe bool
	}{
		{"bind volume of /run", bindVolume("vol", "bind", "/run"), true},
		{"rbind volume of the host root", bindVolume("vol", "rbind,ro", "/"), true},
		{"bind volume of the Docker data root", bindVolume("vol", "bind", "/var/lib/docker"), true},
		{"bind volume of a rootless runtime dir", bindVolume("vol", "ro,bind", "/run/user/1000"), true},
		{"bind volume with a relative device", bindVolume("vol", "bind", "run"), true},
		{"missing volume", nil, true},
		{"ext4 volume of a host disk", &docker.Volume{Name: "vol", Driver: "local", Mountpoint: "/var/lib/docker/volumes/vol/_data",
			Options: map[string]string{"type": "ext4", "device": "/dev/sda1"}}, true},
		{"plain named volume", &docker.Volume{Name: "vol", Driver: "local", Mountpoint: "/var/lib/docker/volumes/vol/_data"}, false},
		{"bind volume of an app directory", bindVolume("vol", "bind", "/srv/app"), false},
		{"bind volume of /var/lib/app", bindVolume("vol", "bind", "/var/lib/app"), false},
		{"NFS volume", &docker.Volume{Name: "vol", Driver: "local", Mountpoint: "/var/lib/docker/volumes/vol/_data",
			Options: map[string]string{"type": "nfs", "o": "addr=10.0.0.2,rw", "device": ":/export"}}, false},
		{"CIFS volume", &docker.Volume{Name: "vol", Driver: "local", Mountpoint: "/var/lib/docker/volumes/vol/_data",
			Options: map[string]string{"type": "cifs", "o": "addr=10.0.0.2,username=app", "device": "//10.0.0.2/share"}}, false},
		{"tmpfs volume", &docker.Volume{Name: "vol", Driver: "local", Mountpoint: "/var/lib/docker/volumes/vol/_data",
			Options: map[string]string{"type": "tmpfs", "o": "size=100m", "device": "tmpfs"}}, false},
		{"volume of another driver", &docker.Volume{Name: "vol", Driver: "rexray", Options: map[string]string{"o": "bind", "device": "/run"}}, false},
	}
	mounts := []docker.Mount{{Type: "volume", Name: "vol", Driver: "local", Source: "/var/lib/docker/volumes/vol/_data", Destination: "/data"}}
	check := func(t *testing.T, err error, unsafe bool) {
		t.Helper()
		refused := errors.Is(err, ErrTargetUnreachable) && strings.Contains(err.Error(), "refusing")
		if refused != unsafe {
			t.Fatalf("resolve error = %v, want refused = %v", err, unsafe)
		}
	}
	for _, tt := range tests {
		t.Run("compose "+tt.name, func(t *testing.T) {
			fake := newFake(t)
			if tt.volume != nil {
				fake.AddVolume(*tt.volume)
			}
			fake.AddContainer(dockertest.Container{ID: "c1", Name: "myapp-postgres-1", Running: true, Labels: composeLabels("myapp", "postgres"),
				Networks: map[string][]string{"myapp_default": nil}, Mounts: mounts})
			_, err := newResolver(t, fake).resolve(testContext(t), Target{Kind: KindCompose, AppName: "myapp", Service: "postgres"})
			check(t, err, tt.unsafe)
		})
		t.Run("Swarm "+tt.name, func(t *testing.T) {
			fake := newFake(t)
			if tt.volume != nil {
				fake.AddVolume(*tt.volume)
			}
			fake.AddContainer(dockertest.Container{ID: "c1", Name: "myapp-web.1.x", Running: true, Labels: swarmLabels("myapp-web"),
				Networks: map[string][]string{"dokploy-network": nil}, Mounts: mounts})
			swarmService{service: service("s1", "myapp-web"), container: "c1",
				addresses: map[string]string{"net-dokploy": "10.0.1.7/24"}}.add(fake)
			_, err := newResolver(t, fake).resolve(testContext(t), Target{Kind: KindSwarmService, AppName: "myapp-web"})
			check(t, err, tt.unsafe)
		})
	}
}

func TestBindExposesHost(t *testing.T) {
	tests := []struct {
		source string
		want   bool
	}{
		{"/var/run/docker.sock", true},
		{"/run", true},
		{"/", true},
		{"/var/lib", true},
		{"/var/lib/docker", true},
		{"/var/lib/docker/", true},
		{"/var/lib/docker/containers", true},
		{"/var/lib/docker/overlay2/abc/merged", true},
		{"/var/lib/docker/volumes/other/_data", true},
		{"/var/lib/containerd", true},
		{"/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs", true},
		{"/var/lib/containers/storage", true},
		{"/home/me/.local/share/docker", true},
		{"/home/me/.local/share/docker/overlay2", true},
		{"/home/me/.local/share", true},
		{"/home/me/.local", true},
		{"/var/lib/./docker/../docker", true},
		{"", false},
		{"myvolume", false},
		{"/var/lib/app", false},
		{"/var/lib/dockerd", false},
		{"/var/lib/docker-backup", false},
		{"/var/lib/postgresql/data", false},
		{"/home/me/.local/share/app", false},
		{"/home/me/.local/share/docker-compose", false},
		{"/srv/app", false},
	}
	for _, tt := range tests {
		if got := bindExposesHost(tt.source, "/var/lib/docker"); got != tt.want {
			t.Errorf("bindExposesHost(%q) = %v, want %v", tt.source, got, tt.want)
		}
	}
	custom := []struct {
		source string
		want   bool
	}{
		{"/data/docker", true},
		{"/data/docker/", true},
		{"/data/docker/overlay2/abc/merged", true},
		{"/data", true},
		{"/data/./docker/../docker/containers", true},
		{"/var/lib/docker", true},
		{"/data/app", false},
		{"/data/docker-backup", false},
		{"/srv/app", false},
	}
	for _, tt := range custom {
		if got := bindExposesHost(tt.source, "/data/docker"); got != tt.want {
			t.Errorf("bindExposesHost(%q) with data root /data/docker = %v, want %v", tt.source, got, tt.want)
		}
	}
}

func TestMountExposesHost(t *testing.T) {
	tests := []struct {
		source string
		want   bool
	}{
		{"/", true},
		{"/run", true},
		{"/run/", true},
		{"/var", true},
		{"/var/run", true},
		{"/var/run/docker.sock", true},
		{"/run/docker.sock", true},
		{"/run/containerd", true},
		{"/run/containerd/containerd.sock", true},
		{"/run/podman/podman.sock", true},
		{"/var/run/podman", true},
		{"/home/me/podman.sock", true},
		{"/srv/app/api.sock", true},
		{"/run/user", true},
		{"/run/user/1000", true},
		{"/run/user/1000/docker.sock", true},
		{"/run/user/1000/podman", true},
		{"/var/run/user/1000", true},
		{"/run/./containerd/../docker.sock", true},
		{"", false},
		{"myvolume", false},
		{"/runner/data", false},
		{"/run/lock", false},
		{"/run/users", false},
		{"/srv/app", false},
		{"/var/lib/app", false},
		{"/var/lib/docker/volumes/data/_data", false},
		{"/etc/ssl/certs", false},
	}
	for _, tt := range tests {
		if got := mountExposesHost(tt.source); got != tt.want {
			t.Errorf("mountExposesHost(%q) = %v, want %v", tt.source, got, tt.want)
		}
	}
}

func TestUnsafeContainer(t *testing.T) {
	ctr := func(edit func(*docker.Container)) docker.Container {
		var c docker.Container
		edit(&c)
		return c
	}
	tests := []struct {
		name   string
		ctr    docker.Container
		unsafe bool
	}{
		{"plain", ctr(func(c *docker.Container) {}), false},
		{"host PID namespace", ctr(func(c *docker.Container) { c.HostConfig.PidMode = "host" }), true},
		{"PID namespace of another container", ctr(func(c *docker.Container) { c.HostConfig.PidMode = "container:abc" }), false},
		{"device", ctr(func(c *docker.Container) {
			c.HostConfig.Devices = []docker.DeviceMapping{{PathOnHost: "/dev/sda", PathInContainer: "/dev/sda"}}
		}), true},
		{"SYS_ADMIN", ctr(func(c *docker.Container) { c.HostConfig.CapAdd = []string{"SYS_ADMIN"} }), true},
		{"CAP_SYS_PTRACE", ctr(func(c *docker.Container) { c.HostConfig.CapAdd = []string{"CAP_SYS_PTRACE"} }), true},
		{"lower-case cap_sys_module", ctr(func(c *docker.Container) { c.HostConfig.CapAdd = []string{"cap_sys_module"} }), true},
		{"SYS_RAWIO", ctr(func(c *docker.Container) { c.HostConfig.CapAdd = []string{"SYS_RAWIO"} }), true},
		{"DAC_READ_SEARCH", ctr(func(c *docker.Container) { c.HostConfig.CapAdd = []string{"DAC_READ_SEARCH"} }), true},
		{"NET_ADMIN", ctr(func(c *docker.Container) { c.HostConfig.CapAdd = []string{"NET_ADMIN"} }), true},
		{"BPF", ctr(func(c *docker.Container) { c.HostConfig.CapAdd = []string{"bpf"} }), true},
		{"PERFMON", ctr(func(c *docker.Container) { c.HostConfig.CapAdd = []string{"PERFMON"} }), true},
		{"ALL", ctr(func(c *docker.Container) { c.HostConfig.CapAdd = []string{"CHOWN", "ALL"} }), true},
		{"host IPC namespace", ctr(func(c *docker.Container) { c.HostConfig.IpcMode = "host" }), true},
		{"shareable IPC namespace", ctr(func(c *docker.Container) { c.HostConfig.IpcMode = "shareable" }), false},
		{"host UTS namespace", ctr(func(c *docker.Container) { c.HostConfig.UTSMode = "host" }), true},
		{"GPU request", ctr(func(c *docker.Container) {
			c.HostConfig.DeviceRequests = []docker.DeviceRequest{{Driver: "nvidia", Count: -1, Capabilities: [][]string{{"gpu"}}}}
		}), true},
		{"host-wide sysctl", ctr(func(c *docker.Container) { c.HostConfig.Sysctls = map[string]string{"kernel.panic": "1"} }), true},
		{"namespaced sysctls", ctr(func(c *docker.Container) {
			c.HostConfig.Sysctls = map[string]string{"net.core.somaxconn": "1024", "kernel.shmmax": "68719476736", "kernel.sem": "250 32000 100 128"}
		}), false},
		{"bind of the Docker data root", ctr(func(c *docker.Container) { c.HostConfig.Binds = []string{"/var/lib/docker:/docker"} }), true},
		{"mount of the containerd data root", ctr(func(c *docker.Container) {
			c.Mounts = []docker.Mount{{Type: "bind", Source: "/var/lib/containerd", Destination: "/c"}}
		}), true},
		{"bind of another volume's data", ctr(func(c *docker.Container) {
			c.Mounts = []docker.Mount{{Type: "bind", Source: "/var/lib/docker/volumes/other/_data", Destination: "/other"}}
		}), true},
		{"bind of a rootless data root", ctr(func(c *docker.Container) { c.HostConfig.Binds = []string{"/home/me/.local/share/docker:/d:ro"} }), true},
		{"bind of /var/lib/app", ctr(func(c *docker.Container) { c.HostConfig.Binds = []string{"/var/lib/app:/app"} }), false},
		{"harmless capabilities", ctr(func(c *docker.Container) { c.HostConfig.CapAdd = []string{"NET_BIND_SERVICE", "CAP_CHOWN"} }), false},
		{"bind of /var/run", ctr(func(c *docker.Container) { c.HostConfig.Binds = []string{"/var/run:/host/run:ro"} }), true},
		{"bind of a rootless runtime dir", ctr(func(c *docker.Container) { c.HostConfig.Binds = []string{"/run/user/1000:/xdg"} }), true},
		{"mount of the host root", ctr(func(c *docker.Container) {
			c.Mounts = []docker.Mount{{Type: "bind", Source: "/", Destination: "/host"}}
		}), true},
		{"volume holding a socket at a socket path", ctr(func(c *docker.Container) {
			c.Mounts = []docker.Mount{{Type: "volume", Source: "/var/lib/docker/volumes/dind/_data", Destination: "/var/run/docker.sock"}}
		}), true},
		{"bind of the daemon's custom data root", ctr(func(c *docker.Container) { c.HostConfig.Binds = []string{"/data/docker/containers:/c"} }), true},
		{"bind next to the daemon's custom data root", ctr(func(c *docker.Container) { c.HostConfig.Binds = []string{"/data/app:/app"} }), false},
		{"seccomp unconfined", ctr(func(c *docker.Container) { c.HostConfig.SecurityOpt = []string{"seccomp=unconfined"} }), true},
		{"legacy seccomp unconfined", ctr(func(c *docker.Container) { c.HostConfig.SecurityOpt = []string{"seccomp:unconfined"} }), true},
		{"AppArmor unconfined", ctr(func(c *docker.Container) {
			c.HostConfig.SecurityOpt = []string{"no-new-privileges", "apparmor=unconfined"}
		}), true},
		{"legacy AppArmor unconfined", ctr(func(c *docker.Container) { c.HostConfig.SecurityOpt = []string{"apparmor:unconfined"} }), true},
		{"label disabled", ctr(func(c *docker.Container) { c.HostConfig.SecurityOpt = []string{"label=disable"} }), true},
		{"legacy label disabled", ctr(func(c *docker.Container) { c.HostConfig.SecurityOpt = []string{"label:disable"} }), true},
		{"bare label disable", ctr(func(c *docker.Container) { c.HostConfig.SecurityOpt = []string{"disable"} }), true},
		{"system paths unconfined", ctr(func(c *docker.Container) { c.HostConfig.SecurityOpt = []string{"systempaths=unconfined"} }), true},
		{"masked paths emptied", ctr(func(c *docker.Container) { c.HostConfig.MaskedPaths = []string{} }), true},
		{"read-only paths emptied", ctr(func(c *docker.Container) { c.HostConfig.ReadonlyPaths = []string{} }), true},
		{"default profiles", ctr(func(c *docker.Container) {
			c.HostConfig.SecurityOpt = []string{"apparmor=docker-default", "no-new-privileges:true", "label=level:s0:c1,c2", `seccomp={"defaultAction":"SCMP_ACT_ERRNO"}`}
			c.HostConfig.MaskedPaths = []string{"/proc/kcore"}
			c.HostConfig.ReadonlyPaths = []string{"/proc/sys"}
		}), false},
		{"safe binds and volumes", ctr(func(c *docker.Container) {
			c.HostConfig.Binds = []string{"/srv/app:/data", "/runner/data:/runner:ro", "pgdata:/var/lib/postgresql/data"}
			c.Mounts = []docker.Mount{{Type: "volume", Source: "/var/lib/docker/volumes/pgdata/_data", Destination: "/var/lib/postgresql/data"}}
		}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if why := unsafeContainer(tt.ctr, "/data/docker"); (why != "") != tt.unsafe {
				t.Errorf("unsafeContainer() = %q, want unsafe = %v", why, tt.unsafe)
			}
		})
	}
}

func TestHostSysctl(t *testing.T) {
	tests := []struct {
		name string
		key  string
		host bool
	}{
		{"IPC parameter", "kernel.shmmax", false},
		{"message queue parameter", "fs.mqueue.msg_max", false},
		{"network parameter", "net.core.somaxconn", false},
		{"UTS domain name", "kernel.domainname", false},
		{"user namespace counter", "user.max_user_namespaces", false},
		{"slash-separated network parameter", "net/ipv4/ip_forward", false},
		{"slash-separated IPC parameter", "kernel/shmmax", false},
		{"host-wide kernel parameter", "kernel.panic", true},
		{"slash-separated host-wide parameter", "kernel/panic", true},
		{"vm parameter", "vm.overcommit_memory", true},
		{"hostname, which runc refuses", "kernel.hostname", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hostSysctl(map[string]string{tt.key: "1"}) != ""
			if got != tt.host {
				t.Errorf("hostSysctl(%q) reported host-wide = %v, want %v", tt.key, got, tt.host)
			}
		})
	}
}
