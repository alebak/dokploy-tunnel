// Package tunnel holds the wire protocol shared by doktunnel and
// doktunnel-companion: the endpoint, how a target is addressed, and the
// error codes. docs/protocol.md is the normative description.
package tunnel

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/alebak/dokploy-tunnel/internal/dokploy"
)

// Endpoints and headers of the protocol.
const (
	// Path is the WebSocket endpoint that opens one TCP stream.
	Path = "/v1/tunnel"
	// HealthPath is the companion's liveness endpoint.
	HealthPath = "/healthz"
	// HeaderAPIKey carries the caller's Dokploy API key.
	HeaderAPIKey = "x-api-key"
	// MaxAPIKeyBytes bounds the length of an API key.
	MaxAPIKeyBytes = 1024
	// MaxMessageBytes bounds the size of one WebSocket message.
	MaxMessageBytes = 64 << 10
)

// Query parameters of a tunnel request.
const (
	ParamServiceType = "serviceType"
	ParamServiceID   = "serviceId"
	ParamPort        = "port"
	// ParamServerID optionally names the Dokploy server the client expects
	// the target on.
	ParamServerID = "serverId"
)

// TypeComposeService is the serviceType of a service inside a compose stack.
const TypeComposeService = "compose_service"

// LocalServer names the Dokploy server itself, whose services have no
// serverId in the Dokploy API.
const LocalServer = "local"

// ErrInvalidTarget means a tunnel request does not name a valid target.
var ErrInvalidTarget = errors.New("invalid tunnel target")

var (
	// dokployID matches the IDs Dokploy generates, with room to spare.
	dokployID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	// composeServiceName matches the service names the Compose
	// specification allows.
	composeServiceName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
)

// Target is what a tunnel connects to: a port of a Dokploy service, or of a
// service inside a compose stack.
type Target struct {
	// ServiceType is the type of the Dokploy service. It is
	// dokploy.ServiceCompose for a service inside a compose stack.
	ServiceType dokploy.ServiceType
	// ServiceID is the Dokploy ID of the service, or of the compose stack.
	ServiceID string
	// ComposeService is the name of the service in the compose file, for a
	// service inside a compose stack, and empty otherwise.
	ComposeService string
	// Port is the container port to connect to.
	Port int
}

// wireType returns the serviceType parameter of t.
func (t Target) wireType() string {
	if t.ComposeService != "" {
		return TypeComposeService
	}
	return string(t.ServiceType)
}

// wireID returns the serviceId parameter of t.
func (t Target) wireID() string {
	if t.ComposeService != "" {
		return t.ServiceID + "/" + t.ComposeService
	}
	return t.ServiceID
}

// Query returns the query parameters that address t.
func (t Target) Query() url.Values {
	return url.Values{
		ParamServiceType: {t.wireType()},
		ParamServiceID:   {t.wireID()},
		ParamPort:        {strconv.Itoa(t.Port)},
	}
}

// String describes t for messages and logs, such as "postgres pg_main:5432".
func (t Target) String() string {
	return fmt.Sprintf("%s %s:%d", t.wireType(), t.wireID(), t.Port)
}

// ParseTarget reads the target of a tunnel request from its query
// parameters. It fails with ErrInvalidTarget.
func ParseTarget(q url.Values) (Target, error) {
	typ, err := single(q, ParamServiceType)
	if err != nil {
		return Target{}, err
	}
	id, err := single(q, ParamServiceID)
	if err != nil {
		return Target{}, err
	}
	rawPort, err := single(q, ParamPort)
	if err != nil {
		return Target{}, err
	}

	var t Target
	if typ == TypeComposeService {
		stack, service, ok := strings.Cut(id, "/")
		if !ok || !composeServiceName.MatchString(service) {
			return Target{}, fmt.Errorf("%w: %s must be <composeId>/<service>", ErrInvalidTarget, ParamServiceID)
		}
		t = Target{ServiceType: dokploy.ServiceCompose, ServiceID: stack, ComposeService: service}
	} else {
		if !knownType(typ) {
			return Target{}, fmt.Errorf("%w: unknown %s %q", ErrInvalidTarget, ParamServiceType, typ)
		}
		t = Target{ServiceType: dokploy.ServiceType(typ), ServiceID: id}
	}
	if !dokployID.MatchString(t.ServiceID) {
		return Target{}, fmt.Errorf("%w: malformed %s", ErrInvalidTarget, ParamServiceID)
	}

	// Atoi alone would accept a sign, as in "+5432".
	port, err := strconv.Atoi(rawPort)
	if err != nil || rawPort[0] < '0' || rawPort[0] > '9' || port < 1 || port > 65535 {
		return Target{}, fmt.Errorf("%w: %s must be a number from 1 to 65535", ErrInvalidTarget, ParamPort)
	}
	t.Port = port
	return t, nil
}

// ValidID reports whether id has the form of a Dokploy ID, such as a
// service or server ID.
func ValidID(id string) bool {
	return dokployID.MatchString(id)
}

// single returns the one value of the query parameter name.
func single(q url.Values, name string) (string, error) {
	switch v := q[name]; {
	case len(v) == 0 || v[0] == "":
		return "", fmt.Errorf("%w: missing %s", ErrInvalidTarget, name)
	case len(v) > 1:
		return "", fmt.Errorf("%w: repeated %s", ErrInvalidTarget, name)
	default:
		return v[0], nil
	}
}

// knownType reports whether typ is a Dokploy service type.
func knownType(typ string) bool {
	for _, known := range dokploy.ServiceTypes() {
		if string(known) == typ {
			return true
		}
	}
	return false
}

// ParseServerID reads the optional serverId parameter of a tunnel request:
// a Dokploy server ID, LocalServer, or empty when absent. It fails with
// ErrInvalidTarget.
func ParseServerID(q url.Values) (string, error) {
	if len(q[ParamServerID]) == 0 {
		return "", nil
	}
	id, err := single(q, ParamServerID)
	if err != nil {
		return "", err
	}
	if !ValidID(id) {
		return "", fmt.Errorf("%w: malformed %s", ErrInvalidTarget, ParamServerID)
	}
	return id, nil
}
