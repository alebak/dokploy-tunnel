package companion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/alebak/dokploy-tunnel/internal/tunnel"
)

const (
	// authorizeTimeout bounds asking Dokploy about one tunnel request.
	authorizeTimeout = 15 * time.Second
	// openTimeout bounds opening the stream to the target.
	openTimeout = 15 * time.Second
	// pingInterval spaces the pings that keep proxies from closing idle
	// streams.
	pingInterval = 30 * time.Second
	// pingTimeout bounds waiting for one pong.
	pingTimeout = 15 * time.Second
	// chunkBytes is the largest message the companion sends.
	chunkBytes = 32 << 10
)

var (
	// ErrNetworkNotAttachable means a Bridge cannot join the network the
	// target is reachable on.
	ErrNetworkNotAttachable = errors.New("the target's network is not attachable")
	// ErrTargetUnreachable means a Bridge could not open a stream to the
	// target.
	ErrTargetUnreachable = errors.New("the target is unreachable")
)

// Bridge opens TCP streams to authorized targets.
type Bridge interface {
	// Open connects to the port of target. ctx bounds opening only; the
	// stream lives until it is closed, and closing it must unblock its
	// pending reads and writes. Errors that wrap
	// ErrNetworkNotAttachable or context.DeadlineExceeded are reported to
	// the client as such; any other error as an unreachable target.
	Open(ctx context.Context, target Target) (io.ReadWriteCloser, error)
}

// UnavailableBridge is a Bridge that opens nothing, for a companion that
// cannot forward yet.
type UnavailableBridge struct{}

// Open implements Bridge.
func (UnavailableBridge) Open(context.Context, Target) (io.ReadWriteCloser, error) {
	return nil, fmt.Errorf("%w: forwarding is not implemented yet", ErrTargetUnreachable)
}

// Server serves the tunnel protocol of docs/protocol.md.
type Server struct {
	auth   *Authorizer
	bridge Bridge
	log    *slog.Logger
	mux    *http.ServeMux

	mu       sync.Mutex
	draining bool
	// drain is closed when draining starts, to close every stream.
	drain   chan struct{}
	streams sync.WaitGroup
}

// NewServer returns a Server that authorizes tunnels with auth and opens
// them with bridge.
func NewServer(auth *Authorizer, bridge Bridge, log *slog.Logger) *Server {
	s := &Server{auth: auth, bridge: bridge, log: log, mux: http.NewServeMux(), drain: make(chan struct{})}
	s.mux.HandleFunc(tunnel.Path, s.handleTunnel)
	s.mux.HandleFunc(tunnel.HealthPath, s.handleHealth)
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// Drain stops accepting tunnels, closes the open ones with "going away",
// and waits for them to end or for ctx to be done. http.Server.Shutdown
// does not track WebSockets, so a graceful shutdown calls both.
func (s *Server) Drain(ctx context.Context) error {
	s.mu.Lock()
	if !s.draining {
		s.draining = true
		close(s.drain)
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.streams.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for tunnels to close: %w", ctx.Err())
	}
}

// acquire registers a new stream, unless the server is draining.
func (s *Server) acquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining {
		return false
	}
	s.streams.Add(1)
	return true
}

func (s *Server) isDraining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	status, body := http.StatusOK, "ok"
	if s.isDraining() {
		status, body = http.StatusServiceUnavailable, "shutting_down"
	}
	writeJSON(w, status, map[string]string{"status": body})
}

// rejection is a tunnel request refused before the upgrade.
type rejection struct {
	status int
	body   tunnel.ErrorResponse
	// detail is logged, never sent.
	detail error
}

func reject(status int, code tunnel.Code, message string) *rejection {
	return &rejection{status: status, body: tunnel.ErrorResponse{Code: code, Message: message}}
}

func (s *Server) handleTunnel(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	log := s.log.With("remote", r.RemoteAddr)

	target, stream, rej := s.open(r)
	if target.ServiceID != "" {
		log = log.With("target", target.String())
	}
	if rej != nil {
		log.Info("tunnel rejected", "status", rej.status, "code", rej.body.Code, "error", rej.detail)
		writeJSON(w, rej.status, rej.body)
		return
	}
	defer s.streams.Done()

	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		stream.Close()
		log.Info("tunnel upgrade failed", "error", err)
		return
	}
	log.Info("tunnel opened")
	code, reason := s.pump(c, stream)
	log.Info("tunnel closed", "close_code", int(code), "reason", reason, "duration", time.Since(start).Round(time.Millisecond))
}

// open validates, authorizes and opens a tunnel request. On success the
// caller owns stream and one s.streams slot.
func (s *Server) open(r *http.Request) (Target, io.ReadWriteCloser, *rejection) {
	switch {
	case r.Method != http.MethodGet:
		return Target{}, nil, reject(http.StatusMethodNotAllowed, tunnel.CodeMethodNotAllowed, "use GET with a WebSocket upgrade")
	case !isUpgrade(r):
		return Target{}, nil, reject(http.StatusUpgradeRequired, tunnel.CodeUpgradeRequired, "a WebSocket upgrade is required")
	case !sameOrigin(r):
		return Target{}, nil, reject(http.StatusForbidden, tunnel.CodeForbiddenOrigin, "cross-origin requests are not allowed")
	}
	q := r.URL.Query()
	requested, err := tunnel.ParseTarget(q)
	if err != nil {
		return Target{}, nil, reject(http.StatusBadRequest, tunnel.CodeInvalidArgument, err.Error())
	}
	wantServer, err := tunnel.ParseServerID(q)
	if err != nil {
		return Target{Target: requested}, nil, reject(http.StatusBadRequest, tunnel.CodeInvalidArgument, err.Error())
	}
	key := r.Header.Get(tunnel.HeaderAPIKey)
	switch {
	case key == "":
		return Target{Target: requested}, nil, reject(http.StatusUnauthorized, tunnel.CodeUnauthenticated, "missing the "+tunnel.HeaderAPIKey+" header")
	case len(key) > tunnel.MaxAPIKeyBytes:
		return Target{Target: requested}, nil, reject(http.StatusBadRequest, tunnel.CodeInvalidArgument, "the API key is too long")
	}

	if !s.acquire() {
		return Target{Target: requested}, nil, reject(http.StatusServiceUnavailable, tunnel.CodeUnavailable, "the companion is shutting down")
	}
	target, stream, rej := s.authorizeAndOpen(r.Context(), key, requested, wantServer)
	if rej != nil {
		s.streams.Done()
		return Target{Target: requested}, nil, rej
	}
	return target, stream, nil
}

func (s *Server) authorizeAndOpen(ctx context.Context, key string, requested tunnel.Target, wantServer string) (Target, io.ReadWriteCloser, *rejection) {
	authCtx, cancel := context.WithTimeout(ctx, authorizeTimeout)
	defer cancel()
	target, err := s.auth.Authorize(authCtx, key, requested, wantServer)
	if err != nil {
		return Target{}, nil, authRejection(err)
	}

	openCtx, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	stream, err := s.bridge.Open(openCtx, target)
	if err != nil {
		var rej *rejection
		switch {
		case errors.Is(err, ErrNetworkNotAttachable):
			rej = reject(http.StatusConflict, tunnel.CodeNetworkNotAttachable,
				"the service's network cannot be attached; enable \"attachable\" on it in Dokploy")
		case errors.Is(err, context.DeadlineExceeded):
			rej = reject(http.StatusGatewayTimeout, tunnel.CodeTimeout, "the target did not answer in time")
		default:
			rej = reject(http.StatusBadGateway, tunnel.CodeTargetUnreachable, "the target could not be reached")
		}
		rej.detail = err
		return Target{}, nil, rej
	}
	return target, stream, nil
}

// authRejection maps an Authorize error to its response.
func authRejection(err error) *rejection {
	var wrong *WrongServerError
	var rej *rejection
	switch {
	case errors.As(err, &wrong):
		rej = reject(http.StatusMisdirectedRequest, tunnel.CodeWrongServer, wrong.Error())
		rej.body.ExpectedServerID = wrong.Expected
	case errors.Is(err, ErrPermissionDenied):
		rej = reject(http.StatusForbidden, tunnel.CodePermissionDenied, ErrPermissionDenied.Error())
	case errors.Is(err, ErrNotFound):
		rej = reject(http.StatusNotFound, tunnel.CodeNotFound, err.Error())
	case errors.Is(err, ErrTimeout):
		rej = reject(http.StatusGatewayTimeout, tunnel.CodeTimeout, "Dokploy did not answer in time")
	case errors.Is(err, ErrDokployUnreachable):
		rej = reject(http.StatusBadGateway, tunnel.CodeUnreachable, "the Dokploy API could not be reached")
	default:
		rej = reject(http.StatusInternalServerError, tunnel.CodeInternal, "unexpected failure")
	}
	rej.detail = err
	return rej
}

// pump copies between the WebSocket and the target stream until either
// ends or the server drains, then closes both. It returns the close code
// and reason it sent.
func (s *Server) pump(c *websocket.Conn, stream io.ReadWriteCloser) (websocket.StatusCode, string) {
	c.SetReadLimit(tunnel.MaxMessageBytes)
	// The connection's own context is never cancelled: coder/websocket
	// drops the connection without a close frame when it is.
	ctx := context.Background()
	type ending struct {
		code   websocket.StatusCode
		reason string
	}
	ended := make(chan ending, 2)

	// Client to target.
	go func() {
		for {
			typ, r, err := c.Reader(ctx)
			if err != nil {
				ended <- ending{peerCloseCode(err), "client closed"}
				return
			}
			if typ != websocket.MessageBinary {
				ended <- ending{websocket.StatusUnsupportedData, "text messages are not allowed"}
				return
			}
			if _, err := io.Copy(stream, r); err != nil {
				// A message over the read limit has already closed c
				// with StatusMessageTooBig.
				ended <- ending{websocket.StatusInternalError, "writing to the target failed"}
				return
			}
		}
	}()

	// Target to client.
	go func() {
		buf := make([]byte, chunkBytes)
		for {
			n, err := stream.Read(buf)
			if n > 0 {
				if werr := c.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					ended <- ending{websocket.StatusInternalError, "writing to the client failed"}
					return
				}
			}
			if errors.Is(err, io.EOF) {
				ended <- ending{websocket.StatusNormalClosure, "target closed"}
				return
			}
			if err != nil {
				ended <- ending{websocket.StatusInternalError, "reading from the target failed"}
				return
			}
		}
	}()

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	var end ending
	for end.code == 0 {
		select {
		case end = <-ended:
		case <-s.drain:
			end = ending{websocket.StatusGoingAway, "companion shutting down"}
		case <-ticker.C:
			// Pings only generate traffic; a slow pong is not fatal, since
			// a target applying backpressure delays reading it.
			go func() {
				pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
				defer cancel()
				_ = c.Ping(pingCtx)
			}()
		}
	}

	// Closing the stream unblocks the goroutine reading it; closing the
	// WebSocket, the other one. Close is a no-op when c is already closed.
	stream.Close()
	_ = c.Close(end.code, end.reason)
	return end.code, end.reason
}

// peerCloseCode is the close code to answer a failed WebSocket read with.
func peerCloseCode(err error) websocket.StatusCode {
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		return websocket.StatusNormalClosure
	default:
		return websocket.StatusInternalError
	}
}

// isUpgrade reports whether r asks for a WebSocket upgrade.
func isUpgrade(r *http.Request) bool {
	return headerHasToken(r.Header, "Connection", "upgrade") && headerHasToken(r.Header, "Upgrade", "websocket")
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// sameOrigin reports whether r has no Origin header, as from doktunnel, or
// one whose host is the request's, as from a page of the companion itself.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
