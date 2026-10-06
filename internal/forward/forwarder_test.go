package forward

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
	"github.com/alebak/dokploy-tunnel/internal/tunnel"
)

// waitTimeout bounds every wait in these tests.
const waitTimeout = 10 * time.Second

// startForwarder listens on a free loopback port and serves until the test
// ends. Events are delivered on the returned channel.
func startForwarder(t *testing.T, fc *fakeCompanion, target tunnel.Target) (*Forwarder, <-chan Event) {
	t.Helper()
	events := make(chan Event, 256)
	f, err := newTestClient(t, fc.url()).Listen(Config{
		Addr:    "127.0.0.1:0",
		Target:  target,
		OnEvent: func(e Event) { events <- e },
	})
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	served := make(chan error, 1)
	go func() { served <- f.Serve(context.Background()) }()
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("Serve() error = %v", err)
			}
		case <-time.After(waitTimeout):
			t.Error("Serve() did not return after Close()")
		}
	})
	return f, events
}

func dialLocal(t *testing.T, f *Forwarder) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", f.Addr().String(), waitTimeout)
	if err != nil {
		t.Fatalf("dialing the forwarder: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(waitTimeout))
	return conn
}

// waitEvent returns the next event of kind, skipping others.
func waitEvent(t *testing.T, events <-chan Event, kind EventKind) Event {
	t.Helper()
	deadline := time.After(waitTimeout)
	for {
		select {
		case e := <-events:
			if e.Kind == kind {
				return e
			}
		case <-deadline:
			t.Fatalf("no event of kind %d", kind)
		}
	}
}

func waitClose(t *testing.T, fc *fakeCompanion) websocket.StatusCode {
	t.Helper()
	select {
	case code := <-fc.closes:
		return code
	case <-time.After(waitTimeout):
		t.Fatal("the companion saw no close")
		return 0
	}
}

// expectEOF fails unless conn is closed by the forwarder.
func expectEOF(t *testing.T, conn net.Conn) {
	t.Helper()
	buf := make([]byte, 1)
	if n, err := conn.Read(buf); err == nil {
		t.Fatalf("Read() = %d bytes, want the connection closed", n)
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("the local connection was not closed")
	}
}

func TestForwarder_EchoRoundTrip(t *testing.T) {
	fc := newFakeCompanion(t)
	f, events := startForwarder(t, fc, pgTarget)
	conn := dialLocal(t, f)

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("reading the echo: %v", err)
	}
	if string(got) != "ping" {
		t.Errorf("echo = %q, want %q", got, "ping")
	}
	if e := waitEvent(t, events, EventOpened); e.ID != 1 || e.Client == nil {
		t.Errorf("opened event = %+v, want ID 1 and the client address", e)
	}

	r := fc.lastRequest()
	if !hasPrefixPath(r, tunnel.Path) {
		t.Errorf("tunnel path = %q, want %q", r.URL.Path, testPrefix+tunnel.Path)
	}
	if q := r.URL.Query(); q.Get(tunnel.ParamPort) != "5432" || q.Get(tunnel.ParamServiceID) != "pg_main" {
		t.Errorf("tunnel query = %v, want pg_main port 5432", q)
	}
}

func TestForwarder_LargePayloadIsSplitIntoFrames(t *testing.T) {
	fc := newFakeCompanion(t)
	f, _ := startForwarder(t, fc, pgTarget)
	conn := dialLocal(t, f)

	payload := make([]byte, 300<<10)
	_, _ = rand.Read(payload)
	go func() { _, _ = conn.Write(payload) }()
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("reading the echo: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("the echoed payload differs from the one sent")
	}
	fc.mu.Lock()
	maxMessage := fc.maxMessage
	fc.mu.Unlock()
	if maxMessage > chunkBytes {
		t.Errorf("largest message = %d bytes, want at most %d", maxMessage, chunkBytes)
	}
}

func TestForwarder_ConcurrentConnections(t *testing.T) {
	fc := newFakeCompanion(t)
	f, _ := startForwarder(t, fc, pgTarget)

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		conn := dialLocal(t, f)
		wg.Add(1)
		go func() {
			defer wg.Done()
			msg := bytes.Repeat([]byte(fmt.Sprintf("conn-%d;", i)), 5000)
			go func() { _, _ = conn.Write(msg) }()
			got := make([]byte, len(msg))
			if _, err := io.ReadFull(conn, got); err != nil {
				errs <- fmt.Errorf("connection %d: %w", i, err)
				return
			}
			if !bytes.Equal(got, msg) {
				errs <- fmt.Errorf("connection %d received another connection's bytes", i)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestForwarder_PreUpgradeErrorClosesOnlyThatConnection(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantCode tunnel.Code
		wantCLI  clierr.Code
		wantText string
	}{
		{"permission denied", http.StatusForbidden,
			`{"code":"permission_denied","message":"the API key cannot read this service"}`,
			tunnel.CodePermissionDenied, clierr.PermissionDenied, "cannot read"},
		{"wrong server", http.StatusMisdirectedRequest,
			`{"code":"wrong_server","message":"another server","expected_server_id":"srv_edge"}`,
			tunnel.CodeWrongServer, clierr.Unreachable, "srv_edge"},
		{"network not attachable", http.StatusConflict,
			`{"code":"network_not_attachable","message":"not attachable"}`,
			tunnel.CodeNetworkNotAttachable, clierr.NetworkNotAttachable, "attachable"},
		{"too many tunnels", http.StatusTooManyRequests,
			`{"code":"too_many_tunnels","message":"too many tunnels are open"}`,
			codeTooManyTunnels, clierr.Unreachable, "too many"},
		{"proxy page", http.StatusBadGateway, `<html>bad gateway</html>`,
			"", clierr.Unreachable, "HTTP 502"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := newFakeCompanion(t)
			fc.setReject("pg_denied", &rejection{tt.status, tt.body})
			denied := pgTarget
			denied.ServiceID = "pg_denied"
			f, events := startForwarder(t, fc, denied)

			expectEOF(t, dialLocal(t, f))
			e := waitEvent(t, events, EventFailed)
			var ce *CompanionError
			if !errors.As(e.Err, &ce) || ce.Code != tt.wantCode {
				t.Fatalf("failed event error = %v, want a *CompanionError with code %q", e.Err, tt.wantCode)
			}
			cli := CLIError(e.Err)
			if cli.Code != tt.wantCLI {
				t.Errorf("CLIError().Code = %q, want %q", cli.Code, tt.wantCLI)
			}
			if !bytes.Contains([]byte(cli.Message+cli.Hint), []byte(tt.wantText)) {
				t.Errorf("CLIError() = %q (hint %q), want it to mention %q", cli.Message, cli.Hint, tt.wantText)
			}

			// The forwarder keeps listening once the target is allowed.
			fc.setReject("pg_denied", nil)
			conn := dialLocal(t, f)
			_, _ = conn.Write([]byte("again"))
			got := make([]byte, 5)
			if _, err := io.ReadFull(conn, got); err != nil || string(got) != "again" {
				t.Errorf("after a refusal, echo = %q, %v; want %q", got, err, "again")
			}
		})
	}
}

func TestForwarder_SendsNoOriginAndTheAPIKey(t *testing.T) {
	// The fake rejects a missing key and any Origin; a working echo proves
	// the headers.
	fc := newFakeCompanion(t)
	f, _ := startForwarder(t, fc, pgTarget)
	conn := dialLocal(t, f)
	_, _ = conn.Write([]byte("x"))
	if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
		t.Fatalf("echo failed: %v", err)
	}
	if got := fc.lastRequest().Header.Get("User-Agent"); got == "" {
		t.Error("no User-Agent header")
	}
}

func TestForwarder_CompanionCloseClosesLocalConnection(t *testing.T) {
	for name, code := range map[string]websocket.StatusCode{
		"normal":      websocket.StatusNormalClosure,
		"going away":  websocket.StatusGoingAway,
		"target fail": websocket.StatusInternalError,
	} {
		t.Run(name, func(t *testing.T) {
			fc := newFakeCompanion(t)
			fc.stream = func(c *websocket.Conn) {
				_ = c.Write(context.Background(), websocket.MessageBinary, []byte("bye"))
				_ = c.Close(code, "target closed")
			}
			f, events := startForwarder(t, fc, pgTarget)
			conn := dialLocal(t, f)

			got := make([]byte, 3)
			if _, err := io.ReadFull(conn, got); err != nil || string(got) != "bye" {
				t.Fatalf("read = %q, %v; want %q", got, err, "bye")
			}
			expectEOF(t, conn)
			e := waitEvent(t, events, EventClosed)
			if e.Received != 3 {
				t.Errorf("closed event Received = %d, want 3", e.Received)
			}
			if wantErr := code == websocket.StatusInternalError; (e.Err != nil) != wantErr {
				t.Errorf("closed event Err = %v, want an error: %v", e.Err, wantErr)
			}
		})
	}
}

func TestForwarder_TextMessageEndsTunnel(t *testing.T) {
	fc := newFakeCompanion(t)
	fc.stream = func(c *websocket.Conn) {
		ctx := context.Background()
		_ = c.Write(ctx, websocket.MessageText, []byte("hello"))
		_, _, err := c.Read(ctx)
		fc.closes <- websocket.CloseStatus(err)
	}
	f, _ := startForwarder(t, fc, pgTarget)
	conn := dialLocal(t, f)
	expectEOF(t, conn)
	if got := waitClose(t, fc); got != websocket.StatusUnsupportedData {
		t.Errorf("close code = %d, want %d", got, websocket.StatusUnsupportedData)
	}
}

func TestForwarder_LocalCloseSendsNormalClosure(t *testing.T) {
	fc := newFakeCompanion(t)
	f, events := startForwarder(t, fc, pgTarget)
	conn := dialLocal(t, f)
	_, _ = conn.Write([]byte("abc"))
	if _, err := io.ReadFull(conn, make([]byte, 3)); err != nil {
		t.Fatal(err)
	}
	conn.Close()

	if got := waitClose(t, fc); got != websocket.StatusNormalClosure {
		t.Errorf("close code = %d, want %d", got, websocket.StatusNormalClosure)
	}
	e := waitEvent(t, events, EventClosed)
	if e.Err != nil || e.Sent != 3 || e.Received != 3 || e.Reason != "client closed" {
		t.Errorf("closed event = %+v, want a normal end with 3 bytes each way", e)
	}
}

func TestForwarder_CloseTearsDownActiveTunnels(t *testing.T) {
	fc := newFakeCompanion(t)
	f, err := newTestClient(t, fc.url()).Listen(Config{Addr: "127.0.0.1:0", Target: pgTarget})
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- f.Serve(context.Background()) }()

	conns := []net.Conn{dialLocal(t, f), dialLocal(t, f)}
	for _, conn := range conns {
		_, _ = conn.Write([]byte("up"))
		if _, err := io.ReadFull(conn, make([]byte, 2)); err != nil {
			t.Fatal(err)
		}
	}

	start := time.Now()
	if err := f.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Close() took %v, want it prompt", d)
	}
	for range conns {
		if got := waitClose(t, fc); got != websocket.StatusGoingAway {
			t.Errorf("close code = %d, want %d", got, websocket.StatusGoingAway)
		}
	}
	for _, conn := range conns {
		expectEOF(t, conn)
	}
	if err := <-served; err != nil {
		t.Errorf("Serve() error = %v", err)
	}
	if _, err := net.DialTimeout("tcp", f.Addr().String(), time.Second); err == nil {
		t.Error("the forwarder still accepts connections after Close()")
	}
	if err := f.Close(); err != nil {
		t.Errorf("second Close() error = %v", err)
	}
}

func TestForwarder_ServeStopsWhenContextIsCancelled(t *testing.T) {
	fc := newFakeCompanion(t)
	f, err := newTestClient(t, fc.url()).Listen(Config{Addr: "127.0.0.1:0", Target: pgTarget})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- f.Serve(ctx) }()

	conn := dialLocal(t, f)
	_, _ = conn.Write([]byte("up"))
	if _, err := io.ReadFull(conn, make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve() error = %v", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Serve() did not return after the context was cancelled")
	}
	if got := waitClose(t, fc); got != websocket.StatusGoingAway {
		t.Errorf("close code = %d, want %d", got, websocket.StatusGoingAway)
	}
}

func TestListen_RefusesNonLoopbackAddresses(t *testing.T) {
	c := newTestClient(t, "https://dokploy.example.com/doktunnel")
	for _, addr := range []string{
		"0.0.0.0:5432",
		"[::]:5432",
		"192.168.1.10:5432",
		"10.0.0.1:5432",
		"localhost:5432",
		"127.0.0.1",
		"127.0.0.1:http",
		"127.0.0.1:70000",
		"[fe80::1%lo]:5432",
	} {
		t.Run(addr, func(t *testing.T) {
			f, err := c.Listen(Config{Addr: addr, Target: pgTarget})
			if err == nil {
				f.Close()
				t.Fatalf("Listen(%q) error = nil, want a refusal", addr)
			}
			if !errors.Is(err, ErrInvalidListenAddress) {
				t.Errorf("Listen(%q) error = %v, want ErrInvalidListenAddress", addr, err)
			}
		})
	}
}

func TestListen_RefusesTargetWithoutPort(t *testing.T) {
	c := newTestClient(t, "https://dokploy.example.com/doktunnel")
	target := pgTarget
	target.Port = 0
	if f, err := c.Listen(Config{Addr: "127.0.0.1:0", Target: target}); err == nil {
		f.Close()
		t.Fatal("Listen() error = nil, want a refusal of port 0")
	}
}
