package dockerproxy

import (
	"log/slog"
	"net/http"
	"time"
)

// NewDefaultServer returns the server Run serves p with.
func NewDefaultServer(p *Proxy, log *slog.Logger) *http.Server {
	return newServer(p, log, defaultTimeouts)
}

// NewServer returns the server Run serves p with, with other timeouts.
func NewServer(p *Proxy, log *slog.Logger, header, body, idle time.Duration) *http.Server {
	return newServer(p, log, timeouts{header: header, body: body, idle: idle})
}
