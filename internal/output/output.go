// Package output renders doktunnel results and errors for people and machines.
package output

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
)

// WriteError renders err for the user.
//
// In JSON mode the error object ({"code", "message", "hint"}) is written to
// stdout and nothing goes to stderr. Stdout is the single machine-readable
// stream: a caller using --json always parses stdout and uses the exit code to
// tell a result from an error, without merging or juggling two streams.
//
// In human mode the error is written to stderr, keeping stdout free for data
// that may be piped to other programs.
func WriteError(stdout, stderr io.Writer, jsonMode bool, err *clierr.Error) {
	if jsonMode {
		// Encoding a struct of strings cannot fail; a write error to stdout
		// has nowhere better to be reported, and the exit code still signals
		// the failure.
		_ = WriteJSON(stdout, err)
		return
	}
	fmt.Fprintf(stderr, "doktunnel: %s [%s]\n", err.Message, err.Code)
	if err.Hint != "" {
		fmt.Fprintf(stderr, "hint: %s\n", err.Hint)
	}
}

// WriteJSON writes v to w as a single line of JSON.
func WriteJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	// Keep hints such as "--context <name>" readable instead of <-escaped.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}
	return nil
}
