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
	srv := &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
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
