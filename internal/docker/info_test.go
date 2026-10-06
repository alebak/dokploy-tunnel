package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// infoDaemon answers /_ping and /info with the data root it holds.
type infoDaemon struct {
	mu       sync.Mutex
	rootDir  string
	status   int
	requests atomic.Int64
}

func (d *infoDaemon) set(rootDir string, status int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rootDir, d.status = rootDir, status
}

func (d *infoDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("API-Version", MaxAPIVersion)
	if r.URL.Path == "/_ping" {
		return
	}
	if !strings.HasSuffix(r.URL.Path, "/info") {
		http.NotFound(w, r)
		return
	}
	d.requests.Add(1)
	d.mu.Lock()
	rootDir, status := d.rootDir, d.status
	d.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if status != 0 && status != http.StatusOK {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "info unavailable"})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"DockerRootDir": rootDir})
}

// clock is a settable time source.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newInfoClient(t *testing.T, d *infoDaemon) (*Client, *clock) {
	t.Helper()
	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)
	c, err := New("tcp://" + srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{now: time.Unix(1_700_000_000, 0)}
	c.now = clk.Now
	return c, clk
}

func infoContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestDockerRootDir_CachesForTTL(t *testing.T) {
	d := &infoDaemon{rootDir: "/data/docker"}
	c, clk := newInfoClient(t, d)
	ctx := infoContext(t)

	for range 3 {
		got, err := c.DockerRootDir(ctx)
		if err != nil || got != "/data/docker" {
			t.Fatalf("DockerRootDir = %q, %v; want /data/docker", got, err)
		}
	}
	if n := d.requests.Load(); n != 1 {
		t.Fatalf("/info asked %d times within the TTL, want 1", n)
	}

	d.set("/srv/docker", 0)
	clk.Advance(RootDirTTL - time.Second)
	if got, _ := c.DockerRootDir(ctx); got != "/data/docker" {
		t.Errorf("DockerRootDir before the TTL = %q, want the cached /data/docker", got)
	}
	clk.Advance(time.Second)
	if got, err := c.DockerRootDir(ctx); err != nil || got != "/srv/docker" {
		t.Errorf("DockerRootDir after the TTL = %q, %v; want the refreshed /srv/docker", got, err)
	}
	if n := d.requests.Load(); n != 2 {
		t.Errorf("/info asked %d times, want 2", n)
	}
}

func TestDockerRootDir_FailsClosed(t *testing.T) {
	d := &infoDaemon{status: http.StatusInternalServerError}
	c, clk := newInfoClient(t, d)
	ctx := infoContext(t)

	if got, err := c.DockerRootDir(ctx); err == nil {
		t.Fatalf("DockerRootDir = %q with /info failing, want an error", got)
	}
	// A failure is not cached: the next call asks again.
	d.set("/var/lib/docker", 0)
	if got, err := c.DockerRootDir(ctx); err != nil || got != "/var/lib/docker" {
		t.Fatalf("DockerRootDir after recovery = %q, %v; want /var/lib/docker", got, err)
	}
	// Once the TTL has passed, a failed refresh never falls back to the
	// stale value.
	d.set("", http.StatusServiceUnavailable)
	clk.Advance(RootDirTTL)
	if got, err := c.DockerRootDir(ctx); err == nil {
		t.Fatalf("DockerRootDir = %q with a failed refresh, want an error", got)
	}
}

func TestDockerRootDir_RejectsUnusableValues(t *testing.T) {
	tests := []struct {
		name, rootDir, want string
	}{
		{"empty", "", ""},
		{"relative", "docker", ""},
		{"Windows path", `C:\ProgramData\docker`, ""},
		{"unclean", "/data//docker/", "/data/docker"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newInfoClient(t, &infoDaemon{rootDir: tt.rootDir})
			got, err := c.DockerRootDir(infoContext(t))
			if tt.want == "" {
				if err == nil {
					t.Fatalf("DockerRootDir = %q for data root %q, want an error", got, tt.rootDir)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("DockerRootDir = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestDockerRootDir_ConcurrentCallsShareOneRead(t *testing.T) {
	d := &infoDaemon{rootDir: "/data/docker"}
	c, _ := newInfoClient(t, d)
	ctx := infoContext(t)

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 32 {
		wg.Go(func() {
			got, err := c.DockerRootDir(ctx)
			if err == nil && got != "/data/docker" {
				err = errUnexpected(got)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := d.requests.Load(); n != 1 {
		t.Errorf("/info asked %d times by concurrent callers, want 1", n)
	}
}

func TestDockerRootDir_HonorsContextWhileWaiting(t *testing.T) {
	c, _ := newInfoClient(t, &infoDaemon{rootDir: "/data/docker"})
	c.rootDirSem <- struct{}{} // another caller is reading /info
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.DockerRootDir(ctx); err == nil {
		t.Fatal("DockerRootDir returned while another read held the cache and the context was done")
	}
}

type errUnexpected string

func (e errUnexpected) Error() string { return "unexpected data root " + string(e) }
