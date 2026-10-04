package companion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/alebak/dokploy-tunnel/internal/dokploy"
	"github.com/alebak/dokploy-tunnel/internal/tunnel"
)

// pipeBridge is an in-memory Bridge: every Open returns one end of a
// net.Pipe and hands the other end, the target's, to the test.
type pipeBridge struct {
	err     error
	targets chan Target
	ends    chan net.Conn
}

func newPipeBridge() *pipeBridge {
	return &pipeBridge{targets: make(chan Target, 8), ends: make(chan net.Conn, 8)}
}

func (b *pipeBridge) Open(_ context.Context, target Target) (io.ReadWriteCloser, error) {
	b.targets <- target
	if b.err != nil {
		return nil, b.err
	}
	companionEnd, targetEnd := net.Pipe()
	b.ends <- targetEnd
	return companionEnd, nil
}

// opened reports whether Open was called.
func (b *pipeBridge) opened() bool {
	return len(b.targets) > 0
}

// target returns the target end of the next stream.
func (b *pipeBridge) target(t *testing.T) net.Conn {
	t.Helper()
	select {
	case c := <-b.ends:
		t.Cleanup(func() { c.Close() })
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("the bridge was not opened")
		return nil
	}
}

// syncBuffer is a log destination safe for concurrent use.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testCompanion is a companion for the main Dokploy server, served by
// httptest, with a fake Dokploy API and an in-memory bridge.
type testCompanion struct {
	fake   *fakeDokploy
	bridge *pipeBridge
	server *Server
	http   *httptest.Server
	logs   *syncBuffer
}

func newTestCompanion(t *testing.T) *testCompanion {
	t.Helper()
	tc := &testCompanion{fake: newFakeDokploy(t), bridge: newPipeBridge(), logs: &syncBuffer{}}
	log := slog.New(slog.NewTextHandler(tc.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tc.server = NewServer(NewAuthorizer(tc.fake.start(), ""), tc.bridge, log)
	tc.http = httptest.NewServer(tc.server)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tc.server.Drain(ctx); err != nil {
			t.Errorf("Drain: %v", err)
		}
		tc.http.Close()
		for _, key := range []string{ownerKey, memberKey, foreignKey} {
			if strings.Contains(tc.logs.String(), key) {
				t.Errorf("the logs contain the API key %q", key)
			}
		}
	})
	return tc
}

func (tc *testCompanion) url(scheme, query string) string {
	return strings.Replace(tc.http.URL, "http", scheme, 1) + tunnel.Path + "?" + query
}

// dial opens a tunnel with key.
func (tc *testCompanion) dial(t *testing.T, key, query string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, tc.url("ws", query), &websocket.DialOptions{
		HTTPHeader: http.Header{tunnel.HeaderAPIKey: {key}},
	})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dialing the tunnel: HTTP %d: %v", status, err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

const pgQuery = "serviceType=postgres&serviceId=pg_main&port=5432"

func TestServer_Tunnel_CarriesTheStream(t *testing.T) {
	tc := newTestCompanion(t)
	c := tc.dial(t, ownerKey, pgQuery)
	target := tc.bridge.target(t)

	got := <-tc.bridge.targets
	if got.Target != (tunnel.Target{ServiceType: dokploy.ServicePostgres, ServiceID: "pg_main", Port: 5432}) {
		t.Errorf("bridge target = %+v", got.Target)
	}
	if got.Service.AppName != "shop-pgmain-a1b2c3" {
		t.Errorf("bridge appName = %q", got.Service.AppName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Client to target.
	if err := c.Write(ctx, websocket.MessageBinary, []byte("SELECT 1;")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := target.Read(buf)
	if err != nil || string(buf[:n]) != "SELECT 1;" {
		t.Fatalf("target read %q, %v", buf[:n], err)
	}

	// Target to client.
	go target.Write([]byte("1 row"))
	typ, msg, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || string(msg) != "1 row" {
		t.Fatalf("client read %v %q, %v", typ, msg, err)
	}

	// Closing the WebSocket closes the target stream.
	if err := c.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("Close: %v", err)
	}
	target.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := target.Read(buf); !errors.Is(err, io.EOF) {
		t.Errorf("target read after close = %v, want EOF", err)
	}
}

func TestServer_Tunnel_ComposeService(t *testing.T) {
	tc := newTestCompanion(t)
	tc.dial(t, ownerKey, "serviceType=compose_service&serviceId=cmp_myapp%2Fpostgres&port=5432")
	tc.bridge.target(t)

	got := <-tc.bridge.targets
	if got.ComposeService != "postgres" || got.Service.AppName != "shop-cmpmyapp-a1b2c3" {
		t.Errorf("bridge target = %+v", got)
	}
}

func TestServer_Tunnel_CloseCodes(t *testing.T) {
	t.Run("target closes", func(t *testing.T) {
		tc := newTestCompanion(t)
		c := tc.dial(t, ownerKey, pgQuery)
		tc.bridge.target(t).Close()
		assertClosed(t, c, websocket.StatusNormalClosure)
	})

	t.Run("text message", func(t *testing.T) {
		tc := newTestCompanion(t)
		c := tc.dial(t, ownerKey, pgQuery)
		tc.bridge.target(t)
		if err := c.Write(context.Background(), websocket.MessageText, []byte("hello")); err != nil {
			t.Fatal(err)
		}
		assertClosed(t, c, websocket.StatusUnsupportedData)
	})

	t.Run("message too big", func(t *testing.T) {
		tc := newTestCompanion(t)
		c := tc.dial(t, ownerKey, pgQuery)
		target := tc.bridge.target(t)
		go io.Copy(io.Discard, target)
		big := make([]byte, tunnel.MaxMessageBytes+1)
		_ = c.Write(context.Background(), websocket.MessageBinary, big)
		assertClosed(t, c, websocket.StatusMessageTooBig)
	})

	t.Run("companion drains", func(t *testing.T) {
		tc := newTestCompanion(t)
		c := tc.dial(t, ownerKey, pgQuery)
		target := tc.bridge.target(t)

		done := make(chan error, 1)
		go func() { done <- tc.server.Drain(context.Background()) }()
		assertClosed(t, c, websocket.StatusGoingAway)
		if err := <-done; err != nil {
			t.Fatalf("Drain: %v", err)
		}
		target.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := target.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Errorf("target read after drain = %v, want EOF", err)
		}

		resp := request(t, http.MethodGet, tc.url("http", pgQuery), ownerKey, upgradeHeaders())
		assertRejected(t, resp, http.StatusServiceUnavailable, tunnel.CodeUnavailable)
		health, err := http.Get(tc.http.URL + tunnel.HealthPath)
		if err != nil {
			t.Fatal(err)
		}
		health.Body.Close()
		if health.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("health while draining = %d, want 503", health.StatusCode)
		}
	})
}

// assertClosed reads from c until it is closed, and checks the close code.
func assertClosed(t *testing.T, c *websocket.Conn, want websocket.StatusCode) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, _, err := c.Read(ctx)
		if err == nil {
			continue
		}
		if got := websocket.CloseStatus(err); got != want {
			t.Fatalf("closed with %v (%v), want %v", got, err, want)
		}
		return
	}
}

func upgradeHeaders() http.Header {
	return http.Header{
		"Connection":            {"Upgrade"},
		"Upgrade":               {"websocket"},
		"Sec-Websocket-Version": {"13"},
		"Sec-Websocket-Key":     {"dGhlIHNhbXBsZSBub25jZQ=="},
	}
}

// request sends a plain HTTP request with key and headers.
func request(t *testing.T, method, url, key string, headers http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = http.Header{}
	}
	if key != "" {
		req.Header.Set(tunnel.HeaderAPIKey, key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// assertRejected checks a rejection's status and JSON body, and returns
// the body.
func assertRejected(t *testing.T, resp *http.Response, status int, code tunnel.Code) tunnel.ErrorResponse {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var body tunnel.ErrorResponse
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("body %q is not a JSON error: %v", raw, err)
	}
	if resp.StatusCode != status || body.Code != code {
		t.Fatalf("got HTTP %d %+v, want HTTP %d %s", resp.StatusCode, body, status, code)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if body.Message == "" {
		t.Error("empty message")
	}
	for _, key := range []string{ownerKey, memberKey, foreignKey} {
		if strings.Contains(string(raw), key) {
			t.Errorf("body %q contains the API key", raw)
		}
	}
	return body
}

func TestServer_Tunnel_Rejected(t *testing.T) {
	compose := "serviceType=compose_service&serviceId=cmp_myapp%2Fredis&port=6379"
	tests := []struct {
		name    string
		method  string
		key     string
		query   string
		headers http.Header
		status  int
		code    tunnel.Code
	}{
		{"not a GET", http.MethodPost, ownerKey, pgQuery, upgradeHeaders(), 405, tunnel.CodeMethodNotAllowed},
		{"not an upgrade", http.MethodGet, ownerKey, pgQuery, nil, 426, tunnel.CodeUpgradeRequired},
		{"cross-site origin", http.MethodGet, ownerKey, pgQuery, withOrigin("https://evil.example.com"), 403, tunnel.CodeForbiddenOrigin},
		{"malformed target", http.MethodGet, ownerKey, "serviceType=postgres&serviceId=pg_main&port=0", upgradeHeaders(), 400, tunnel.CodeInvalidArgument},
		{"malformed serverId", http.MethodGet, ownerKey, pgQuery + "&serverId=a%2Fb", upgradeHeaders(), 400, tunnel.CodeInvalidArgument},
		{"key too long", http.MethodGet, strings.Repeat("k", tunnel.MaxAPIKeyBytes+1), pgQuery, upgradeHeaders(), 400, tunnel.CodeInvalidArgument},
		{"no key", http.MethodGet, "", pgQuery, upgradeHeaders(), 401, tunnel.CodeUnauthenticated},
		{"invalid key", http.MethodGet, "dok_revoked", pgQuery, upgradeHeaders(), 403, tunnel.CodePermissionDenied},
		{"key of another organization", http.MethodGet, foreignKey, pgQuery, upgradeHeaders(), 403, tunnel.CodePermissionDenied},
		{"member without access", http.MethodGet, memberKey, pgQuery, upgradeHeaders(), 403, tunnel.CodePermissionDenied},
		{"unknown service", http.MethodGet, ownerKey, "serviceType=postgres&serviceId=pg_gone&port=5432", upgradeHeaders(), 403, tunnel.CodePermissionDenied},
		{"unknown compose service", http.MethodGet, ownerKey, compose, upgradeHeaders(), 404, tunnel.CodeNotFound},
		{"service on another server", http.MethodGet, ownerKey, "serviceType=postgres&serviceId=pg_edge&port=5432", upgradeHeaders(), 421, tunnel.CodeWrongServer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := newTestCompanion(t)
			resp := request(t, tt.method, tc.url("http", tt.query), tt.key, tt.headers)
			body := assertRejected(t, resp, tt.status, tt.code)
			if tt.code == tunnel.CodeWrongServer && body.ExpectedServerID != "srv_edge" {
				t.Errorf("expected_server_id = %q, want srv_edge", body.ExpectedServerID)
			}
			if tc.bridge.opened() {
				t.Error("the bridge was opened for a rejected request")
			}
		})
	}
}

func TestServer_Tunnel_RejectedBeforeDokploy(t *testing.T) {
	tc := newTestCompanion(t)
	for _, query := range []string{"serviceType=postgres&port=5432", pgQuery} {
		request(t, http.MethodGet, tc.url("http", query), "", upgradeHeaders())
	}
	if calls := tc.fake.recorded(); len(calls) != 0 {
		t.Errorf("Dokploy was called for unauthenticated or malformed requests: %v", calls)
	}
}

func TestServer_Tunnel_FailuresAfterAuthorization(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*testCompanion)
		status int
		code   tunnel.Code
	}{
		{"Dokploy is broken", func(tc *testCompanion) { tc.fake.broken = true }, 502, tunnel.CodeUnreachable},
		{"network not attachable", func(tc *testCompanion) {
			tc.bridge.err = errors.Join(ErrNetworkNotAttachable, errors.New("network net_backend"))
		}, 409, tunnel.CodeNetworkNotAttachable},
		{"target unreachable", func(tc *testCompanion) { tc.bridge.err = errors.New("repeater failed") }, 502, tunnel.CodeTargetUnreachable},
		{"target timeout", func(tc *testCompanion) { tc.bridge.err = context.DeadlineExceeded }, 504, tunnel.CodeTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := newTestCompanion(t)
			tt.setup(tc)
			resp := request(t, http.MethodGet, tc.url("http", pgQuery), ownerKey, upgradeHeaders())
			assertRejected(t, resp, tt.status, tt.code)
		})
	}
}

func withOrigin(origin string) http.Header {
	h := upgradeHeaders()
	h.Set("Origin", origin)
	return h
}

func TestServer_Health(t *testing.T) {
	tc := newTestCompanion(t)
	resp, err := http.Get(tc.http.URL + tunnel.HealthPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct{ Status string }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || body.Status != "ok" {
		t.Errorf("health = %d %+v, want 200 ok", resp.StatusCode, body)
	}
}
