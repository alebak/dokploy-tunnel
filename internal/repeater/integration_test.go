//go:build integration

// Integration tests against a real Docker daemon. They create and remove
// containers, so they only run in the integration workflow
// (.github/workflows/integration.yml), which prepares the Swarm targets
// below, and only when DOKTUNNEL_INTEGRATION=1 as a second guard.
//
// The repeater reaches Docker at DOCKER_HOST, which the workflow points at
// doktunnel-socket-proxy for a second run. What the tests set up besides
// the repeater goes to the daemon directly, as an admin would.

package repeater

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/docker"
)

// Targets the workflow deploys.
const (
	// itSwarmService is a Swarm service on an attachable overlay that
	// answers on port 7000.
	itSwarmService = "doktunnel-it-web"
	// itClosedService is a Swarm service only on a non-attachable overlay.
	itClosedService = "doktunnel-it-closed"
	itPort          = 7000
)

// adminHost is the runner's Docker daemon, which the tests set up their
// targets on directly, whatever DOCKER_HOST says.
const adminHost = docker.DefaultHost

// adminClient talks to the daemon directly.
func adminClient(t *testing.T) *docker.Client {
	t.Helper()
	c, err := docker.New(adminHost)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func integrationClient(t *testing.T) *docker.Client {
	t.Helper()
	if os.Getenv("DOKTUNNEL_INTEGRATION") != "1" {
		t.Skip("set DOKTUNNEL_INTEGRATION=1 to run against a real Docker daemon")
	}
	c, err := docker.New(os.Getenv("DOCKER_HOST"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(testContext(t)); err != nil {
		t.Fatalf("Docker is not reachable: %v", err)
	}
	return c
}

// composeProject deploys testdata/compose.integration.yml, whose service
// echo answers on port 7000, the way Dokploy does: as project appName from
// <dir>/<appName>/code. It returns the appName and dir, to use as
// Options.ComposeDir, and removes the project when the test ends.
func composeProject(t *testing.T) (appName, dir string) {
	t.Helper()
	appName = "doktunnel-it-" + randomHex(4)
	dir = t.TempDir()
	code := filepath.Join(dir, appName, "code")
	src, err := os.ReadFile(filepath.Join("testdata", "compose.integration.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(code, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(code, "docker-compose.yml")
	if err := os.WriteFile(file, src, 0o644); err != nil {
		t.Fatal(err)
	}
	compose := func(args ...string) error {
		cmd := exec.Command("docker", append([]string{"compose", "-p", appName, "-f", file}, args...)...)
		cmd.Env = append(os.Environ(), "DOCKER_HOST="+adminHost)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		return cmd.Run()
	}
	t.Cleanup(func() {
		if err := compose("down", "--timeout", "1"); err != nil {
			t.Errorf("removing compose project %s: %v", appName, err)
		}
	})
	if err := compose("up", "-d", "--wait"); err != nil {
		t.Fatalf("deploying compose project %s: %v", appName, err)
	}
	return appName, dir
}

func integrationRepeater(t *testing.T, c *docker.Client, opts Options) *Repeater {
	t.Helper()
	opts.Log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	r := New(c, opts)
	t.Cleanup(func() {
		if err := r.Close(testContext(t)); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return r
}

// repeaters lists the repeater containers on the daemon.
func repeaters(t *testing.T, c *docker.Client) []docker.ContainerSummary {
	t.Helper()
	found, err := c.ListContainers(testContext(t), docker.ListOptions{All: true, Labels: []string{LabelRepeater + "=1"}})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// echo writes line and reads the echoed line back.
func echo(t *testing.T, s io.ReadWriter, line string) {
	t.Helper()
	if _, err := io.WriteString(s, line+"\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := bufio.NewReader(s).ReadString('\n')
	if err != nil || got != line+"\n" {
		t.Fatalf("read %q, %v; want %q", got, err, line)
	}
}

func TestIntegration_ComposeService(t *testing.T) {
	c := integrationClient(t)
	project, dir := composeProject(t)
	r := integrationRepeater(t, c, Options{Grace: time.Second, ComposeDir: dir})
	target := Target{Kind: KindCompose, AppName: project, Service: "echo", Port: itPort}

	ports, err := r.ExposedPorts(testContext(t), target)
	if err != nil || !slices.Contains(ports, Port{itPort, "tcp"}) {
		t.Errorf("ExposedPorts = %v, %v; want %d/tcp", ports, err, itPort)
	}

	// Concurrent connections share one repeater.
	var wg sync.WaitGroup
	streams := make([]*Stream, 3)
	for i := range streams {
		wg.Go(func() {
			s, err := r.Open(testContext(t), target)
			if err != nil {
				t.Errorf("Open: %v", err)
				return
			}
			streams[i] = s
		})
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	for i, s := range streams {
		echo(t, s, strings.Repeat("ping", i+1))
	}

	reps := repeaters(t, c)
	if len(reps) != 1 {
		t.Fatalf("%d repeaters, want 1 shared", len(reps))
	}
	ctr, err := c.InspectContainer(testContext(t), reps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(ctr.NetworkSettings.Networks); n != 1 {
		t.Errorf("repeater joined %d networks (%v), want only the target's", n, ctr.NetworkSettings.Networks)
	}
	if _, ok := ctr.NetworkSettings.Networks[project+"_default"]; !ok {
		t.Errorf("repeater networks = %v, want %s_default", ctr.NetworkSettings.Networks, project)
	}
	if got := ctr.Config.Labels[LabelTarget]; got != project+"/echo" {
		t.Errorf("repeater target label = %q, want %s/echo", got, project)
	}
	if ctr.HostConfig.Privileged {
		t.Error("repeater runs privileged")
	}

	// Half-close reaches the target, which still answers.
	s := streams[0]
	io.WriteString(s, "last words\n")
	if err := s.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	if got, _ := io.ReadAll(s); string(got) != "last words\n" {
		t.Errorf("after half-close read %q", got)
	}

	for _, s := range streams {
		s.Close()
	}
	deadline := time.Now().Add(15 * time.Second)
	for len(repeaters(t, c)) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the repeater outlived its grace period")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestIntegration_ComposeIgnoresDecoys runs containers that copy the
// target's Compose labels, named to sort first and on the same network,
// and answering differently: tunnels must still reach the real service.
func TestIntegration_ComposeIgnoresDecoys(t *testing.T) {
	c := integrationClient(t)
	project, dir := composeProject(t)
	r := integrationRepeater(t, c, Options{Grace: time.Second, ComposeDir: dir})
	target := Target{Kind: KindCompose, AppName: project, Service: "echo", Port: itPort}

	real, err := c.ListContainers(testContext(t), docker.ListOptions{Labels: []string{
		labelComposeProject + "=" + project, labelComposeService + "=echo"}})
	if err != nil || len(real) != 1 {
		t.Fatalf("listing the real container: %v, %v", real, err)
	}
	victim, err := c.InspectContainer(testContext(t), real[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	// Decoys are not repeaters: a socket proxy would rightly refuse them.
	admin := adminClient(t)

	decoy := func(name string, labels map[string]string) {
		t.Helper()
		cfg := docker.ContainerConfig{
			Image:  victim.Config.Image,
			Cmd:    []string{"TCP-LISTEN:7000,fork,reuseaddr", "SYSTEM:echo decoy"},
			Labels: labels,
			HostConfig: docker.HostConfig{
				NetworkMode: project + "_default",
			},
		}
		id, err := admin.CreateContainer(testContext(t), name, cfg)
		if err != nil {
			t.Fatalf("creating decoy %s: %v", name, err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := admin.RemoveContainer(ctx, id); err != nil {
				t.Errorf("removing decoy %s: %v", name, err)
			}
		})
		if err := admin.StartContainer(testContext(t), id); err != nil {
			t.Fatalf("starting decoy %s: %v", name, err)
		}
	}
	// What a stack file can do: any label, next to Swarm's own.
	stackLike := maps.Clone(victim.Config.Labels)
	stackLike["com.docker.swarm.service.name"] = "evil_echo"
	decoy("aaa-"+project+"-swarm-decoy", stackLike)
	// A container started from another directory.
	elsewhere := maps.Clone(victim.Config.Labels)
	elsewhere[labelComposeWorkingDir] = "/srv/elsewhere"
	elsewhere[labelComposeConfigFiles] = "/srv/elsewhere/docker-compose.yml"
	decoy("aaa-"+project+"-dir-decoy", elsewhere)

	ep, err := r.Resolve(testContext(t), target)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if ep.ContainerID != victim.ID {
		t.Fatalf("resolved container %s, want the real one %s", ep.ContainerID, victim.ID)
	}
	for i := range 3 {
		s, err := r.Open(testContext(t), target)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		echo(t, s, strings.Repeat("real", i+1))
		s.Close()
	}
}

func TestIntegration_SwarmService(t *testing.T) {
	c := integrationClient(t)
	r := integrationRepeater(t, c, Options{})
	s, err := r.Open(testContext(t), Target{Kind: KindSwarmService, AppName: itSwarmService, Port: itPort})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	echo(t, s, "swarm")
}

func TestIntegration_Failures(t *testing.T) {
	c := integrationClient(t)
	project, dir := composeProject(t)
	r := integrationRepeater(t, c, Options{Grace: time.Second, ComposeDir: dir})

	_, err := r.Open(testContext(t), Target{Kind: KindSwarmService, AppName: itClosedService, Port: itPort})
	if !errors.Is(err, ErrNetworkNotAttachable) {
		t.Errorf("non-attachable overlay: error = %v, want %v", err, ErrNetworkNotAttachable)
	}
	// Outside Dokploy's directory for the appName, nothing is found.
	other := integrationRepeater(t, c, Options{Grace: time.Second})
	_, err = other.Open(testContext(t), Target{Kind: KindCompose, AppName: project, Service: "echo", Port: itPort})
	if !errors.Is(err, ErrTargetUnreachable) {
		t.Errorf("compose project outside the compose directory: error = %v, want %v", err, ErrTargetUnreachable)
	}
	_, err = r.Open(testContext(t), Target{Kind: KindCompose, AppName: project, Service: "echo", Port: itPort + 1})
	if !errors.Is(err, ErrTargetUnreachable) || !strings.Contains(err.Error(), "socat") {
		t.Errorf("closed port: error = %v, want %v from socat", err, ErrTargetUnreachable)
	}
}

func TestIntegration_ReaperRemovesAbandonedRepeaters(t *testing.T) {
	c := integrationClient(t)
	project, dir := composeProject(t)
	// A companion that dies with a tunnel open: its Repeater is never
	// closed. Restarted, it holds the same key.
	key := randomBytes(32)
	crashed := New(c, Options{Grace: time.Hour, ComposeDir: dir, Key: key, Log: slog.New(slog.DiscardHandler)})
	s, err := crashed.Open(testContext(t), Target{Kind: KindCompose, AppName: project, Service: "echo", Port: itPort})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.exec.Close()
	if n := len(repeaters(t, c)); n != 1 {
		t.Fatalf("%d repeaters, want 1", n)
	}

	time.Sleep(1100 * time.Millisecond) // Created has a resolution of one second.
	next := integrationRepeater(t, c, Options{TTL: time.Second, Key: key})
	removed, err := next.Reap(testContext(t))
	if err != nil || len(removed) != 1 {
		t.Fatalf("Reap = %v, %v; want the abandoned repeater", removed, err)
	}
	if n := len(repeaters(t, c)); n != 0 {
		t.Errorf("%d repeaters left after reaping", n)
	}
}
