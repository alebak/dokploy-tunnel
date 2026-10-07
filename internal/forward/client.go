// Package forward is the client side of the tunnel protocol: it asks a
// companion which ports a service exposes, and forwards local TCP
// connections to a service through the companion, one WebSocket per
// connection. docs/protocol.md is the normative description of the wire
// protocol.
package forward

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/coder/websocket"

	"github.com/alebak/dokploy-tunnel/internal/tunnel"
	"github.com/alebak/dokploy-tunnel/internal/version"
)

// Limits of what the client reads from a companion.
const (
	// maxBodyBytes bounds a JSON body read from the companion.
	maxBodyBytes = 1 << 20
	// requestTimeout bounds a ports request.
	requestTimeout = 30 * time.Second
	// dialTimeout bounds opening one tunnel. The companion may have to
	// start a repeater container first, so it is generous.
	dialTimeout = 60 * time.Second
)

// ErrCompanionUnreachable means the companion could not be reached, or did
// not answer like a doktunnel companion.
var ErrCompanionUnreachable = errors.New("companion unreachable")

// Client talks to one companion with one API key. It is safe for
// concurrent use.
type Client struct {
	base   *url.URL
	apiKey string
	http   *http.Client
}

// NewClient returns a Client for the companion at companionURL, an http,
// https, ws or wss URL that may carry a path prefix such as
// https://dokploy.example.com/doktunnel, authenticating with apiKey.
// Plain http is accepted; warning about it is up to the caller.
func NewClient(companionURL, apiKey string) (*Client, error) {
	u, err := url.Parse(companionURL)
	if err != nil {
		return nil, fmt.Errorf("invalid companion URL: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	default:
		return nil, fmt.Errorf("invalid companion URL %q: the scheme must be http or https", u.Redacted())
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid companion URL %q: want scheme://host[:port][/path]", u.Redacted())
	}
	if apiKey == "" || len(apiKey) > tunnel.MaxAPIKeyBytes {
		return nil, fmt.Errorf("invalid API key: it must be 1 to %d bytes", tunnel.MaxAPIKeyBytes)
	}
	return &Client{
		base:   u,
		apiKey: apiKey,
		http: &http.Client{
			Transport: http.DefaultTransport.(*http.Transport).Clone(),
			// A redirect would carry the API key to wherever it points:
			// Go keeps custom headers across redirects.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// endpoint returns the URL of the companion endpoint path with query q.
func (c *Client) endpoint(path string, q url.Values) string {
	u := c.base.JoinPath(path)
	u.RawQuery = q.Encode()
	return u.String()
}

// header returns the headers of every request to the companion. No Origin
// is sent: the companion only checks it for browsers.
func (c *Client) header() http.Header {
	h := http.Header{}
	h.Set(tunnel.HeaderAPIKey, c.apiKey)
	h.Set("User-Agent", "doktunnel/"+version.Version)
	return h
}

// Ports asks the companion which TCP ports the service ref names exposes,
// sorted by port. ref.ServerID, when not empty, is the Dokploy server the
// service is expected on. A rejection by the companion is a
// *CompanionError; a companion that cannot be reached wraps
// ErrCompanionUnreachable.
func (c *Client) Ports(ctx context.Context, ref tunnel.TargetRef) ([]tunnel.Port, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	endpoint := c.endpoint(tunnel.PortsPath, ref.Query())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("building ports request: %w", err)
	}
	req.Header = c.header()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: requesting the ports of %s: %w", ErrCompanionUnreachable, ref.ServiceID, err)
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, maxBodyBytes)
	if resp.StatusCode != http.StatusOK {
		return nil, readError(resp.StatusCode, body)
	}

	var pr tunnel.PortsResponse
	if err := json.NewDecoder(body).Decode(&pr); err != nil {
		return nil, fmt.Errorf("%w: the ports answer is not valid JSON: %w", ErrCompanionUnreachable, err)
	}
	for _, p := range pr.Ports {
		if p.Port < 1 || p.Port > 65535 || p.Protocol != tunnel.ProtocolTCP {
			return nil, fmt.Errorf("%w: the companion reported an invalid port %d/%s",
				ErrCompanionUnreachable, p.Port, p.Protocol)
		}
	}
	slices.SortFunc(pr.Ports, func(a, b tunnel.Port) int { return a.Port - b.Port })
	pr.Ports = slices.Compact(pr.Ports)
	if pr.Ports == nil {
		pr.Ports = []tunnel.Port{}
	}
	return pr.Ports, nil
}

// dial opens one tunnel to target. A rejection by the companion is a
// *CompanionError; a companion that cannot be reached wraps
// ErrCompanionUnreachable.
func (c *Client) dial(ctx context.Context, target tunnel.Target, serverID string) (*websocket.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	q := target.Query()
	if serverID != "" {
		q.Set(tunnel.ParamServerID, serverID)
	}
	conn, resp, err := websocket.Dial(ctx, c.endpoint(tunnel.Path, q), &websocket.DialOptions{
		HTTPClient: c.http,
		HTTPHeader: c.header(),
	})
	if err == nil {
		conn.SetReadLimit(tunnel.MaxMessageBytes)
		return conn, nil
	}
	if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols && resp.Body != nil {
		return nil, readError(resp.StatusCode, resp.Body)
	}
	return nil, fmt.Errorf("%w: opening a tunnel to %s: %w", ErrCompanionUnreachable, target, err)
}

// readError reads the JSON error body of a rejected request.
func readError(status int, body io.Reader) error {
	var er tunnel.ErrorResponse
	if err := json.NewDecoder(body).Decode(&er); err != nil || er.Code == "" {
		return &CompanionError{Status: status, Message: fmt.Sprintf(
			"the companion answered HTTP %d without a doktunnel error; check the companion URL", status)}
	}
	return &CompanionError{
		Status:           status,
		Code:             er.Code,
		Message:          er.Message,
		ExpectedServerID: er.ExpectedServerID,
	}
}
