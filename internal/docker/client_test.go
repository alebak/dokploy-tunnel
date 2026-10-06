package docker_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/docker"
	"github.com/alebak/dokploy-tunnel/internal/docker/dockertest"
)

func newClient(t *testing.T, fake *dockertest.Fake) *docker.Client {
	t.Helper()
	c, err := docker.New(fake.Host())
	if err != nil {
		t.Fatalf("New(%q): %v", fake.Host(), err)
	}
	return c
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestNew_Hosts(t *testing.T) {
	tests := []struct {
		name, host string
		wantErr    bool
	}{
		{"empty is the default socket", "", false},
		{"unix socket", "unix:///run/docker.sock", false},
		{"tcp socket proxy", "tcp://docker-proxy:2375", false},
		{"unix without a path", "unix://", true},
		{"tcp without a port", "tcp://docker-proxy", true},
		{"TLS is not supported", "https://docker:2376", true},
		{"ssh is not supported", "ssh://user@host", true},
		{"garbage", "::", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := docker.New(tt.host)
			if (err != nil) != tt.wantErr {
				t.Errorf("New(%q) error = %v, want error %v", tt.host, err, tt.wantErr)
			}
		})
	}
}

func TestClient_NegotiatesAPIVersion(t *testing.T) {
	tests := []struct {
		name, server, want string
	}{
		{"older daemon", "1.44", "1.44"},
		{"newer daemon is capped", "1.51", docker.MaxAPIVersion},
		{"same version", docker.MaxAPIVersion, docker.MaxAPIVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := dockertest.New(t)
			fake.APIVersion = tt.server
			c := newClient(t, fake)
			if _, err := c.ListContainers(testContext(t), docker.ListOptions{}); err != nil {
				t.Fatalf("ListContainers: %v", err)
			}
			if got := c.APIVersion(); got != tt.want {
				t.Errorf("APIVersion = %q, want %q", got, tt.want)
			}
			if got := fake.Versions(); !slices.Equal(got, []string{tt.want}) {
				t.Errorf("request versions = %q, want only %q", got, tt.want)
			}
		})
	}
}

func TestClient_UnixSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the companion runs on Linux; unix sockets are only exercised there and on macOS")
	}
	// A short directory: unix socket paths are limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "dk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("API-Version", "1.45")
		if r.URL.Path == "/v1.45/containers/json" {
			_, _ = w.Write([]byte("[]"))
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	c, err := docker.New("unix://" + sock)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Ping(testContext(t)); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got, err := c.ListContainers(testContext(t), docker.ListOptions{}); err != nil || len(got) != 0 {
		t.Fatalf("ListContainers = %v, %v; want none", got, err)
	}
}

func TestClient_ListContainers_Filters(t *testing.T) {
	fake := dockertest.New(t)
	fake.AddContainer(dockertest.Container{ID: "a1", Name: "myapp-postgres-1", Running: true,
		Labels: map[string]string{"com.docker.compose.project": "myapp", "com.docker.compose.service": "postgres"}})
	fake.AddContainer(dockertest.Container{ID: "b2", Name: "myapp-web-1", Running: true,
		Labels: map[string]string{"com.docker.compose.project": "myapp", "com.docker.compose.service": "web"}})
	fake.AddContainer(dockertest.Container{ID: "c3", Name: "myapp-postgres-old", Running: false,
		Labels: map[string]string{"com.docker.compose.project": "myapp", "com.docker.compose.service": "postgres"}})
	c := newClient(t, fake)

	tests := []struct {
		name string
		opts docker.ListOptions
		want []string
	}{
		{"running by labels", docker.ListOptions{Labels: []string{"com.docker.compose.project=myapp", "com.docker.compose.service=postgres"}}, []string{"a1"}},
		{"all by labels", docker.ListOptions{All: true, Labels: []string{"com.docker.compose.service=postgres"}}, []string{"a1", "c3"}},
		{"label key only", docker.ListOptions{Labels: []string{"com.docker.compose.project"}}, []string{"a1", "b2"}},
		{"no match", docker.ListOptions{Labels: []string{"com.docker.compose.project=other"}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.ListContainers(testContext(t), tt.opts)
			if err != nil {
				t.Fatalf("ListContainers: %v", err)
			}
			var ids []string
			for _, s := range got {
				ids = append(ids, s.ID)
			}
			slices.Sort(ids)
			if !slices.Equal(ids, tt.want) {
				t.Errorf("ids = %q, want %q", ids, tt.want)
			}
		})
	}
}

func TestClient_InspectContainer(t *testing.T) {
	fake := dockertest.New(t)
	fake.AddNetwork(docker.Network{ID: "net1", Name: "myapp_default", Driver: "bridge", Scope: "local"})
	fake.AddContainer(dockertest.Container{
		ID: "a1", Name: "myapp-postgres-1", Running: true,
		ExposedPorts: []string{"5432/tcp"},
		Networks:     map[string][]string{"myapp_default": {"postgres", "myapp-postgres-1"}},
	})
	c := newClient(t, fake)

	got, err := c.InspectContainer(testContext(t), "a1")
	if err != nil {
		t.Fatalf("InspectContainer: %v", err)
	}
	if got.Name != "/myapp-postgres-1" || !got.State.Running {
		t.Errorf("container = %+v", got)
	}
	if _, ok := got.Config.ExposedPorts["5432/tcp"]; !ok {
		t.Errorf("ExposedPorts = %v, want 5432/tcp", got.Config.ExposedPorts)
	}
	ep, ok := got.NetworkSettings.Networks["myapp_default"]
	if !ok || ep.NetworkID != "net1" || !slices.Contains(ep.Aliases, "postgres") {
		t.Errorf("networks = %+v", got.NetworkSettings.Networks)
	}

	_, err = c.InspectContainer(testContext(t), "missing")
	if !docker.IsNotFound(err) {
		t.Errorf("inspecting a missing container: error = %v, want not found", err)
	}
	var apiErr *docker.APIError
	if !errors.As(err, &apiErr) || apiErr.Message == "" {
		t.Errorf("error = %#v, want an *APIError with the daemon's message", err)
	}
}

func TestClient_ContainerLifecycle(t *testing.T) {
	fake := dockertest.New(t)
	fake.AddNetwork(docker.Network{ID: "net1", Name: "myapp_default", Driver: "bridge", Scope: "local"})
	c := newClient(t, fake)
	ctx := testContext(t)

	cfg := docker.ContainerConfig{
		Image:      "alpine/socat:1",
		Entrypoint: []string{"sleep"},
		Cmd:        []string{"infinity"},
		Labels:     map[string]string{"dev.doktunnel.repeater": "1"},
		HostConfig: docker.HostConfig{NetworkMode: "myapp_default", CapDrop: []string{"ALL"}},
	}
	if _, err := c.CreateContainer(ctx, "doktunnel-repeater-x", cfg); !docker.IsNotFound(err) {
		t.Fatalf("creating from a missing image: error = %v, want not found", err)
	}
	if err := c.PullImage(ctx, "alpine/socat:1"); err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	id, err := c.CreateContainer(ctx, "doktunnel-repeater-x", cfg)
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if err := c.StartContainer(ctx, id); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	got, err := c.InspectContainer(ctx, id)
	if err != nil {
		t.Fatalf("InspectContainer: %v", err)
	}
	if !got.State.Running || got.Config.Labels["dev.doktunnel.repeater"] != "1" {
		t.Errorf("created container = %+v", got)
	}
	if _, ok := got.NetworkSettings.Networks["myapp_default"]; !ok {
		t.Errorf("created container networks = %v, want myapp_default", got.NetworkSettings.Networks)
	}
	if created := fake.Created(); len(created) != 1 || created[0].HostConfig.CapDrop[0] != "ALL" {
		t.Errorf("created = %+v", created)
	}
	if err := c.RemoveContainer(ctx, id); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if err := c.RemoveContainer(ctx, id); !docker.IsNotFound(err) {
		t.Errorf("removing twice: error = %v, want not found", err)
	}
	if n := fake.VolumeRemovals(); n != 0 {
		t.Errorf("RemoveContainer asked to remove volumes %d times", n)
	}
}

func TestClient_ListTasks_Filters(t *testing.T) {
	fake := dockertest.New(t)
	fake.AddService(docker.Service{ID: "s1", Spec: docker.ServiceSpec{Name: "web"}})
	fake.AddService(docker.Service{ID: "s2", Spec: docker.ServiceSpec{Name: "db"}})
	running := docker.Task{ID: "t1", ServiceID: "s1"}
	running.Status.ContainerStatus.ContainerID = "c1"
	att := docker.TaskNetwork{Addresses: []string{"10.0.1.7/24"}}
	att.Network.ID = "net1"
	running.NetworksAttachments = []docker.TaskNetwork{att}
	fake.AddTask(running)
	fake.AddTask(docker.Task{ID: "t2", ServiceID: "s1", DesiredState: "shutdown"})
	fake.AddTask(docker.Task{ID: "t3", ServiceID: "s2"})

	got, err := newClient(t, fake).ListTasks(testContext(t), docker.TaskListOptions{Service: "s1", DesiredState: "running"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != 1 || got[0].ID != "t1" || got[0].Status.ContainerStatus.ContainerID != "c1" ||
		got[0].NetworksAttachments[0].Network.ID != "net1" || got[0].NetworksAttachments[0].Addresses[0] != "10.0.1.7/24" {
		t.Errorf("ListTasks = %+v, want running task t1 with its attachment", got)
	}
}

func TestClient_InspectTask(t *testing.T) {
	fake := dockertest.New(t)
	task := docker.Task{ID: "t1", ServiceID: "s1"}
	att := docker.TaskNetwork{Addresses: []string{"10.0.1.7/24"}}
	att.Network.ID = "net1"
	task.NetworksAttachments = []docker.TaskNetwork{att}
	fake.AddTask(task)
	c := newClient(t, fake)
	ctx := testContext(t)

	got, err := c.InspectTask(ctx, "t1")
	if err != nil || got.ID != "t1" || got.Status.State != "running" || got.NetworksAttachments[0].Addresses[0] != "10.0.1.7/24" {
		t.Errorf("InspectTask = %+v, %v, want running task t1 with its attachment", got, err)
	}
	fake.SetTaskState("t1", "shutdown")
	if got, err := c.InspectTask(ctx, "t1"); err != nil || got.Status.State != "shutdown" {
		t.Errorf("InspectTask after it ended = %+v, %v, want state shutdown", got, err)
	}
	if _, err := c.InspectTask(ctx, "t2"); !docker.IsNotFound(err) {
		t.Errorf("InspectTask missing: error = %v, want not found", err)
	}
}

func TestClient_PullImage_StreamError(t *testing.T) {
	fake := dockertest.New(t)
	fake.PullError = "manifest unknown"
	c := newClient(t, fake)
	err := c.PullImage(testContext(t), "alpine/socat:nope")
	if err == nil || !strings.Contains(err.Error(), "manifest unknown") {
		t.Errorf("PullImage error = %v, want the error from the progress stream", err)
	}
}

func TestClient_PullImage_Digest(t *testing.T) {
	fake := dockertest.New(t)
	c := newClient(t, fake)
	const ref = "alpine/socat:1.8@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := c.PullImage(testContext(t), ref); err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	want := "alpine/socat@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if got := fake.Pulls(); !slices.Equal(got, []string{want}) {
		t.Errorf("pulls = %q, want %q (a digest pins the image, the tag is informative)", got, want)
	}
}

func TestClient_InspectNetworkAndService(t *testing.T) {
	fake := dockertest.New(t)
	fake.AddNetwork(docker.Network{ID: "ov1", Name: "dokploy-network", Driver: "overlay", Scope: "swarm", Attachable: true})
	fake.AddService(docker.Service{ID: "svc1", Spec: docker.ServiceSpec{Name: "myapp-web",
		TaskTemplate: docker.TaskTemplate{Networks: []docker.NetworkAttachment{{Target: "ov1"}}}}})
	c := newClient(t, fake)
	ctx := testContext(t)

	n, err := c.InspectNetwork(ctx, "dokploy-network")
	if err != nil || n.ID != "ov1" || !n.Attachable || n.Driver != "overlay" {
		t.Errorf("InspectNetwork by name = %+v, %v", n, err)
	}
	s, err := c.InspectService(ctx, "myapp-web")
	if err != nil || s.Spec.Name != "myapp-web" || len(s.Spec.TaskTemplate.Networks) != 1 {
		t.Errorf("InspectService by name = %+v, %v", s, err)
	}
	if _, err := c.InspectService(ctx, "other"); !docker.IsNotFound(err) {
		t.Errorf("InspectService missing: error = %v, want not found", err)
	}
}

func TestClient_InspectVolume(t *testing.T) {
	fake := dockertest.New(t)
	fake.AddVolume(docker.Volume{Name: "hostrun", Driver: "local", Mountpoint: "/var/lib/docker/volumes/hostrun/_data",
		Options: map[string]string{"type": "none", "o": "bind", "device": "/run"}})
	c := newClient(t, fake)
	ctx := testContext(t)

	v, err := c.InspectVolume(ctx, "hostrun")
	if err != nil || v.Driver != "local" || v.Options["o"] != "bind" || v.Options["device"] != "/run" {
		t.Errorf("InspectVolume = %+v, %v", v, err)
	}
	if _, err := c.InspectVolume(ctx, "other"); !docker.IsNotFound(err) {
		t.Errorf("InspectVolume missing: error = %v, want not found", err)
	}
}

func TestClient_DaemonUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	c, err := docker.New("tcp://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(testContext(t)); err == nil {
		t.Error("Ping to a closed port succeeded")
	}
}

func TestClient_Info(t *testing.T) {
	fake := dockertest.New(t)
	fake.SetDockerRootDir("/data/docker")
	c := newClient(t, fake)
	info, err := c.Info(testContext(t))
	if err != nil || info.DockerRootDir != "/data/docker" {
		t.Fatalf("Info = %+v, %v; want DockerRootDir /data/docker", info, err)
	}
	fake.FailInfo(&docker.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"})
	if _, err := c.Info(testContext(t)); err == nil {
		t.Fatal("Info succeeded while /info fails")
	}
}

func TestClient_InspectContainer_SecurityOptions(t *testing.T) {
	fake := dockertest.New(t)
	fake.AddContainer(dockertest.Container{ID: "c1", Name: "app", Running: true,
		SecurityOpt: []string{"seccomp=unconfined"}, MaskedPaths: []string{}})
	ctr, err := newClient(t, fake).InspectContainer(testContext(t), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ctr.HostConfig.SecurityOpt, []string{"seccomp=unconfined"}) {
		t.Errorf("SecurityOpt = %v, want [seccomp=unconfined]", ctr.HostConfig.SecurityOpt)
	}
	if ctr.HostConfig.MaskedPaths == nil || len(ctr.HostConfig.MaskedPaths) != 0 {
		t.Errorf("MaskedPaths = %#v, want an empty, non-nil list", ctr.HostConfig.MaskedPaths)
	}
	if ctr.HostConfig.ReadonlyPaths != nil {
		t.Errorf("ReadonlyPaths = %#v, want nil when the daemon reports none", ctr.HostConfig.ReadonlyPaths)
	}
}
