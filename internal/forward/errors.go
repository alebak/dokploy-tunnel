package forward

import (
	"errors"
	"fmt"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
	"github.com/alebak/dokploy-tunnel/internal/tunnel"
)

// codeTooManyTunnels is the companion's code for a request over its tunnel
// limits (HTTP 429).
const codeTooManyTunnels tunnel.Code = "too_many_tunnels"

// CompanionError is a request the companion rejected before the upgrade,
// as its JSON error body describes it.
type CompanionError struct {
	// Status is the HTTP status of the answer.
	Status int
	// Code is the companion's error code, or empty when the answer carried
	// no doktunnel error body, as when a proxy answered instead.
	Code tunnel.Code
	// Message is the companion's human-readable message.
	Message string
	// ExpectedServerID is the Dokploy server the target runs on, for
	// tunnel.CodeWrongServer only.
	ExpectedServerID string
}

// Error implements the error interface.
func (e *CompanionError) Error() string {
	if e.Code == "" {
		return e.Message
	}
	msg := fmt.Sprintf("the companion refused the request: %s (%s)", e.Message, e.Code)
	if e.ExpectedServerID != "" {
		msg += fmt.Sprintf("; the service runs on Dokploy server %s", e.ExpectedServerID)
	}
	return msg
}

// CLIError converts an error of this package into a CLI error with a
// stable code: companion rejections by their code, an unreachable
// companion as clierr.Unreachable, and anything else as clierr.From does.
// CLIError(nil) returns nil.
func CLIError(err error) *clierr.Error {
	if err == nil {
		return nil
	}
	var ce *CompanionError
	switch {
	case errors.As(err, &ce):
		return companionCLIError(ce)
	case errors.Is(err, ErrCompanionUnreachable):
		return clierr.New(clierr.Unreachable, err.Error()).
			WithHint("check that the companion URL of the context points at a running doktunnel companion")
	default:
		return clierr.From(err)
	}
}

func companionCLIError(e *CompanionError) *clierr.Error {
	msg := e.Error()
	switch e.Code {
	case tunnel.CodeUnauthenticated, tunnel.CodePermissionDenied, tunnel.CodeForbiddenOrigin:
		return clierr.New(clierr.PermissionDenied, msg).
			WithHint("check that the context's API key is valid and can read the service in Dokploy")
	case tunnel.CodeNotFound:
		return clierr.New(clierr.NotFound, msg)
	case tunnel.CodeNetworkNotAttachable:
		return clierr.New(clierr.NetworkNotAttachable, msg).
			WithHint("enable \"attachable\" on the service's network in Dokploy")
	case tunnel.CodeWrongServer:
		return clierr.New(clierr.Unreachable, msg).
			WithHint(fmt.Sprintf("forward through the companion installed on Dokploy server %s", e.ExpectedServerID))
	case codeTooManyTunnels:
		return clierr.New(clierr.Unreachable, msg).
			WithHint("close some forwarded connections and retry")
	case tunnel.CodeUnreachable, tunnel.CodeTargetUnreachable, tunnel.CodeTimeout, tunnel.CodeUnavailable:
		return clierr.New(clierr.Unreachable, msg)
	case tunnel.CodeInvalidArgument:
		return clierr.New(clierr.InvalidArgument, msg)
	case "":
		return clierr.New(clierr.Unreachable, msg).
			WithHint("check that the companion URL of the context points at a running doktunnel companion")
	default:
		return clierr.New(clierr.Internal, msg)
	}
}
