package repeater

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/docker/dockertest"
)

// upperServer is a target that answers each line with it upper-cased, and
// "BYE" once the client half-closes.
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

// closedAddr is an address nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

var postgresTarget = Target{Kind: KindCompose, AppName: "myapp", Service: "postgres", Port: 5432}

// postgresIP is the address of compose service myapp/postgres on
// myapp_default.
const postgresIP = "172.20.0.5"

// newTargetFake is a fake daemon running compose service myapp/postgres
// on myapp_default, whose repeaters run socat that reaches addrs, keyed by
// the address socat dials.
func newTargetFake(t *testing.T, addrs map[string]string) *dockertest.Fake {
	t.Helper()
	fake := newFake(t)
	fake.AddImage(testImage)
	fake.AddContainer(dockertest.Container{
		ID: "pg1", Name: "myapp-postgres-1", Running: true, Labels: composeLabels("myapp", "postgres"),
		ExposedPorts: []string{"5432/tcp"},
		Networks:     map[string][]string{"myapp_default": {"postgres", "myapp-postgres-1"}},
		IPs:          map[string]string{"myapp_default": postgresIP},
	})
	fake.ExecHandler = dockertest.Socat(func(host string) (string, bool) {
		addr, ok := addrs[host]
		return addr, ok
	})
	return fake
}

func TestOpen_RoundTripWithHalfClose(t *testing.T) {
	fake := newTargetFake(t, map[string]string{postgresIP: upperServer(t)})
	r := newRepeater(t, fake, Options{})

	s, err := r.Open(testContext(t), postgresTarget)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if _, err := io.WriteString(s, "select 1\nselect 2\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	got, err := io.ReadAll(s)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := "SELECT 1\nSELECT 2\nBYE\n"; string(got) != want {
		t.Errorf("read %q, want %q", got, want)
	}

	execs := fake.Execs()
	if len(execs) != 1 {
		t.Fatalf("%d execs, want 1", len(execs))
	}
	// socat dials the address the daemon reports, not a name.
	want := []string{"socat", "-d", "-d", "STDIO", "TCP:" + postgresIP + ":5432,connect-timeout=15"}
	if !slices.Equal(execs[0].Cmd, want) {
		t.Errorf("exec cmd = %q, want %q", execs[0].Cmd, want)
	}
}

func TestOpen_DataBeforeReadinessIsKept(t *testing.T) {
	fake := newTargetFake(t, nil)
	// A target that speaks first, such as MySQL, can get its greeting
	// through before socat's notice.
	fake.ExecHandler = func(_ string, _ []string, stdin io.Reader, stdout, stderr io.Writer) int {
		io.WriteString(stdout, "greeting\n")
		io.WriteString(stderr, "2026/10/03 12:00:00 socat[7] N starting data transfer loop with FDs [0,1] and [5,5]\n")
		io.Copy(stdout, stdin)
		return 0
	}
	r := newRepeater(t, fake, Options{})
	s, err := r.Open(testContext(t), postgresTarget)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	io.WriteString(s, "echo\n")
	s.CloseWrite()
	got, _ := io.ReadAll(s)
	if string(got) != "greeting\necho\n" {
		t.Errorf("read %q", got)
	}
}

func TestOpen_ConcurrentStreamsShareTheRepeater(t *testing.T) {
	fake := newTargetFake(t, map[string]string{postgresIP: upperServer(t)})
	r := newRepeater(t, fake, Options{Grace: 20 * time.Millisecond})

	var streams []io.ReadWriteCloser
	for range 3 {
		s, err := r.Open(testContext(t), postgresTarget)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		streams = append(streams, s)
	}
	for i, s := range streams {
		msg := strings.Repeat("x", i+1)
		io.WriteString(s, msg+"\n")
		line, err := bufio.NewReader(s).ReadString('\n')
		if err != nil || line != strings.ToUpper(msg)+"\n" {
			t.Errorf("stream %d read %q, %v", i, line, err)
		}
	}
	if n := len(fake.Created()); n != 1 {
		t.Errorf("created %d repeaters for one target, want 1", n)
	}
	repeaterID := fake.Execs()[0].ContainerID
	for _, s := range streams[:2] {
		s.Close()
	}
	time.Sleep(60 * time.Millisecond)
	if _, ok := fake.Container(repeaterID); !ok {
		t.Fatal("repeater removed while a stream is open")
	}
	streams[2].Close()
	streams[2].Close() // idempotent
	eventually(t, "the repeater to be removed", func() bool {
		_, ok := fake.Container(repeaterID)
		return !ok
	})
}

func TestOpen_Failures(t *testing.T) {
	tests := []struct {
		name    string
		addrs   map[string]string
		target  Target
		wantErr error
		detail  string
	}{
		{"connection refused", map[string]string{postgresIP: closedAddr(t)}, postgresTarget, ErrTargetUnreachable, "Connection refused"},
		{"address unreachable", nil, postgresTarget, ErrTargetUnreachable, "socat"},
		{"no such service", nil, Target{Kind: KindCompose, AppName: "myapp", Service: "redis", Port: 6379}, ErrTargetUnreachable, "no running container"},
		{"invalid port", nil, Target{Kind: KindCompose, AppName: "myapp", Service: "postgres", Port: 70000}, ErrTargetUnreachable, "port"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newTargetFake(t, tt.addrs)
			r := newRepeater(t, fake, Options{Grace: time.Millisecond})
			_, err := r.Open(testContext(t), tt.target)
			if !errors.Is(err, tt.wantErr) || !strings.Contains(err.Error(), tt.detail) {
				t.Fatalf("Open error = %v, want %v mentioning %q", err, tt.wantErr, tt.detail)
			}
			eventually(t, "no repeater left", func() bool {
				return len(fake.ContainerIDs()) == 1 // only the target
			})
		})
	}
}

func TestOpen_TimesOutWaitingForSocat(t *testing.T) {
	fake := newTargetFake(t, nil)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	fake.ExecHandler = func(string, []string, io.Reader, io.Writer, io.Writer) int {
		<-release
		return 0
	}
	r := newRepeater(t, fake, Options{})
	ctx, cancel := context.WithTimeout(testContext(t), 100*time.Millisecond)
	defer cancel()
	_, err := r.Open(ctx, postgresTarget)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Open error = %v, want a deadline", err)
	}
}

func TestOpen_ReplacesADeadRepeater(t *testing.T) {
	fake := newTargetFake(t, map[string]string{postgresIP: upperServer(t)})
	r := newRepeater(t, fake, Options{Grace: time.Hour})
	s, err := r.Open(testContext(t), postgresTarget)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	dead := fake.Execs()[0].ContainerID
	fake.StopContainer(dead)

	s, err = r.Open(testContext(t), postgresTarget)
	if err != nil {
		t.Fatalf("Open after the repeater died: %v", err)
	}
	defer s.Close()
	io.WriteString(s, "ok\n")
	if line, _ := bufio.NewReader(s).ReadString('\n'); line != "OK\n" {
		t.Errorf("read %q", line)
	}
	if n := len(fake.Created()); n != 2 {
		t.Errorf("created %d repeaters, want a replacement", n)
	}
	eventually(t, "the dead repeater to be removed", func() bool {
		_, ok := fake.Container(dead)
		return !ok
	})
}

func TestExposedPorts_IgnoresNetworks(t *testing.T) {
	fake := newFake(t)
	fake.AddContainer(dockertest.Container{
		ID: "mq", Name: "myapp-rabbitmq-1", Running: true, Labels: composeLabels("myapp", "rabbitmq"),
		ExposedPorts: []string{"15672/tcp", "5672/tcp", "4369/tcp"},
		Networks:     map[string][]string{"closed-net": nil},
	})
	r := newRepeater(t, fake, Options{})
	got, err := r.ExposedPorts(testContext(t), Target{Kind: KindCompose, AppName: "myapp", Service: "rabbitmq"})
	if err != nil {
		t.Fatalf("ExposedPorts: %v", err)
	}
	want := []Port{{4369, "tcp"}, {5672, "tcp"}, {15672, "tcp"}}
	if !slices.Equal(got, want) {
		t.Errorf("ExposedPorts = %v, want %v", got, want)
	}
}
