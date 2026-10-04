// Command doktunnel-companion is the server-side half of dokploy-tunnel. A
// Dokploy administrator runs one on each Dokploy server; it serves the
// tunnel WebSocket that doktunnel connects to.
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

	"github.com/alebak/dokploy-tunnel/internal/companion"
	"github.com/alebak/dokploy-tunnel/internal/version"
)

const binaryName = "doktunnel-companion"

func main() {
	cfg, err := companion.ParseConfig(os.Args[1:], os.Getenv, os.Stderr)
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
	// Forwarding through Docker is not implemented yet: every authorized
	// tunnel is refused with target_unreachable.
	if err := companion.Run(ctx, cfg, companion.UnavailableBridge{}, log); err != nil {
		log.Error("companion failed", "error", err)
		stop()
		os.Exit(1)
	}
}
