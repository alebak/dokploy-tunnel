package repeater

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/alebak/dokploy-tunnel/internal/docker"
)

const (
	// connectTimeoutSeconds bounds socat's connection attempt, matching
	// the companion's bound on opening a tunnel.
	connectTimeoutSeconds = 15
	// readyMarker is the notice socat logs at -d -d once both ends are
	// open.
	readyMarker = "starting data transfer loop"
	// maxStderrBytes bounds the socat log kept while waiting for it.
	maxStderrBytes = 8 << 10
)

// Stream is one TCP connection to a target, through a repeater. Reads
// return the target's bytes, writes send to it.
type Stream struct {
	exec *docker.Exec
	// pending holds target bytes read while waiting for socat to connect.
	pending []byte
	release func()
	close   sync.Once
}

// Open connects to t through its repeater, creating the repeater when no
// stream to t is open, and returns the connected stream. ctx bounds
// opening only. It fails with ErrTargetUnreachable, a *NotAttachableError,
// or ctx's error.
func (r *Repeater) Open(ctx context.Context, t Target) (*Stream, error) {
	if t.Port < 1 || t.Port > 65535 {
		return nil, fmt.Errorf("%w: invalid port %d", ErrTargetUnreachable, t.Port)
	}
	ep, err := Resolve(ctx, r.docker, t)
	if err != nil {
		return nil, err
	}
	cmd := []string{"socat", "-d", "-d", "STDIO",
		"TCP:" + ep.Host + ":" + strconv.Itoa(t.Port) + ",connect-timeout=" + strconv.Itoa(connectTimeoutSeconds)}

	// A repeater that died since it was created is replaced once.
	for attempt := 0; ; attempt++ {
		id, release, err := r.acquire(ctx, ep)
		if err != nil {
			return nil, err
		}
		ex, err := r.docker.Exec(ctx, id, cmd, nil)
		if (docker.IsNotFound(err) || docker.IsConflict(err)) && attempt == 0 {
			r.log.Warn("repeater gone, replacing it", "container", shortID(id), "error", err)
			r.invalidate(ep, id)
			release()
			continue
		}
		if err != nil {
			release()
			return nil, fmt.Errorf("%w: running socat in the repeater: %v", ErrTargetUnreachable, err)
		}
		s := &Stream{exec: ex, release: release}
		if err := s.waitConnected(ctx); err != nil {
			s.Close()
			return nil, err
		}
		return s, nil
	}
}

// waitConnected reads socat's log until it reports the connection open,
// keeping any target bytes that arrive first.
func (s *Stream) waitConnected(ctx context.Context) error {
	// Closing the exec unblocks the read below when ctx ends.
	stop := context.AfterFunc(ctx, func() { _ = s.exec.Close() })
	defer stop()

	var log bytes.Buffer
	buf := make([]byte, 4<<10)
	for {
		n, isStderr, err := s.exec.ReadStream(buf)
		if isStderr {
			if log.Len() < maxStderrBytes {
				log.Write(buf[:n])
			}
			if bytes.Contains(log.Bytes(), []byte(readyMarker)) {
				return nil
			}
		} else {
			s.pending = append(s.pending, buf[:n]...)
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("waiting for the target to accept: %w", ctxErr)
			}
			return fmt.Errorf("%w: %s", ErrTargetUnreachable, socatError(log.String(), err))
		}
	}
}

// socatError extracts socat's error from its log, without timestamps.
func socatError(log string, readErr error) string {
	var last string
	for line := range strings.Lines(log) {
		// Lines look like "2026/10/03 12:00:00 socat[7] E connect(...): ...".
		if _, msg, ok := strings.Cut(line, "] E "); ok {
			last = strings.TrimSpace(msg)
		}
	}
	if last != "" {
		return "socat: " + last
	}
	if trimmed := strings.TrimSpace(log); trimmed != "" {
		return "socat: " + trimmed
	}
	return fmt.Sprintf("socat ended without connecting: %v", readErr)
}

// Read reads bytes from the target. It returns io.EOF when the target
// closed the connection.
func (s *Stream) Read(p []byte) (int, error) {
	if len(s.pending) > 0 {
		n := copy(p, s.pending)
		s.pending = s.pending[n:]
		return n, nil
	}
	return s.exec.Read(p)
}

// Write sends bytes to the target.
func (s *Stream) Write(p []byte) (int, error) {
	return s.exec.Write(p)
}

// CloseWrite tells the target no more bytes follow. socat then waits half
// a second (its -t default) for the target to finish before ending the
// stream.
func (s *Stream) CloseWrite() error {
	return s.exec.CloseWrite()
}

// Close ends the stream and releases the repeater. It is safe to call more
// than once.
func (s *Stream) Close() error {
	var err error
	s.close.Do(func() {
		err = s.exec.Close()
		s.release()
	})
	return err
}

// ExposedPorts returns the ports the image of t's running container
// exposes, as the package-level ExposedPorts.
func (r *Repeater) ExposedPorts(ctx context.Context, t Target) ([]Port, error) {
	return ExposedPorts(ctx, r.docker, t)
}
