package dockerproxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// shutdownTimeout bounds waiting for requests in flight when stopping.
// Exec streams are not waited for: they end with the process.
const shutdownTimeout = 10 * time.Second

// Run serves the proxy configured by cfg until ctx is done.
func Run(ctx context.Context, cfg Config, log *slog.Logger) error {
	p, err := New(cfg.DockerHost, cfg.RepeaterImage, log)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listening: %w", err)
	}
	return serve(ctx, ln, p, log)
}

// serve serves p on ln until ctx is done.
func serve(ctx context.Context, ln net.Listener, p *Proxy, log *slog.Logger) error {
	srv := newServer(p, log, defaultTimeouts)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("serving", "address", ln.Addr().String(), "repeater_image", p.image)
	select {
	case err := <-errc:
		return fmt.Errorf("serving: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("stopping: %w", err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving: %w", err)
	}
	return nil
}

// timeouts bound how long a client may take: to send a request's headers,
// then its body, and to send the next request on a kept-alive connection.
// The server lifts the read deadline once the body is read, and when the
// connection is hijacked, so they bound neither the daemon's answer nor an
// exec's stream; TestServer_Timeouts holds it to that.
type timeouts struct {
	header, body, idle time.Duration
}

var defaultTimeouts = timeouts{header: 10 * time.Second, body: 30 * time.Second, idle: 2 * time.Minute}

// newServer returns the server that serves p.
func newServer(p *Proxy, log *slog.Logger, t timeouts) *http.Server {
	return &http.Server{
		Handler:           p,
		ReadHeaderTimeout: t.header,
		ReadTimeout:       t.header + t.body,
		IdleTimeout:       t.idle,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}
