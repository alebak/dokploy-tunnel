package dockerproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// startExec starts an exec created through the proxy and splices its
// hijacked stream with the client's connection, half-closes included.
func (p *Proxy) startExec(w http.ResponseWriter, r *http.Request, prefix, id string, q url.Values) error {
	if len(q) > 0 {
		return deny("unexpected query")
	}
	var start execStart
	if err := readBody(r, &start); err != nil {
		return err
	}
	if start.Detach || start.Tty {
		return deny("an exec starts attached and without a TTY")
	}
	// Without an upgrade the connection could not carry stdin.
	if !strings.EqualFold(r.Header.Get("Upgrade"), "tcp") {
		return deny("an exec stream must be upgraded to tcp")
	}
	if !p.takeExec(id) {
		return deny("exec %s was not created through the proxy, or was already started", id)
	}
	body, err := json.Marshal(start)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(r.Context(), handshakeTimeout)
	defer cancel()
	daemon, br, resp, err := p.handshake(ctx, prefix+"/exec/"+url.PathEscape(id)+"/start", body)
	if err != nil {
		return err
	}
	// Daemons answer 101 to an upgrade request; very old ones 200 with the
	// raw stream right after the headers.
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		defer daemon.Close()
		defer resp.Body.Close()
		relay(w, resp)
		return nil
	}

	client, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		daemon.Close()
		return fmt.Errorf("taking over the client's connection: %w", err)
	}
	// The server's deadlines may still be set on the connection; the
	// stream lasts as long as the command does.
	_ = client.SetDeadline(time.Time{})
	contentType := "application/vnd.docker.raw-stream"
	if resp.Header.Get("Content-Type") == "application/vnd.docker.multiplexed-stream" {
		contentType = "application/vnd.docker.multiplexed-stream"
	}
	_, _ = fmt.Fprintf(brw, "HTTP/1.1 101 UPGRADED\r\nContent-Type: %s\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n", contentType)
	if err := brw.Flush(); err != nil {
		client.Close()
		daemon.Close()
		return nil
	}
	p.log.Debug("exec stream started", "exec", id)
	splice(client, brw.Reader, daemon, br)
	return nil
}

// handshake sends an upgrading exec start to the daemon on a connection of
// its own, and returns the connection, a reader holding any bytes buffered
// after the response, and the response. ctx bounds the handshake only.
func (p *Proxy) handshake(ctx context.Context, path string, body []byte) (net.Conn, *bufio.Reader, *http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, "http://docker"+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")

	conn, err := p.dial(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("asking the Docker daemon: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	br := bufio.NewReader(conn)
	err = req.Write(conn)
	var resp *http.Response
	if err == nil {
		resp, err = http.ReadResponse(br, req)
	}
	if !stop() || err != nil {
		conn.Close()
		if err == nil || ctx.Err() != nil {
			err = errors.Join(ctx.Err(), err)
		}
		return nil, nil, nil, fmt.Errorf("starting the exec stream: %w", err)
	}
	return conn, br, resp, nil
}

// splice copies between the client and the daemon until the command's
// output ends. The client's half-close reaches the daemon as the end of
// stdin; the end of the output reaches the client as a half-close, after
// which the client has lingerTimeout to close its side.
func splice(client net.Conn, fromClient io.Reader, daemon net.Conn, fromDaemon io.Reader) {
	stdinDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
		_, _ = io.Copy(daemon, fromClient)
		closeWrite(daemon)
	}()
	_, _ = io.Copy(client, fromDaemon)
	closeWrite(client)
	select {
	case <-stdinDone:
	case <-time.After(lingerTimeout):
	}
	client.Close()
	daemon.Close()
	<-stdinDone
}

// closeWrite half-closes conn, or closes it when it cannot be half-closed.
func closeWrite(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = conn.Close()
}
