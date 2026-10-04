package repeater

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/docker"
)

// DefaultImage is the repeater image: Alpine's socat build, pinned by
// digest so a retagged or compromised tag cannot change what runs next to
// tenant services. It needs only socat and a sleep that accepts
// "infinity", which its BusyBox provides.
const DefaultImage = "alpine/socat:1.8.1.1@sha256:7f9a06753033f2b7de18edc2353f2c15153413d95a039163c6db270fc7a6c3b0"

// Defaults of Options.
const (
	DefaultGrace        = 30 * time.Second
	DefaultTTL          = time.Minute
	DefaultReapInterval = time.Minute
	DefaultMaxRepeaters = 128
)

// Resource limits of every repeater: it only runs sleep and one socat per
// stream, so these bound a misbehaving repeater, not a working one.
const (
	repeaterPidsLimit   = 256
	repeaterMemoryBytes = 64 << 20
)

// repeaterNamePrefix starts the name of every repeater container.
const repeaterNamePrefix = "doktunnel-repeater-"

// Labels on every repeater container.
const (
	// LabelRepeater marks a repeater; the reaper only removes containers
	// with it.
	LabelRepeater = "dev.doktunnel.repeater"
	// LabelOwner is the ID of the companion process that created it.
	LabelOwner = "dev.doktunnel.owner"
	// LabelTarget names the target the repeater reaches, for people only.
	LabelTarget = "dev.doktunnel.target"
	// LabelNetwork is the network the repeater joined.
	LabelNetwork = "dev.doktunnel.network"
	// LabelCreatedAt is the creation time, RFC 3339.
	LabelCreatedAt = "dev.doktunnel.created-at"
)

// errClosed means the Repeater was closed.
var errClosed = errors.New("the repeater is closed")

// ErrTooManyRepeaters means MaxRepeaters repeaters are in use and none is
// idle.
var ErrTooManyRepeaters = errors.New("too many repeaters are running")

const (
	// removeTimeout bounds removing one repeater.
	removeTimeout = 30 * time.Second
	// reapTimeout bounds one pass of the reaper.
	reapTimeout = 2 * time.Minute
)

// Options configure a Repeater. Zero values take the defaults.
type Options struct {
	// Image is the repeater image; DefaultImage by default. It must
	// provide socat and sleep.
	Image string
	// Grace is how long a repeater outlives its last tunnel, so that a
	// client opening connection after connection reuses it.
	Grace time.Duration
	// TTL is how old a repeater this process does not track must be before
	// the reaper removes it. It keeps a repeater being created from being
	// reaped.
	TTL time.Duration
	// ReapInterval spaces the reaper's runs.
	ReapInterval time.Duration
	// Owner identifies this process in LabelOwner; random by default.
	Owner string
	// MaxRepeaters caps the repeaters running at once;
	// DefaultMaxRepeaters by default. Idle repeaters are removed early to
	// make room.
	MaxRepeaters int
	// ComposeDir is where Dokploy keeps compose deployments;
	// DefaultComposeDir by default.
	ComposeDir string
	// Log receives lifecycle events; nothing is logged when nil.
	Log *slog.Logger
}

// Repeater runs repeater containers and opens streams through them. It is
// safe for concurrent use.
type Repeater struct {
	docker *docker.Client
	opts   Options
	log    *slog.Logger
	// now is the clock of the reaper.
	now func() time.Time
	// reapTimeout bounds one pass of the reaper.
	reapTimeout time.Duration

	mu      sync.Mutex
	closed  bool
	entries map[entryKey]*entry
	// busy tracks creations and background removals, which Close waits
	// for.
	busy sync.WaitGroup
}

// entryKey identifies a repeater: one per target address on one network.
type entryKey struct {
	network string
	addr    string
}

// entry is a repeater shared by the tunnels to one target.
type entry struct {
	// ready is closed once id or err is set.
	ready chan struct{}
	id    string
	err   error
	refs  int
	// timer removes the repeater when the grace period after its last
	// tunnel ends.
	timer *time.Timer
}

// New returns a Repeater that runs repeaters through c.
func New(c *docker.Client, opts Options) *Repeater {
	opts.Image = cmp.Or(opts.Image, DefaultImage)
	opts.Grace = cmp.Or(opts.Grace, DefaultGrace)
	opts.TTL = cmp.Or(opts.TTL, DefaultTTL)
	opts.ReapInterval = cmp.Or(opts.ReapInterval, DefaultReapInterval)
	opts.Owner = cmp.Or(opts.Owner, randomHex(8))
	opts.MaxRepeaters = cmp.Or(opts.MaxRepeaters, DefaultMaxRepeaters)
	opts.ComposeDir = cmp.Or(opts.ComposeDir, DefaultComposeDir)
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Repeater{docker: c, opts: opts, log: log, now: time.Now, reapTimeout: reapTimeout, entries: map[entryKey]*entry{}}
}

// acquire returns the running repeater for ep, creating it when needed,
// and a release func to call once when the tunnel using it ends.
func (r *Repeater) acquire(ctx context.Context, ep Endpoint) (string, func(), error) {
	key := keyOf(ep)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return "", nil, errClosed
	}
	e, exists := r.entries[key]
	if !exists && len(r.entries) >= r.opts.MaxRepeaters && !r.evictIdleLocked() {
		r.mu.Unlock()
		return "", nil, fmt.Errorf("%w (%d)", ErrTooManyRepeaters, r.opts.MaxRepeaters)
	}
	if !exists {
		e = &entry{ready: make(chan struct{})}
		r.entries[key] = e
		// Close waits for the creation; it cannot have started waiting,
		// since r.closed is false.
		r.busy.Add(1)
	}
	e.refs++
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	r.mu.Unlock()

	var once sync.Once
	release := func() { once.Do(func() { r.release(key, e) }) }

	if !exists {
		id, err := r.create(ctx, ep)
		r.mu.Lock()
		if err == nil && r.closed {
			r.removeLocked(id, "companion stopping")
			err = errClosed
		}
		e.id, e.err = id, err
		if err != nil && r.entries[key] == e {
			delete(r.entries, key)
		}
		close(e.ready)
		r.mu.Unlock()
		r.busy.Done()
	}
	select {
	case <-e.ready:
	case <-ctx.Done():
		release()
		return "", nil, ctx.Err()
	}
	if e.err != nil {
		release()
		return "", nil, e.err
	}
	return e.id, release, nil
}

// keyOf returns the key of the repeater for ep.
func keyOf(ep Endpoint) entryKey {
	return entryKey{network: ep.Network.ID, addr: ep.Addr.String()}
}

// evictIdleLocked removes one repeater no tunnel uses, waiting out its
// grace period, and reports whether there was one; r.mu must be held.
func (r *Repeater) evictIdleLocked() bool {
	for key, e := range r.entries {
		if e.refs == 0 && e.timer != nil {
			e.timer.Stop()
			delete(r.entries, key)
			r.removeLocked(e.id, "evicted to make room")
			return true
		}
	}
	return false
}

// release drops one reference to e, and schedules its removal after the
// grace period when it was the last one.
func (r *Repeater) release(key entryKey, e *entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.refs--
	if e.refs > 0 || e.err != nil || r.entries[key] != e {
		return
	}
	e.timer = time.AfterFunc(r.opts.Grace, func() {
		r.mu.Lock()
		if e.refs > 0 || r.entries[key] != e {
			r.mu.Unlock()
			return
		}
		delete(r.entries, key)
		r.removeLocked(e.id, "idle")
		r.mu.Unlock()
	})
}

// invalidate forgets e, for a repeater that died, and removes it. Tunnels
// still holding it release it as usual.
func (r *Repeater) invalidate(ep Endpoint, id string) {
	key := keyOf(ep)
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.entries[key]; e != nil && e.id == id {
		if e.timer != nil {
			e.timer.Stop()
		}
		delete(r.entries, key)
		r.removeLocked(id, "unusable")
	}
}

// removeLocked removes a repeater in the background; r.mu must be held.
func (r *Repeater) removeLocked(id, why string) {
	r.busy.Add(1)
	go func() {
		defer r.busy.Done()
		ctx, cancel := context.WithTimeout(context.Background(), removeTimeout)
		defer cancel()
		if err := r.docker.RemoveContainer(ctx, id); err != nil && !docker.IsNotFound(err) {
			r.log.Warn("removing repeater failed", "container", shortID(id), "error", err)
			return
		}
		r.log.Info("repeater removed", "container", shortID(id), "reason", why)
	}()
}

// create creates and starts a repeater for ep, pulling the image if it is
// missing.
func (r *Repeater) create(ctx context.Context, ep Endpoint) (string, error) {
	cfg := docker.ContainerConfig{
		Image: r.opts.Image,
		// It only waits for execs; it listens on no port.
		Entrypoint: []string{"sleep"},
		Cmd:        []string{"infinity"},
		User:       "65534:65534",
		Labels: map[string]string{
			LabelRepeater:  "1",
			LabelOwner:     r.opts.Owner,
			LabelTarget:    ep.Name,
			LabelNetwork:   ep.Network.Name,
			LabelCreatedAt: time.Now().UTC().Format(time.RFC3339),
		},
		HostConfig: docker.HostConfig{
			// The target's network is the only one it joins.
			NetworkMode:    ep.Network.ID,
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			// An init reaps the socat processes of ended streams.
			Init:      ptr(true),
			PidsLimit: ptr(int64(repeaterPidsLimit)),
			Memory:    repeaterMemoryBytes,
		},
	}
	name := repeaterNamePrefix + randomHex(6)
	id, err := r.docker.CreateContainer(ctx, name, cfg)
	if docker.IsNotFound(err) && strings.Contains(strings.ToLower(err.Error()), "no such image") {
		if perr := r.docker.PullImage(ctx, r.opts.Image); perr != nil {
			return "", fmt.Errorf("pulling repeater image: %w", perr)
		}
		id, err = r.docker.CreateContainer(ctx, name, cfg)
	}
	var apiErr *docker.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == 403 && strings.Contains(apiErr.Message, "attachable") {
		return "", &NotAttachableError{Networks: []string{ep.Network.Name}}
	}
	if err != nil {
		return "", fmt.Errorf("creating repeater: %w", err)
	}
	if err := r.docker.StartContainer(ctx, id); err != nil {
		r.mu.Lock()
		r.removeLocked(id, "failed to start")
		r.mu.Unlock()
		return "", fmt.Errorf("starting repeater: %w", err)
	}
	r.log.Info("repeater started", "container", shortID(id), "target", ep.Name, "network", ep.Network.Name)
	return id, nil
}

// Reap removes every repeater container this Repeater does not track and
// that is older than the TTL, running or not, and returns their IDs. One
// companion per Docker daemon is assumed: another live companion's
// repeaters would be reaped too.
//
// The repeater label alone proves nothing, since any container may carry
// it: a container is only removed when it also has a repeater's name and
// runs the configured repeater image.
func (r *Repeater) Reap(ctx context.Context) ([]string, error) {
	found, err := r.docker.ListContainers(ctx, docker.ListOptions{All: true, Labels: []string{LabelRepeater + "=1"}})
	if err != nil {
		return nil, fmt.Errorf("listing repeaters: %w", err)
	}
	r.mu.Lock()
	tracked := map[string]bool{}
	for _, e := range r.entries {
		if e.id != "" {
			tracked[e.id] = true
		}
	}
	r.mu.Unlock()

	var removed []string
	var errs []error
	cutoff := r.now().Add(-r.opts.TTL)
	for _, c := range found {
		if tracked[c.ID] || time.Unix(c.Created, 0).After(cutoff) || !slices.ContainsFunc(c.Names, isRepeaterName) {
			continue
		}
		ctr, err := r.docker.InspectContainer(ctx, c.ID)
		if docker.IsNotFound(err) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("inspecting repeater %s: %w", shortID(c.ID), err))
			continue
		}
		if ctr.Config.Image != r.opts.Image || ctr.Config.Labels[LabelRepeater] != "1" || !isRepeaterName(ctr.Name) {
			continue
		}
		if err := r.docker.RemoveContainer(ctx, c.ID); err != nil && !docker.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("removing orphaned repeater %s: %w", shortID(c.ID), err))
			continue
		}
		removed = append(removed, c.ID)
	}
	return removed, errors.Join(errs...)
}

// Run reaps orphaned repeaters now and then every ReapInterval until ctx
// is done.
func (r *Repeater) Run(ctx context.Context) {
	ticker := time.NewTicker(r.opts.ReapInterval)
	defer ticker.Stop()
	for {
		// A daemon that stops answering must not stall the reaper.
		reapCtx, cancel := context.WithTimeout(ctx, r.reapTimeout)
		removed, err := r.Reap(reapCtx)
		cancel()
		if err != nil && ctx.Err() == nil {
			r.log.Warn("reaping repeaters failed", "error", err)
		}
		if len(removed) > 0 {
			r.log.Info("orphaned repeaters removed", "count", len(removed))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Close removes every repeater this Repeater runs and waits for the
// removals, or for ctx to be done. Streams still open through them end.
func (r *Repeater) Close(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	for key, e := range r.entries {
		if e.timer != nil {
			e.timer.Stop()
		}
		delete(r.entries, key)
		select {
		case <-e.ready:
			if e.id != "" {
				r.removeLocked(e.id, "companion stopping")
			}
		default:
			// Still being created: its creator removes it.
		}
	}
	r.mu.Unlock()

	done := make(chan struct{})
	go func() {
		r.busy.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("removing repeaters: %w", ctx.Err())
	}
}

// isRepeaterName reports whether a container name, with or without its
// leading slash, is a repeater's.
func isRepeaterName(name string) bool {
	return strings.HasPrefix(strings.TrimPrefix(name, "/"), repeaterNamePrefix)
}

func ptr[T any](v T) *T { return &v }

func shortID(id string) string {
	return id[:min(len(id), 12)]
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
