package dockerproxy

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/docker"
	"github.com/alebak/dokploy-tunnel/internal/docker/dockertest"
	"github.com/alebak/dokploy-tunnel/internal/repeater"
)

func TestServe_StopsWithItsContext(t *testing.T) {
	fake := dockertest.New(t)
	p, err := New(fake.Host(), repeater.DefaultImage, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, ln, p, slog.New(slog.DiscardHandler)) }()

	client, err := docker.New("tcp://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()
	if err := client.Ping(pingCtx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve = %v, want nil after the context ends", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop")
	}
}
