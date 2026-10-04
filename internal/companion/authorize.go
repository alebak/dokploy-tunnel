// Package companion implements doktunnel-companion, the server that a
// Dokploy administrator installs on each Dokploy server. It accepts tunnel
// WebSockets, asks the Dokploy API whether the caller may reach the target,
// and hands authorized streams to a Bridge.
package companion

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/alebak/dokploy-tunnel/internal/dokploy"
	"github.com/alebak/dokploy-tunnel/internal/tunnel"
)

var (
	// ErrPermissionDenied means Dokploy did not let the API key read the
	// target. It covers invalid keys, keys of another organization, missing
	// grants and services that do not exist, which Dokploy answers alike
	// for most keys and the companion never tells apart.
	ErrPermissionDenied = errors.New("the API key cannot read this service")
	// ErrNotFound means the caller can read the compose stack, but its
	// stored compose file declares no such service, or none is stored.
	ErrNotFound = errors.New("no such service in the compose stack")
	// ErrDokployUnreachable means the Dokploy API could not be asked.
	ErrDokployUnreachable = errors.New("the Dokploy API is unreachable")
	// ErrTimeout means Dokploy or the target did not answer in time.
	ErrTimeout = errors.New("timed out")
)

// WrongServerError means the target runs on another Dokploy server than the
// companion's, so the client must use that server's companion.
type WrongServerError struct {
	// Expected is the server the target runs on: a Dokploy server ID, or
	// tunnel.LocalServer.
	Expected string
	// Actual is this companion's server, in the same form.
	Actual string
}

func (e *WrongServerError) Error() string {
	return fmt.Sprintf("the service runs on Dokploy server %s, not on this companion's server %s", e.Expected, e.Actual)
}

// Target is an authorized tunnel target with what Dokploy knows about it.
type Target struct {
	tunnel.Target
	// Service is the Dokploy service, or the compose stack of a service
	// inside one: its appName, server and networks.
	Service dokploy.ServiceDetails
}

// Authorizer decides whether a caller may reach a target by asking the
// Dokploy API with the caller's own key. It reimplements no permission.
type Authorizer struct {
	panel    *dokploy.Client
	serverID string
}

// NewAuthorizer returns an Authorizer that asks the panel panel, and that
// only accepts targets on the Dokploy server serverID, or on the Dokploy
// server itself when serverID is empty. The key of panel is never used.
func NewAuthorizer(panel *dokploy.Client, serverID string) *Authorizer {
	return &Authorizer{panel: panel, serverID: serverID}
}

// Authorize returns target with its Dokploy details when apiKey can read it
// and it runs on this companion's server. wantServer, when not empty, is the
// server the client expects the target on (a server ID or
// tunnel.LocalServer), and must match too.
//
// The only procedures called are <type>.one, which checks the key's access
// to that service, and for a service inside a compose stack
// compose.loadServices with type=cache. It fails with ErrPermissionDenied,
// *WrongServerError, ErrNotFound, ErrDokployUnreachable or ErrTimeout.
func (a *Authorizer) Authorize(ctx context.Context, apiKey string, target tunnel.Target, wantServer string) (Target, error) {
	client := a.panel.WithAPIKey(apiKey)

	details, err := client.Details(ctx, target.ServiceType, target.ServiceID)
	if err != nil {
		return Target{}, dokployError(ctx, err)
	}

	here := serverName(a.serverID)
	there := serverName(details.ServerID)
	if there != here || (wantServer != "" && wantServer != here) {
		return Target{}, &WrongServerError{Expected: there, Actual: here}
	}

	if target.ComposeService != "" {
		names, err := client.ComposeServices(ctx, target.ServiceID)
		if errors.Is(err, dokploy.ErrNotFound) {
			return Target{}, fmt.Errorf("%w: compose stack %s has no compose file on the server yet", ErrNotFound, target.ServiceID)
		}
		if err != nil {
			return Target{}, dokployError(ctx, err)
		}
		if !slices.Contains(names, target.ComposeService) {
			return Target{}, fmt.Errorf("%w: compose stack %s declares no service %q", ErrNotFound, target.ServiceID, target.ComposeService)
		}
	}
	return Target{Target: target, Service: details}, nil
}

// dokployError maps an error of the Dokploy client to the companion's.
func dokployError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, dokploy.ErrUnauthorized), errors.Is(err, dokploy.ErrNotFound):
		return ErrPermissionDenied
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("%w: asking Dokploy", ErrTimeout)
	default:
		return fmt.Errorf("%w: %v", ErrDokployUnreachable, err)
	}
}

// serverName names the Dokploy server with ID id, where an empty ID is the
// Dokploy server itself.
func serverName(id string) string {
	if id == "" {
		return tunnel.LocalServer
	}
	return id
}
