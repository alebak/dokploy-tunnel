package companion

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/tunnel"
)

func TestServe_ShutsDownWithTheContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	panel, _ := url.Parse("http://127.0.0.1:1")
	cfg := Config{DokployURL: panel}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ln, cfg, UnavailableBridge{}, slog.New(slog.DiscardHandler))
	}()

	resp, err := http.Get("http://" + ln.Addr().String() + tunnel.HealthPath)
	if err != nil {
		t.Fatal(err)
	}
	var body struct{ Status string }
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || body.Status != "ok" {
		t.Errorf("health = %d %+v", resp.StatusCode, body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the context was cancelled")
	}
}
