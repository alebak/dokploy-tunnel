package dokploy

import (
	"errors"
	"fmt"
)

// Response size limits. Every response is read into memory before it is
// decoded, so each procedure gets a cap sized to what it can return.
const (
	// maxResponseBytes is the default limit, for procedures that return one
	// small object, such as user.session, organization.one or a database's
	// <type>.one.
	maxResponseBytes = 1 << 20
	// maxLargeResponseBytes is the limit for procedures whose responses grow
	// with the organization: project.all lists every project, environment
	// and service, and application.one and compose.one embed the service's
	// whole deployment and preview deployment history and its compose file.
	// 16 MiB leaves room for organizations about sixteen times larger than
	// the default allows, while still bounding the memory a misbehaving or
	// hostile panel can make the client allocate.
	maxLargeResponseBytes = 16 << 20
)

// responseLimit returns the most bytes the client reads from a response to
// procedure.
func responseLimit(procedure string) int64 {
	switch procedure {
	case "project.all", "application.one", "compose.one":
		return maxLargeResponseBytes
	default:
		return maxResponseBytes
	}
}

// ErrResponseTooLarge means a response exceeded its procedure's size limit.
// Errors matching it are *ResponseTooLargeError values, which also match
// ErrUnexpectedResponse.
var ErrResponseTooLarge = errors.New("Dokploy response is too large")

// ResponseTooLargeError reports the procedure whose response exceeded its
// limit, and that limit.
type ResponseTooLargeError struct {
	// Procedure is the tRPC procedure called, such as project.all.
	Procedure string
	// Limit is the most bytes the client reads from its response.
	Limit int64
}

// Error implements error.
func (e *ResponseTooLargeError) Error() string {
	return fmt.Sprintf("%v: %s response exceeds %d bytes", ErrResponseTooLarge, e.Procedure, e.Limit)
}

// Unwrap lets errors.Is match both ErrResponseTooLarge and, for callers that
// only distinguish unexpected responses, ErrUnexpectedResponse.
func (e *ResponseTooLargeError) Unwrap() []error {
	return []error{ErrResponseTooLarge, ErrUnexpectedResponse}
}
