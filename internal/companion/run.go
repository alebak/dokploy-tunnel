package companion

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/dokploy"
)

const (
	// shutdownTimeout bounds a graceful shutdown.
	shutdownTimeout = 30 * time.Second
	// maxHeaderBytes bounds the request line and headers of a request.
	maxHeaderBytes = 16 << 10
)

// Run serves the companion on cfg.Listen until ctx is done, then shuts
// down gracefully.
func Run(ctx context.Context, cfg Config, bridge Bridge, log *slog.Logger) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.Listen, err)
	}
	return Serve(ctx, ln, cfg, bridge, log)
}

// Serve serves the companion on ln until ctx is done, then stops accepting
// connections, closes open tunnels with "going away", and waits up to 30
// seconds for them to end.
func Serve(ctx context.Context, ln net.Listener, cfg Config, bridge Bridge, log *slog.Logger) error {
	auth := NewAuthorizer(dokploy.New(cfg.DokployURL, ""), cfg.ServerID)
	srv := NewServer(auth, bridge, log, Limits{PerKey: cfg.MaxTunnelsPerKey, Total: cfg.MaxTunnels})
	httpSrv := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	served := make(chan error, 1)
	go func() { served <- httpSrv.Serve(ln) }()
	log.Info("companion listening", "address", ln.Addr().String(),
		"dokploy_url", cfg.DokployURL.String(), "server_id", serverName(cfg.ServerID))

	select {
	case err := <-served:
		return fmt.Errorf("serving: %w", err)
	case <-ctx.Done():
	}
	log.Info("companion shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	// Drain closes the WebSockets, which Shutdown does not track.
	errDrain := srv.Drain(shutdownCtx)
	errShutdown := httpSrv.Shutdown(shutdownCtx)
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving: %w", err)
	}
	return errors.Join(errDrain, errShutdown)
}
