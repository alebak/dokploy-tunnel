package forward

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"

	"github.com/alebak/dokploy-tunnel/internal/dokploy"
	"github.com/alebak/dokploy-tunnel/internal/tunnel"
)

const (
	testKey    = "test-api-key"
	testPrefix = "/doktunnel"
)

// pgTarget is the target most tests forward to.
var pgTarget = tunnel.Target{ServiceType: dokploy.ServicePostgres, ServiceID: "pg_main", Port: 5432}

// fakeCompanion is a loopback companion. Its tunnel endpoint rejects
// requests whose serviceId has an entry in reject, and otherwise hands the
// accepted WebSocket to stream.
type fakeCompanion struct {
	t      *testing.T
	srv    *httptest.Server
	reject map[string]rejection
	// stream serves an accepted tunnel; it defaults to echo.
	stream func(c *websocket.Conn)
	// ports answers the ports endpoint.
	ports func(w http.ResponseWriter, r *http.Request)

	mu       sync.Mutex
	requests []*http.Request
	// maxMessage is the largest message received on any tunnel.
	maxMessage int
	// closes receives the close status of every tunnel the client ended.
	closes chan websocket.StatusCode
}

type rejection struct {
	status int
	body   string
}

func newFakeCompanion(t *testing.T) *fakeCompanion {
	t.Helper()
	fc := &fakeCompanion{
		t:      t,
		reject: map[string]rejection{},
		closes: make(chan websocket.StatusCode, 64),
	}
	fc.stream = fc.echo
	mux := http.NewServeMux()
	mux.HandleFunc(testPrefix+tunnel.Path, fc.handleTunnel)
	mux.HandleFunc(testPrefix+portsPath, func(w http.ResponseWriter, r *http.Request) {
		fc.record(r)
		fc.ports(w, r)
	})
	fc.srv = httptest.NewServer(mux)
	t.Cleanup(fc.srv.Close)
	return fc
}

// url returns the companion URL, with its path prefix.
func (fc *fakeCompanion) url() string {
	return fc.srv.URL + testPrefix
}

func (fc *fakeCompanion) record(r *http.Request) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.requests = append(fc.requests, r.Clone(context.Background()))
}

func (fc *fakeCompanion) lastRequest() *http.Request {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.requests) == 0 {
		fc.t.Fatal("the companion received no request")
	}
	return fc.requests[len(fc.requests)-1]
}

func (fc *fakeCompanion) handleTunnel(w http.ResponseWriter, r *http.Request) {
	fc.record(r)
	if r.Header.Get(tunnel.HeaderAPIKey) != testKey {
		writeJSON(w, http.StatusUnauthorized, `{"code":"unauthenticated","message":"missing API key"}`)
		return
	}
	if r.Header.Get("Origin") != "" {
		writeJSON(w, http.StatusForbidden, `{"code":"forbidden_origin","message":"origin"}`)
		return
	}
	target, err := tunnel.ParseTarget(r.URL.Query())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, `{"code":"invalid_argument","message":"bad target"}`)
		return
	}
	fc.mu.Lock()
	rej, ok := fc.reject[target.ServiceID]
	fc.mu.Unlock()
	if ok {
		writeJSON(w, rej.status, rej.body)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(tunnel.MaxMessageBytes)
	fc.stream(c)
}

// echo sends every message back and reports how the client closed.
func (fc *fakeCompanion) echo(c *websocket.Conn) {
	ctx := context.Background()
	for {
		typ, msg, err := c.Read(ctx)
		if err != nil {
			fc.closes <- websocket.CloseStatus(err)
			return
		}
		fc.mu.Lock()
		fc.maxMessage = max(fc.maxMessage, len(msg))
		fc.mu.Unlock()
		if err := c.Write(ctx, typ, msg); err != nil {
			return
		}
	}
}

// setReject makes the tunnel endpoint refuse serviceID, or accept it again
// when rej is nil.
func (fc *fakeCompanion) setReject(serviceID string, rej *rejection) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if rej == nil {
		delete(fc.reject, serviceID)
		return
	}
	fc.reject[serviceID] = *rej
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// mustJSON encodes v for a fake answer.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// queryOf returns the query of r without the parameters' order.
func queryOf(r *http.Request) url.Values {
	return r.URL.Query()
}

// hasPrefixPath reports whether r was sent under the companion's prefix.
func hasPrefixPath(r *http.Request, path string) bool {
	return r.URL.Path == testPrefix+path && strings.HasPrefix(r.URL.Path, testPrefix+"/")
}
