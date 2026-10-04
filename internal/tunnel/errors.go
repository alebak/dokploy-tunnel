package tunnel

// Code is the stable code of a rejected tunnel request. Clients branch on
// it, never on the message. New codes may be added; existing codes keep
// their meaning.
type Code string

// The codes a companion rejects a tunnel request with, before the upgrade.
const (
	CodeInvalidArgument      Code = "invalid_argument"
	CodeUnauthenticated      Code = "unauthenticated"
	CodePermissionDenied     Code = "permission_denied"
	CodeForbiddenOrigin      Code = "forbidden_origin"
	CodeNotFound             Code = "not_found"
	CodeMethodNotAllowed     Code = "method_not_allowed"
	CodeNetworkNotAttachable Code = "network_not_attachable"
	CodeWrongServer          Code = "wrong_server"
	CodeUpgradeRequired      Code = "upgrade_required"
	CodeUnreachable          Code = "unreachable"
	CodeTargetUnreachable    Code = "target_unreachable"
	CodeUnavailable          Code = "unavailable"
	CodeTimeout              Code = "timeout"
	CodeInternal             Code = "internal"
)

// ErrorResponse is the JSON body of a rejected tunnel request.
type ErrorResponse struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	// ExpectedServerID is the Dokploy server the target runs on, a server
	// ID or LocalServer, for CodeWrongServer only.
	ExpectedServerID string `json:"expected_server_id,omitempty"`
}
