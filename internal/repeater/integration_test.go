//go:build integration

// Integration tests against a real Docker daemon. They create and remove
// containers, so they only run in the integration workflow
// (.github/workflows/integration.yml), which prepares the targets below,
// and only when DOKTUNNEL_INTEGRATION=1 as a second guard.

package repeater

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/docker"
)

// Targets the workflow deploys.
const (
	// itProject is a docker compose project whose service echo answers
	// on port 7000 (testdata/compose.integration.yml).
	itProject = "doktunnel-it"
	// itSwarmService is a Swarm service on an attachable overlay that
	// answers on port 7000.
	itSwarmService = "doktunnel-it-web"
	// itClosedService is a Swarm service only on a non-attachable overlay.
	itClosedService = "doktunnel-it-closed"
	itPort          = 7000
)

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
	r := integrationRepeater(t, c, Options{Grace: time.Second})
	target := Target{Kind: KindCompose, AppName: itProject, Service: "echo", Port: itPort}

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
	if _, ok := ctr.NetworkSettings.Networks[itProject+"_default"]; !ok {
		t.Errorf("repeater networks = %v, want %s_default", ctr.NetworkSettings.Networks, itProject)
	}
	if got := ctr.Config.Labels[LabelTarget]; got != "echo" {
		t.Errorf("repeater target label = %q, want the Compose service name", got)
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
	r := integrationRepeater(t, c, Options{Grace: time.Second})

	_, err := r.Open(testContext(t), Target{Kind: KindSwarmService, AppName: itClosedService, Port: itPort})
	if !errors.Is(err, ErrNetworkNotAttachable) {
		t.Errorf("non-attachable overlay: error = %v, want %v", err, ErrNetworkNotAttachable)
	}
	_, err = r.Open(testContext(t), Target{Kind: KindCompose, AppName: itProject, Service: "echo", Port: itPort + 1})
	if !errors.Is(err, ErrTargetUnreachable) || !strings.Contains(err.Error(), "socat") {
		t.Errorf("closed port: error = %v, want %v from socat", err, ErrTargetUnreachable)
	}
}

func TestIntegration_ReaperRemovesAbandonedRepeaters(t *testing.T) {
	c := integrationClient(t)
	// A companion that dies with a tunnel open: its Repeater is never
	// closed.
	crashed := New(c, Options{Grace: time.Hour, Log: slog.New(slog.DiscardHandler)})
	s, err := crashed.Open(testContext(t), Target{Kind: KindCompose, AppName: itProject, Service: "echo", Port: itPort})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.exec.Close()
	if n := len(repeaters(t, c)); n != 1 {
		t.Fatalf("%d repeaters, want 1", n)
	}

	time.Sleep(1100 * time.Millisecond) // Created has a resolution of one second.
	next := integrationRepeater(t, c, Options{TTL: time.Second})
	removed, err := next.Reap(testContext(t))
	if err != nil || len(removed) != 1 {
		t.Fatalf("Reap = %v, %v; want the abandoned repeater", removed, err)
	}
	if n := len(repeaters(t, c)); n != 0 {
		t.Errorf("%d repeaters left after reaping", n)
	}
}
