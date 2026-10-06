package forward

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/alebak/dokploy-tunnel/internal/tunnel"
)

// chunkBytes is the largest message the client sends. The protocol allows
// 64 KiB; the client stays at half of it, like the companion.
const chunkBytes = 32 << 10

// Accept retry backoff after a listener error that is not a close.
const (
	minAcceptBackoff = 5 * time.Millisecond
	maxAcceptBackoff = time.Second
)

// ErrInvalidListenAddress means a listen address is not a loopback
// ip:port.
var ErrInvalidListenAddress = errors.New("invalid listen address")

// EventKind tells what an Event reports.
type EventKind int

// The kinds of Event.
const (
	// EventOpened: a tunnel was opened for an accepted local connection.
	EventOpened EventKind = iota + 1
	// EventClosed: a tunnel ended and its local connection was closed.
	EventClosed
	// EventFailed: an accepted local connection could not be forwarded,
	// because the companion refused the tunnel or could not be reached,
	// and was closed. Err is a *CompanionError or wraps
	// ErrCompanionUnreachable; CLIError converts it.
	EventFailed
	// EventAcceptError: accepting a local connection failed; the
	// forwarder keeps listening.
	EventAcceptError
)

// Event reports something that happened to one local connection.
type Event struct {
	Kind EventKind
	// ID numbers the local connections of a Forwarder from 1; it is 0 for
	// EventAcceptError.
	ID uint64
	// Client is the address of the local peer, when known.
	Client net.Addr
	// Sent and Received count the bytes copied to and from the companion,
	// for EventClosed.
	Sent, Received int64
	// Reason says why a tunnel ended, for EventClosed, such as
	// "client closed" or "companion closed".
	Reason string
	// Err is the cause of EventFailed and EventAcceptError, and of an
	// EventClosed that did not end normally; nil otherwise.
	Err error
}

// Config describes one forward: a local address and the target it
// forwards to.
type Config struct {
	// Addr is the local address to listen on: a loopback IP and a port,
	// such as 127.0.0.2:5432. Port 0 picks a free port.
	Addr string
	// Target is the service and container port to forward to.
	Target tunnel.Target
	// ServerID, when not empty, is the Dokploy server the target is
	// expected on, a server ID or tunnel.LocalServer.
	ServerID string
	// OnEvent, when not nil, is called for every Event. It is called from
	// several goroutines at once and must not block for long.
	OnEvent func(Event)
}

// Forwarder listens on a local address and forwards every accepted TCP
// connection through its own WebSocket tunnel.
type Forwarder struct {
	client *Client
	cfg    Config
	ln     net.Listener

	// dialCtx is cancelled by Close to abort tunnels being opened.
	dialCtx    context.Context
	cancelDial context.CancelFunc
	// shutdown is closed by Close to end every open tunnel.
	shutdown chan struct{}

	mu       sync.Mutex
	closed   bool
	closeErr error
	// conns counts the local connections being handled.
	conns  sync.WaitGroup
	nextID atomic.Uint64
}

// Listen validates cfg and starts listening on cfg.Addr. Call Serve to
// accept connections, and Close to stop.
func (c *Client) Listen(cfg Config) (*Forwarder, error) {
	if err := checkListenAddress(cfg.Addr); err != nil {
		return nil, err
	}
	if cfg.Target.Port < 1 || cfg.Target.Port > 65535 {
		return nil, fmt.Errorf("invalid target port %d", cfg.Target.Port)
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", cfg.Addr, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Forwarder{
		client:     c,
		cfg:        cfg,
		ln:         ln,
		dialCtx:    ctx,
		cancelDial: cancel,
		shutdown:   make(chan struct{}),
	}, nil
}

// checkListenAddress accepts only a loopback IP and a numeric port:
// forwards must never be reachable from other machines.
func checkListenAddress(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%w %q: %w", ErrInvalidListenAddress, addr, err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" || !ip.IsLoopback() {
		return fmt.Errorf("%w %q: the host must be a loopback IP address, such as 127.0.0.1", ErrInvalidListenAddress, addr)
	}
	if _, err := netip.ParseAddrPort(net.JoinHostPort(ip.String(), port)); err != nil {
		return fmt.Errorf("%w %q: the port must be a number from 0 to 65535", ErrInvalidListenAddress, addr)
	}
	return nil
}

// Addr returns the address the forwarder listens on.
func (f *Forwarder) Addr() net.Addr {
	return f.ln.Addr()
}

// Serve accepts local connections until Close is called or ctx is done,
// which also closes the forwarder. It returns nil once the forwarder is
// closed and every tunnel has ended.
func (f *Forwarder) Serve(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { _ = f.Close() })
	defer stop()

	backoff := time.Duration(0)
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			if f.isClosed() {
				f.conns.Wait()
				return nil
			}
			backoff = min(max(2*backoff, minAcceptBackoff), maxAcceptBackoff)
			f.emit(Event{Kind: EventAcceptError, Err: fmt.Errorf("accepting a connection on %s: %w", f.Addr(), err)})
			select {
			case <-time.After(backoff):
			case <-f.shutdown:
			}
			continue
		}
		backoff = 0

		f.mu.Lock()
		if f.closed {
			f.mu.Unlock()
			conn.Close()
			continue
		}
		f.conns.Add(1)
		f.mu.Unlock()
		go f.handle(conn)
	}
}

// Close stops accepting connections, closes every open tunnel with close
// code 1001 (going away) and its local connection, and waits for them to
// end. It is safe to call more than once.
func (f *Forwarder) Close() error {
	f.mu.Lock()
	if !f.closed {
		f.closed = true
		close(f.shutdown)
		f.cancelDial()
		f.closeErr = f.ln.Close()
	}
	err := f.closeErr
	f.mu.Unlock()
	f.conns.Wait()
	return err
}

func (f *Forwarder) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *Forwarder) emit(e Event) {
	if f.cfg.OnEvent != nil {
		f.cfg.OnEvent(e)
	}
}

// handle forwards one local connection through its own tunnel.
func (f *Forwarder) handle(local net.Conn) {
	defer f.conns.Done()
	defer local.Close()
	id := f.nextID.Add(1)

	ws, err := f.client.dial(f.dialCtx, f.cfg.Target, f.cfg.ServerID)
	if err != nil {
		if !f.isClosed() {
			f.emit(Event{Kind: EventFailed, ID: id, Client: local.RemoteAddr(), Err: err})
		}
		return
	}
	f.emit(Event{Kind: EventOpened, ID: id, Client: local.RemoteAddr()})
	end, sent, received := f.pump(local, ws)
	f.emit(Event{
		Kind: EventClosed, ID: id, Client: local.RemoteAddr(),
		Sent: sent, Received: received, Reason: end.reason, Err: end.err,
	})
}

// ending is how a tunnel ended: the close code sent to the companion and
// why.
type ending struct {
	code   websocket.StatusCode
	reason string
	err    error
}

// pump copies between the local connection and the WebSocket until either
// ends or the forwarder closes, then closes both and waits for the copies
// to stop. It mirrors the companion's pump: there is no half-close.
func (f *Forwarder) pump(local net.Conn, ws *websocket.Conn) (end ending, sent, received int64) {
	// The connection's own context is never cancelled: coder/websocket
	// drops the connection without a close frame when it is.
	ctx := context.Background()
	ended := make(chan ending, 2)
	var copies sync.WaitGroup
	copies.Add(2)
	var nSent, nReceived atomic.Int64

	// Local to companion.
	go func() {
		defer copies.Done()
		buf := make([]byte, chunkBytes)
		for {
			n, err := local.Read(buf)
			if n > 0 {
				if werr := ws.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					ended <- ending{websocket.StatusInternalError, "writing to the companion failed", werr}
					return
				}
				nSent.Add(int64(n))
			}
			if errors.Is(err, io.EOF) {
				ended <- ending{websocket.StatusNormalClosure, "client closed", nil}
				return
			}
			if err != nil {
				ended <- ending{websocket.StatusInternalError, "reading from the client failed", err}
				return
			}
		}
	}()

	// Companion to local.
	go func() {
		defer copies.Done()
		for {
			typ, r, err := ws.Reader(ctx)
			if err != nil {
				ended <- companionEnding(err)
				return
			}
			if typ != websocket.MessageBinary {
				ended <- ending{websocket.StatusUnsupportedData, "text messages are not allowed",
					errors.New("the companion sent a text message")}
				return
			}
			n, err := io.Copy(local, r)
			nReceived.Add(n)
			if err != nil {
				ended <- ending{websocket.StatusInternalError, "copying to the client failed", err}
				return
			}
		}
	}()

	select {
	case end = <-ended:
	case <-f.shutdown:
		end = ending{websocket.StatusGoingAway, "client shutting down", nil}
	}
	// Closing the local connection unblocks the copy reading it; closing
	// the WebSocket, the other one. Close is a no-op when the companion
	// already closed it.
	local.Close()
	_ = ws.Close(end.code, end.reason)
	copies.Wait()
	return end, nSent.Load(), nReceived.Load()
}

// companionEnding describes a tunnel the companion ended, from the error of
// a failed WebSocket read.
func companionEnding(err error) ending {
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure:
		return ending{websocket.StatusNormalClosure, "companion closed", nil}
	case websocket.StatusGoingAway:
		return ending{websocket.StatusNormalClosure, "companion shutting down", nil}
	default:
		return ending{websocket.StatusInternalError, "companion closed the tunnel",
			fmt.Errorf("the tunnel to the companion failed: %w", err)}
	}
}
