// Package dokploy is a minimal client for the Dokploy HTTP API.
//
// Dokploy's API is a tRPC router exposed over HTTP by an OpenAPI adapter:
// every procedure is served at <panel>/api/<router>.<procedure>, queries as GET
// with their input as query parameters, and requests authenticate with an API
// key in the x-api-key header. Each API key is bound to one organization.
package dokploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// requestTimeout bounds each request, including reading the response.
// Response size limits are in limits.go.
const requestTimeout = 30 * time.Second

var (
	// ErrInvalidURL means a panel URL is malformed.
	ErrInvalidURL = errors.New("invalid Dokploy URL")
	// ErrUnauthorized means Dokploy rejected the API key.
	ErrUnauthorized = errors.New("Dokploy rejected the API key")
	// ErrUnreachable means the panel could not be reached at all.
	ErrUnreachable = errors.New("Dokploy panel is unreachable")
	// ErrUnexpectedResponse means the panel answered, but not like Dokploy.
	ErrUnexpectedResponse = errors.New("unexpected response from Dokploy")
	// ErrNotFound means Dokploy reported that the requested resource, such
	// as a service, does not exist.
	ErrNotFound = errors.New("not found in Dokploy")
)

// Organization is a Dokploy organization.
type Organization struct {
	ID   string
	Name string
}

// API is the subset of the Dokploy API that doktunnel uses.
type API interface {
	// Organization returns the organization the API key is bound to. It
	// fails with ErrUnauthorized, ErrUnreachable or ErrUnexpectedResponse.
	Organization(ctx context.Context) (Organization, error)
	Catalog
	Detailer
	ComposeLister
}

var _ API = (*Client)(nil)

// ParseBaseURL validates and normalizes a panel URL such as
// https://dokploy.example.com or http://192.168.1.20:3000. Any port and path
// prefix is accepted; credentials, queries and fragments are not.
func ParseBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%w %q: %v", ErrInvalidURL, raw, err)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, fmt.Errorf("%w %q: use an http:// or https:// URL", ErrInvalidURL, raw)
	case u.Host == "" || u.Hostname() == "":
		return nil, fmt.Errorf("%w %q: missing host", ErrInvalidURL, raw)
	case u.User != nil:
		return nil, fmt.Errorf("%w %q: credentials do not belong in the URL", ErrInvalidURL, raw)
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return nil, fmt.Errorf("%w %q: remove the query and fragment", ErrInvalidURL, raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

// Client calls the Dokploy API of one panel with one API key.
type Client struct {
	base   *url.URL
	apiKey string
	http   *http.Client
}

// New returns a client for the panel at base, a URL from ParseBaseURL.
// Requests honor HTTPS_PROXY, HTTP_PROXY and NO_PROXY.
func New(base *url.URL, apiKey string) *Client {
	return &Client{
		base:   base,
		apiKey: apiKey,
		http: &http.Client{
			Transport: http.DefaultTransport.(*http.Transport).Clone(),
			Timeout:   requestTimeout,
			// Redirects are not followed: Go would resend the custom
			// x-api-key header to whatever host the panel points at.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// WithAPIKey returns a client for the same panel that authenticates with
// apiKey instead. It shares c's connections, so a server acting for many
// callers keeps one connection pool to the panel.
func (c *Client) WithAPIKey(apiKey string) *Client {
	return &Client{base: c.base, apiKey: apiKey, http: c.http}
}

// Organization implements API. It reads the organization bound to the key
// from user.session, then its name from organization.one.
func (c *Client) Organization(ctx context.Context) (Organization, error) {
	var session *struct {
		Session struct {
			ActiveOrganizationID string `json:"activeOrganizationId"`
		} `json:"session"`
	}
	if err := c.query(ctx, "user.session", nil, &session); err != nil {
		return Organization{}, err
	}
	// user.session answers null when the key carries no organization.
	if session == nil || session.Session.ActiveOrganizationID == "" {
		return Organization{}, fmt.Errorf("%w: the key is not bound to an organization", ErrUnauthorized)
	}
	id := session.Session.ActiveOrganizationID

	var org *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := c.query(ctx, "organization.one", url.Values{"organizationId": {id}}, &org); err != nil {
		return Organization{}, err
	}
	if org == nil || org.ID != id || org.Name == "" {
		return Organization{}, fmt.Errorf("%w: organization.one did not describe organization %q", ErrUnexpectedResponse, id)
	}
	return Organization{ID: org.ID, Name: org.Name}, nil
}

// query calls the tRPC query procedure with params and decodes its JSON
// result into out.
func (c *Client) query(ctx context.Context, procedure string, params url.Values, out any) error {
	u := c.base.JoinPath("api", procedure)
	u.RawQuery = params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("building %s request: %w", procedure, err)
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "doktunnel")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w (HTTP %d from %s)", ErrUnauthorized, resp.StatusCode, procedure)
	case resp.StatusCode == http.StatusNotFound && isNotFoundError(resp.Body):
		return fmt.Errorf("%w (%s)", ErrNotFound, procedure)
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return fmt.Errorf("%w: %s redirected to %q; use that URL instead",
			ErrUnexpectedResponse, procedure, resp.Header.Get("Location"))
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%w: HTTP %d from %s", ErrUnexpectedResponse, resp.StatusCode, procedure)
	}

	limit := responseLimit(procedure)
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Errorf("%w: reading %s response: %v", ErrUnreachable, procedure, err)
	}
	if int64(len(body)) > limit {
		return &ResponseTooLargeError{Procedure: procedure, Limit: limit}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%w: %s did not return JSON: %v", ErrUnexpectedResponse, procedure, err)
	}
	return nil
}

// isNotFoundError reports whether body is the error Dokploy sends when a
// procedure finds nothing, telling it apart from a 404 page served by
// something other than Dokploy.
func isNotFoundError(body io.Reader) bool {
	var e struct {
		Code string `json:"code"`
	}
	err := json.NewDecoder(io.LimitReader(body, maxResponseBytes)).Decode(&e)
	return err == nil && e.Code == "NOT_FOUND"
}
