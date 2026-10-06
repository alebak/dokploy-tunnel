package repeater

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/docker"
	"github.com/alebak/dokploy-tunnel/internal/docker/dockertest"
)

const testImage = "alpine/socat:test"

// testKey is the ownership key of the companion under test.
var testKey = []byte("0123456789abcdef0123456789abcdef")

// endpointOn is a target at addr on n.
func endpointOn(n docker.Network, addr string) Endpoint {
	return Endpoint{Addr: netip.MustParseAddr(addr), Name: "target-" + addr, Network: n}
}

// Addresses of targets on composeDefault and dokployNetwork.
const (
	pgAddr  = "172.20.0.5"
	webAddr = "10.0.1.7"
)

func newRepeater(t *testing.T, fake *dockertest.Fake, opts Options) *Repeater {
	t.Helper()
	if opts.Image == "" {
		opts.Image = testImage
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Key == nil {
		opts.Key = testKey
	}
	r := New(newClient(t, fake), opts)
	t.Cleanup(func() { _ = r.Close(testContext(t)) })
	return r
}

// eventually waits for cond, failing the test after a few seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAcquire_CreatesAHardenedLabeledRepeater(t *testing.T) {
	fake := newFake(t)
	fake.AddImage(testImage)
	r := newRepeater(t, fake, Options{Owner: "companion-a"})

	id, release, err := r.acquire(testContext(t), endpointOn(composeDefault, pgAddr))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	c, ok := fake.Container(id)
	if !ok || !c.State.Running {
		t.Fatalf("repeater %s is not running", id)
	}
	created := fake.Created()
	if len(created) != 1 {
		t.Fatalf("created %d containers, want 1", len(created))
	}
	cfg := created[0]
	if cfg.Image != testImage || !slices.Equal(cfg.Entrypoint, []string{"sleep"}) || !slices.Equal(cfg.Cmd, []string{"infinity"}) {
		t.Errorf("image and command = %q %q %q", cfg.Image, cfg.Entrypoint, cfg.Cmd)
	}
	if cfg.HostConfig.NetworkMode != composeDefault.ID {
		t.Errorf("NetworkMode = %q, want only the target's network %q", cfg.HostConfig.NetworkMode, composeDefault.ID)
	}
	hc := cfg.HostConfig
	if cfg.User == "" || cfg.User == "0" || !hc.ReadonlyRootfs || !slices.Equal(hc.CapDrop, []string{"ALL"}) ||
		!slices.Contains(hc.SecurityOpt, "no-new-privileges") {
		t.Errorf("repeater is not hardened: %+v", cfg)
	}
	if hc.Init == nil || !*hc.Init || hc.PidsLimit == nil || *hc.PidsLimit != repeaterPidsLimit || hc.Memory != repeaterMemoryBytes {
		t.Errorf("repeater resources are not limited: init %v, pids %v, memory %d", hc.Init, hc.PidsLimit, hc.Memory)
	}
	for k, want := range map[string]string{
		LabelRepeater: "1",
		LabelOwner:    "companion-a",
		LabelTarget:   "target-" + pgAddr,
		LabelNetwork:  "myapp_default",
	} {
		if got := cfg.Labels[k]; got != want {
			t.Errorf("label %s = %q, want %q", k, got, want)
		}
	}
	if _, err := time.Parse(time.RFC3339, cfg.Labels[LabelCreatedAt]); err != nil {
		t.Errorf("label %s = %q: %v", LabelCreatedAt, cfg.Labels[LabelCreatedAt], err)
	}
	if want := ownershipProof(testKey, strings.TrimPrefix(c.Name, "/")); cfg.Labels[LabelOwnership] != want {
		t.Errorf("label %s = %q, want the proof %q for name %s", LabelOwnership, cfg.Labels[LabelOwnership], want, c.Name)
	}
}

func TestAcquire_SharesOneRepeaterPerTarget(t *testing.T) {
	fake := newFake(t)
	fake.AddImage(testImage)
	r := newRepeater(t, fake, Options{Grace: time.Hour})
	ep := endpointOn(composeDefault, pgAddr)

	const n = 8
	ids := make([]string, n)
	releases := make([]func(), n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			id, release, err := r.acquire(testContext(t), ep)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			ids[i], releases[i] = id, release
		})
	}
	wg.Wait()
	if len(fake.Created()) != 1 || len(slices.Compact(slices.Clone(ids))) != 1 {
		t.Fatalf("concurrent acquires created %d repeaters with IDs %q, want one shared", len(fake.Created()), ids)
	}

	other, releaseOther, err := r.acquire(testContext(t), endpointOn(dokployNetwork, webAddr))
	if err != nil {
		t.Fatalf("acquire other target: %v", err)
	}
	defer releaseOther()
	if other == ids[0] {
		t.Error("another target shares the repeater")
	}
	for _, release := range releases {
		release()
	}
}

func TestRelease_RemovesAfterGrace(t *testing.T) {
	fake := newFake(t)
	fake.AddImage(testImage)
	r := newRepeater(t, fake, Options{Grace: 50 * time.Millisecond})
	ep := endpointOn(composeDefault, pgAddr)

	id, release1, err := r.acquire(testContext(t), ep)
	if err != nil {
		t.Fatal(err)
	}
	_, release2, err := r.acquire(testContext(t), ep)
	if err != nil {
		t.Fatal(err)
	}
	release1()
	release1() // idempotent
	time.Sleep(100 * time.Millisecond)
	if _, ok := fake.Container(id); !ok {
		t.Fatal("repeater removed while a tunnel still uses it")
	}
	release2()
	eventually(t, "the repeater to be removed", func() bool {
		_, ok := fake.Container(id)
		return !ok
	})
}

func TestAcquire_DuringGraceReusesTheRepeater(t *testing.T) {
	fake := newFake(t)
	fake.AddImage(testImage)
	r := newRepeater(t, fake, Options{Grace: time.Hour})
	ep := endpointOn(composeDefault, pgAddr)

	id1, release, err := r.acquire(testContext(t), ep)
	if err != nil {
		t.Fatal(err)
	}
	release()
	id2, release, err := r.acquire(testContext(t), ep)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if id1 != id2 || len(fake.Created()) != 1 {
		t.Errorf("acquire during the grace period created a new repeater")
	}
}

func TestAcquire_PullsAMissingImage(t *testing.T) {
	fake := newFake(t)
	r := newRepeater(t, fake, Options{})
	_, release, err := r.acquire(testContext(t), endpointOn(composeDefault, pgAddr))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()
	if got := fake.Pulls(); !slices.Equal(got, []string{testImage}) {
		t.Errorf("pulls = %q, want %q", got, testImage)
	}
}

func TestAcquire_FailuresAreNotCached(t *testing.T) {
	fake := newFake(t)
	fake.PullError = "registry unreachable"
	r := newRepeater(t, fake, Options{})
	ep := endpointOn(composeDefault, pgAddr)
	if _, _, err := r.acquire(testContext(t), ep); err == nil {
		t.Fatal("acquire without an image succeeded")
	}
	fake.AddImage(testImage)
	_, release, err := r.acquire(testContext(t), ep)
	if err != nil {
		t.Fatalf("acquire after the image appeared: %v", err)
	}
	release()
}

func TestAcquire_CreateFailureLeavesNothing(t *testing.T) {
	fake := newFake(t)
	fake.AddImage(testImage)
	fake.CreateError = &docker.APIError{StatusCode: 500, Message: "no space left"}
	r := newRepeater(t, fake, Options{})
	if _, _, err := r.acquire(testContext(t), endpointOn(composeDefault, pgAddr)); err == nil {
		t.Fatal("acquire succeeded although the daemon fails creations")
	}
	if ids := fake.ContainerIDs(); len(ids) != 0 {
		t.Errorf("containers left behind: %q", ids)
	}
}

func TestClose_RemovesEveryRepeater(t *testing.T) {
	fake := newFake(t)
	fake.AddImage(testImage)
	r := New(newClient(t, fake), Options{Image: testImage, Grace: time.Hour, Log: slog.New(slog.DiscardHandler)})
	if _, _, err := r.acquire(testContext(t), endpointOn(composeDefault, pgAddr)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.acquire(testContext(t), endpointOn(dokployNetwork, webAddr)); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(testContext(t)); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if ids := fake.ContainerIDs(); len(ids) != 0 {
		t.Errorf("containers left after Close: %q", ids)
	}
	if _, _, err := r.acquire(testContext(t), endpointOn(composeDefault, pgAddr)); err == nil {
		t.Error("acquire after Close succeeded")
	}
}

// orphan is a repeater container a companion holding key created and no
// live companion tracks.
func orphan(key []byte, id string, created time.Time) dockertest.Container {
	name := repeaterNamePrefix + id
	return dockertest.Container{ID: id, Name: name, Image: testImage, Running: true, Created: created,
		Labels: map[string]string{LabelRepeater: "1", LabelOwner: "dead-companion", LabelOwnership: ownershipProof(key, name)}}
}

// syncBuffer is a bytes.Buffer safe for concurrent use, for logs.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestReap(t *testing.T) {
	fake := newFake(t)
	fake.AddImage(testImage)
	old := time.Now().Add(-time.Hour)
	fake.AddContainer(orphan(testKey, "orphan-running", old))
	exited := orphan(testKey, "orphan-exited", old)
	exited.Running = false
	fake.AddContainer(exited)
	fake.AddContainer(orphan(testKey, "recent-orphan", time.Now()))
	fake.AddContainer(dockertest.Container{ID: "unrelated", Name: "u", Running: true, Created: old, Labels: map[string]string{"app": "x"}})
	// Containers anyone could label as repeaters are kept unless they
	// also have a repeater's name and image.
	wrongName := orphan(testKey, "wrong-name", old)
	wrongName.Name = "tenant-db"
	fake.AddContainer(wrongName)
	wrongImage := orphan(testKey, "wrong-image", old)
	wrongImage.Image = "postgres:16"
	fake.AddContainer(wrongImage)

	r := newRepeater(t, fake, Options{TTL: time.Minute, Grace: time.Hour})
	tracked, release, err := r.acquire(testContext(t), endpointOn(composeDefault, pgAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	// Two hours later, even the recent orphan is past the TTL; the tracked
	// repeater is kept however old it is.
	r.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	fake.AddContainer(orphan(testKey, "young-later", time.Now().Add(2*time.Hour)))

	removed, err := r.Reap(testContext(t))
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	slices.Sort(removed)
	if want := []string{"orphan-exited", "orphan-running", "recent-orphan"}; !slices.Equal(removed, want) {
		t.Errorf("reaped %q, want %q", removed, want)
	}
	for _, id := range []string{tracked, "unrelated", "young-later", "wrong-name", "wrong-image"} {
		if _, ok := fake.Container(id); !ok {
			t.Errorf("container %s was reaped", id)
		}
	}
	if n := fake.VolumeRemovals(); n != 0 {
		t.Errorf("%d removals also removed volumes", n)
	}
}

func TestReap_SkipsLookAlikesItDidNotCreate(t *testing.T) {
	fake := newFake(t)
	fake.AddImage(testImage)
	old := time.Now().Add(-time.Hour)
	genuine := orphan(testKey, "genuine", old)
	fake.AddContainer(genuine)

	// Look-alikes a tenant could create: a repeater's label, name and
	// image, but no proof of ownership, a proof made with another key, or
	// the genuine orphan's proof copied onto another name.
	unsigned := orphan(testKey, "unsigned", old)
	delete(unsigned.Labels, LabelOwnership)
	forged := orphan([]byte("a key the tenant guessed, 32 b.."), "forged", old)
	copied := orphan(testKey, "copied", old)
	copied.Labels[LabelOwnership] = genuine.Labels[LabelOwnership]
	malformed := orphan(testKey, "malformed", old)
	malformed.Labels[LabelOwnership] = "not hex"
	lookAlikes := []dockertest.Container{unsigned, forged, copied, malformed}
	for _, c := range lookAlikes {
		fake.AddContainer(c)
	}

	logs := &syncBuffer{}
	r := newRepeater(t, fake, Options{TTL: time.Minute, Log: slog.New(slog.NewTextHandler(logs, nil))})
	for range 2 {
		removed, err := r.Reap(testContext(t))
		if err != nil {
			t.Fatalf("Reap: %v", err)
		}
		if len(removed) > 1 || (len(removed) == 1 && removed[0] != "genuine") {
			t.Fatalf("reaped %q, want only the genuine orphan", removed)
		}
	}
	if _, ok := fake.Container("genuine"); ok {
		t.Error("the genuine orphan was not reaped")
	}
	for _, c := range lookAlikes {
		if _, ok := fake.Container(c.ID); !ok {
			t.Errorf("look-alike %s was reaped", c.ID)
		}
		// Each is logged once, not on every pass.
		if n := strings.Count(logs.String(), "container="+c.ID+" "); n != 1 {
			t.Errorf("look-alike %s logged %d times, want once:\n%s", c.ID, n, logs)
		}
	}
}

func TestReap_AfterARestartReapsOnlyItsOwnOrphans(t *testing.T) {
	fake := newFake(t)
	fake.AddImage(testImage)
	// A companion that dies with a tunnel open: its Repeater is never
	// closed.
	crashed := New(newClient(t, fake), Options{Image: testImage, Key: testKey, Grace: time.Hour, Log: slog.New(slog.DiscardHandler)})
	abandoned, _, err := crashed.acquire(testContext(t), endpointOn(composeDefault, pgAddr))
	if err != nil {
		t.Fatal(err)
	}
	later := func() time.Time { return time.Now().Add(time.Hour) }

	// Another installation, with another key, does not own it.
	stranger := newRepeater(t, fake, Options{TTL: time.Minute, Key: []byte("another installation's key, 32 b")})
	stranger.now = later
	if removed, err := stranger.Reap(testContext(t)); err != nil || len(removed) != 0 {
		t.Fatalf("Reap with another key = %q, %v; want nothing", removed, err)
	}

	// The restarted companion holds the same key and reaps it.
	restarted := newRepeater(t, fake, Options{TTL: time.Minute})
	restarted.now = later
	removed, err := restarted.Reap(testContext(t))
	if err != nil || !slices.Equal(removed, []string{abandoned}) {
		t.Fatalf("Reap after a restart = %q, %v; want the abandoned repeater %s", removed, err, abandoned)
	}
}

func TestNew_WithoutAKeyOwnsOnlyItsOwnRepeaters(t *testing.T) {
	fake := newFake(t)
	fake.AddImage(testImage)
	fake.AddContainer(orphan(testKey, "orphan", time.Now().Add(-time.Hour)))
	r := New(newClient(t, fake), Options{Image: testImage, TTL: time.Minute, Log: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { _ = r.Close(testContext(t)) })
	if removed, err := r.Reap(testContext(t)); err != nil || len(removed) != 0 {
		t.Errorf("Reap with a random key = %q, %v; want nothing", removed, err)
	}
}

func TestRun_AHungReapPassTimesOut(t *testing.T) {
	fake := newFake(t)
	var lists atomic.Int32
	fake.Intercept = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/containers/json") {
			lists.Add(1)
			<-r.Context().Done() // a daemon that never answers
			return true
		}
		return false
	}
	r := newRepeater(t, fake, Options{ReapInterval: 10 * time.Millisecond})
	r.reapTimeout = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(testContext(t))
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	eventually(t, "a second reap pass", func() bool { return lists.Load() >= 2 })
	cancel()
	<-done
}

func TestAcquire_CapsRepeaters(t *testing.T) {
	fake := newFake(t)
	fake.AddImage(testImage)
	r := newRepeater(t, fake, Options{MaxRepeaters: 1, Grace: time.Hour})
	first, release, err := r.acquire(testContext(t), endpointOn(composeDefault, pgAddr))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.acquire(testContext(t), endpointOn(dokployNetwork, webAddr)); !errors.Is(err, ErrTooManyRepeaters) {
		t.Fatalf("acquire over the cap: error = %v, want %v", err, ErrTooManyRepeaters)
	}
	// An idle repeater makes room.
	release()
	_, release, err = r.acquire(testContext(t), endpointOn(dokployNetwork, webAddr))
	if err != nil {
		t.Fatalf("acquire with an idle repeater to evict: %v", err)
	}
	defer release()
	eventually(t, "the idle repeater to be evicted", func() bool {
		_, ok := fake.Container(first)
		return !ok
	})
}

func TestRun_ReapsAtStartup(t *testing.T) {
	fake := newFake(t)
	fake.AddContainer(orphan(testKey, "orphan", time.Now().Add(-time.Hour)))
	r := newRepeater(t, fake, Options{TTL: time.Minute, ReapInterval: time.Hour})
	ctx, cancel := context.WithCancel(testContext(t))
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	eventually(t, "the startup reap", func() bool {
		_, ok := fake.Container("orphan")
		return !ok
	})
	cancel()
	<-done
}
