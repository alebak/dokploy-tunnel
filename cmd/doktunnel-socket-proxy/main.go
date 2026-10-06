// Command doktunnel-socket-proxy holds the Docker socket for
// doktunnel-companion and forwards only the Docker API calls the companion
// makes, with their bodies checked, so that the companion never holds the
// root-equivalent socket itself.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/alebak/dokploy-tunnel/internal/dockerproxy"
	"github.com/alebak/dokploy-tunnel/internal/version"
)

const binaryName = "doktunnel-socket-proxy"

func main() {
	cfg, err := dockerproxy.ParseConfig(os.Args[1:], os.Getenv, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", binaryName, err)
		os.Exit(2)
	}
	if cfg.Version {
		fmt.Println(version.String(binaryName))
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	log.Info("starting", "version", version.String(binaryName))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := dockerproxy.Run(ctx, cfg, log); err != nil {
		log.Error("socket proxy failed", "error", err)
		stop()
		os.Exit(1)
	}
}
