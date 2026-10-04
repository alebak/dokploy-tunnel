package docker

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Exec is a running command's attached stream: reads return its stdout,
// writes go to its stdin. It ends when the command exits.
type Exec struct {
	conn  net.Conn
	out   *demux
	close sync.Once
}

// Exec runs cmd in the running container id with stdin, stdout and stderr
// attached and no TTY, and returns its stream. Stderr is copied to stderr
// as it is read along with stdout; it must not block. ctx bounds starting
// the command only.
func (c *Client) Exec(ctx context.Context, id string, cmd []string, stderr io.Writer) (*Exec, error) {
	create := map[string]any{
		"AttachStdin":  true,
		"AttachStdout": true,
		"AttachStderr": true,
		"Tty":          false,
		"Cmd":          cmd,
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/exec", nil, create, &created); err != nil {
		return nil, err
	}
	if created.ID == "" {
		return nil, errors.New("docker: creating exec: no ID returned")
	}

	conn, br, err := c.hijack(ctx, "/exec/"+url.PathEscape(created.ID)+"/start", map[string]bool{"Detach": false, "Tty": false})
	if err != nil {
		return nil, err
	}
	return &Exec{conn: conn, out: &demux{r: br, stderr: stderr}}, nil
}

// hijack sends a POST that the daemon answers by switching the connection
// to a raw stream, and returns the connection and a reader that holds any
// bytes already buffered after the response.
func (c *Client) hijack(ctx context.Context, path string, body any) (net.Conn, *bufio.Reader, error) {
	req, err := c.newRequest(ctx, http.MethodPost, path, nil, body)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")

	conn, err := c.dial(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("docker: POST %s: %w", path, err)
	}
	// The handshake must respect ctx; the stream that follows has no
	// deadline.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	conn, br, err := handshake(conn, req)
	if !stop() || err != nil {
		if conn != nil {
			conn.Close()
		}
		if err == nil || ctx.Err() != nil {
			err = errors.Join(ctx.Err(), err)
		}
		return nil, nil, fmt.Errorf("docker: POST %s: %w", path, err)
	}
	return conn, br, nil
}

func handshake(conn net.Conn, req *http.Request) (net.Conn, *bufio.Reader, error) {
	if err := req.Write(conn); err != nil {
		return conn, nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return conn, nil, err
	}
	// Daemons answer 101 to an upgrade request; very old ones 200 with the
	// raw stream right after the headers.
	if resp.StatusCode == http.StatusSwitchingProtocols || resp.StatusCode == http.StatusOK {
		return conn, br, nil
	}
	defer resp.Body.Close()
	return conn, nil, checkResponse(resp)
}

// Read reads the command's stdout. It returns io.EOF when the command has
// exited and its output is drained.
func (e *Exec) Read(p []byte) (int, error) {
	return e.out.Read(p)
}

// Write writes to the command's stdin.
func (e *Exec) Write(p []byte) (int, error) {
	return e.conn.Write(p)
}

// CloseWrite closes the command's stdin, leaving its stdout readable.
func (e *Exec) CloseWrite() error {
	if cw, ok := e.conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.New("docker: the connection cannot be half-closed")
}

// Close closes the stream, which unblocks pending reads and writes. The
// daemon then closes the command's stdin.
func (e *Exec) Close() error {
	var err error
	e.close.Do(func() { err = e.conn.Close() })
	return err
}

// errBadFrame means a multiplexed stream carried an unknown stream type.
var errBadFrame = errors.New("docker: malformed multiplexed stream")

// Stream types of the multiplexed stream.
const (
	streamStdin  = 0
	streamStdout = 1
	streamStderr = 2
)

// demux reads the multiplexed stream of a non-TTY exec: frames of an
// 8-byte header (stream type, three zero bytes, big-endian payload length)
// and the payload. Read returns stdout payloads; stderr payloads are copied
// to stderr.
type demux struct {
	r      io.Reader
	stderr io.Writer
	header [8]byte
	// stream and remaining describe the frame being read.
	stream    byte
	remaining int64
}

func (d *demux) Read(p []byte) (int, error) {
	for {
		if d.remaining == 0 {
			if _, err := io.ReadFull(d.r, d.header[:]); err != nil {
				return 0, err
			}
			d.stream = d.header[0]
			d.remaining = int64(binary.BigEndian.Uint32(d.header[4:]))
			if d.stream > streamStderr {
				return 0, fmt.Errorf("%w: stream type %d", errBadFrame, d.stream)
			}
			continue
		}
		if d.stream != streamStdout {
			sink := io.Discard
			if d.stream == streamStderr && d.stderr != nil {
				sink = d.stderr
			}
			n, err := io.CopyN(sink, d.r, d.remaining)
			d.remaining -= n
			if err != nil {
				return 0, unexpectedEOF(err)
			}
			continue
		}
		if len(p) == 0 {
			return 0, nil
		}
		if int64(len(p)) > d.remaining {
			p = p[:d.remaining]
		}
		n, err := d.r.Read(p)
		d.remaining -= int64(n)
		if err == io.EOF && d.remaining > 0 {
			err = io.ErrUnexpectedEOF
		}
		if err == io.EOF {
			// The frame is complete; report EOF on the next call, if the
			// stream really ends there.
			err = nil
		}
		return n, err
	}
}

// unexpectedEOF reports an EOF inside a frame as io.ErrUnexpectedEOF.
func unexpectedEOF(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}
