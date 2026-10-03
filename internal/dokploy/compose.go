package dokploy

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
)

// ComposeLister lists the services inside a compose stack.
type ComposeLister interface {
	// ComposeServices returns the names of the services declared in the
	// compose file of the compose service composeID, in file order. It
	// fails with ErrUnauthorized when the key cannot read the compose
	// service, ErrNotFound when it does not exist or no compose file is
	// cached on the server yet, and ErrUnreachable or ErrUnexpectedResponse
	// as other calls do.
	ComposeServices(ctx context.Context, composeID string) ([]string, error)
}

var _ ComposeLister = (*Client)(nil)

// composeServiceName matches the service names the Compose specification
// allows.
var composeServiceName = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// ComposeServices implements ComposeLister with compose.loadServices. Dokploy
// checks that the key can read this compose service before answering.
//
// The input is always type=cache, which reads the compose file already on
// the server. type=fetch is never sent: it makes Dokploy git clone the
// compose source on the server first.
func (c *Client) ComposeServices(ctx context.Context, composeID string) ([]string, error) {
	if composeID == "" {
		return nil, fmt.Errorf("empty compose ID")
	}
	const procedure = "compose.loadServices"
	var names []string
	params := url.Values{"composeId": {composeID}, "type": {"cache"}}
	if err := c.query(ctx, procedure, params, &names); err != nil {
		return nil, err
	}
	if names == nil {
		return nil, fmt.Errorf("%w: %s for %q returned no list", ErrUnexpectedResponse, procedure, composeID)
	}
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if !composeServiceName.MatchString(n) || seen[n] {
			return nil, fmt.Errorf("%w: %s for %q returned invalid or duplicate service name %q",
				ErrUnexpectedResponse, procedure, composeID, n)
		}
		seen[n] = true
	}
	return names, nil
}
