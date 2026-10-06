package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
	"github.com/alebak/dokploy-tunnel/internal/dokploy"
	"github.com/alebak/dokploy-tunnel/internal/tunnel"
)

const (
	// companionPathSuffix is where the install guide publishes the
	// companion: under the Dokploy panel's own address.
	companionPathSuffix = "/doktunnel"
	// companionProbeTimeout bounds the health probe, including reading the
	// response, so an unresponsive address does not stall "context add".
	companionProbeTimeout = 5 * time.Second
	// maxHealthBytes bounds the health response that is read.
	maxHealthBytes = 4 << 10
)

// defaultCompanionURL returns the conventional companion URL for the panel at
// base, a URL from dokploy.ParseBaseURL: <panel>/doktunnel.
func defaultCompanionURL(base *url.URL) *url.URL {
	u := *base
	u.Path += companionPathSuffix
	return &u
}

// parseCompanionURL validates a companion URL given by the user. The rules
// are the panel URL's: http or https, a host, no credentials, query or
// fragment.
func parseCompanionURL(raw string) (*url.URL, error) {
	u, err := dokploy.ParseBaseURL(raw)
	if err != nil {
		return nil, clierr.New(clierr.InvalidArgument, "invalid companion URL: "+err.Error()).
			WithHint("pass --companion-url with the address of the doktunnel companion, such as https://dokploy.example.com/doktunnel")
	}
	return u, nil
}

// probeCompanion checks that a doktunnel companion answers at u: GET
// <u>/healthz must return 200 with {"status":"ok"}. It sends no credentials
// and does not follow redirects, so the stored URL is the one that works.
func probeCompanion(ctx context.Context, u *url.URL) error {
	ctx, cancel := context.WithTimeout(ctx, companionProbeTimeout)
	defer cancel()
	health := *u
	health.Path += tunnel.HealthPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, health.String(), nil)
	if err != nil {
		return fmt.Errorf("building health request: %w", err)
	}
	client := &http.Client{
		Transport: http.DefaultTransport.(*http.Transport).Clone(),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("requesting %s: %w", health.Redacted(), err)
	}
	defer resp.Body.Close()

	if loc := resp.Header.Get("Location"); resp.StatusCode >= 300 && resp.StatusCode < 400 && loc != "" {
		return fmt.Errorf("%s redirects to %s (HTTP %d)", health.Redacted(), loc, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered HTTP %d", health.Redacted(), resp.StatusCode)
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxHealthBytes)).Decode(&body); err != nil {
		return fmt.Errorf("%s did not answer like a doktunnel companion: %w", health.Redacted(), err)
	}
	if body.Status != "ok" {
		return fmt.Errorf("%s reports status %q", health.Redacted(), body.Status)
	}
	return nil
}

// companionWarnings probes the companion at u and returns the non-fatal
// warnings to report about it for the context called name. warnHTTP adds the
// plain-HTTP warning; callers turn it off when the panel warning already
// covers the same address.
func (e *Env) companionWarnings(name string, u *url.URL, warnHTTP bool) []string {
	var warnings []string
	if warnHTTP && u.Scheme == "http" {
		warnings = append(warnings, "the companion URL uses plain http://, so the API key sent to open tunnels and "+
			"the tunneled traffic travel unencrypted; use https:// unless the network is trusted")
	}
	probe := e.ProbeCompanion
	if probe == nil {
		probe = probeCompanion
	}
	if err := probe(context.Background(), u); err != nil {
		warnings = append(warnings, fmt.Sprintf("the doktunnel companion did not pass its health check (%v); "+
			"%s is stored anyway as the companion URL. If the companion is installed at another address, "+
			"pass --companion-url to 'doktunnel context add', or run 'doktunnel context set-companion %s <URL>'",
			err, u, name))
	}
	return warnings
}
