// Package clierr defines the stable error codes and exit codes of the
// doktunnel CLI.
//
// Error codes are part of the public contract: scripts and AI agents match on
// them, so existing codes and their exit codes must never change meaning.
package clierr

import (
	"errors"
	"fmt"
)

// Code is a stable, machine-readable error identifier.
type Code string

// Stable error codes. Keep this list, exitCodes and the README table in sync.
const (
	// Internal is an unexpected failure; it is also used for untyped errors.
	Internal Code = "internal"
	// InvalidArgument means a command, flag or argument is unknown or malformed.
	InvalidArgument Code = "invalid_argument"
	// MissingInput means a required value is missing and prompting is not allowed.
	MissingInput Code = "missing_input"
	// NotImplemented means the command exists but is not implemented yet.
	NotImplemented Code = "not_implemented"
	// PermissionDenied means the caller lacks permission for the operation.
	PermissionDenied Code = "permission_denied"
	// NetworkNotAttachable means the target Docker network cannot be attached.
	NetworkNotAttachable Code = "network_not_attachable"
	// ElevationRequired means the operation needs administrator privileges.
	ElevationRequired Code = "elevation_required"
	// Unreachable means a remote endpoint could not be reached.
	Unreachable Code = "unreachable"
)

// exitCodes maps each code to its process exit code. 0 means success and is
// never used here; 2 follows the common convention for usage errors.
var exitCodes = map[Code]int{
	Internal:             1,
	InvalidArgument:      2,
	MissingInput:         3,
	NotImplemented:       4,
	PermissionDenied:     5,
	NetworkNotAttachable: 6,
	ElevationRequired:    7,
	Unreachable:          8,
}

// Codes returns every stable error code ordered by exit code.
func Codes() []Code {
	return []Code{
		Internal, InvalidArgument, MissingInput, NotImplemented,
		PermissionDenied, NetworkNotAttachable, ElevationRequired, Unreachable,
	}
}

// ExitCode returns the process exit code for c. Unknown codes map to the exit
// code of Internal.
func (c Code) ExitCode() int {
	if exit, ok := exitCodes[c]; ok {
		return exit
	}
	return exitCodes[Internal]
}

// Error is a CLI error carrying a stable code, a human-readable message and an
// optional hint on how to fix it.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// New returns an Error with the given code and message.
func New(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Newf returns an Error with the given code and a formatted message.
func Newf(code Code, format string, args ...any) *Error {
	return New(code, fmt.Sprintf(format, args...))
}

// WithHint returns a copy of e with the hint set.
func (e *Error) WithHint(hint string) *Error {
	c := *e
	c.Hint = hint
	return &c
}

// Error implements the error interface.
func (e *Error) Error() string {
	return e.Message
}

// From converts err into an *Error. A typed error anywhere in the wrap chain
// is returned as is; any other error becomes Internal. From(nil) returns nil.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return New(Internal, err.Error())
}
