// Package prompt decides how commands obtain values the user did not pass as
// flags: by asking interactively when input is allowed, or by failing with a
// missing_input error that names the flag to pass when it is not.
package prompt

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
)

// Request describes a value a command needs.
type Request struct {
	// Flag is the flag name, without dashes, that supplies the value
	// non-interactively.
	Flag string
	// Label is the human-readable name shown when asking.
	Label string
}

// Input obtains missing values for commands.
type Input interface {
	// Ask returns the value for req, or a *clierr.Error with code
	// MissingInput when no value can be obtained.
	Ask(req Request) (string, error)
}

// New returns the Input for the given policy. When allowed is false every
// request fails with MissingInput; otherwise questions are written to out and
// answers are read line by line from in.
func New(allowed bool, in io.Reader, out io.Writer) Input {
	if !allowed {
		return noInput{}
	}
	return &lineInput{in: bufio.NewReader(in), out: out}
}

// IsTerminal reports whether f is an interactive terminal (a character
// device), which is how doktunnel decides that --no-input is implied.
func IsTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func missing(req Request) *clierr.Error {
	return clierr.Newf(clierr.MissingInput, "missing value for --%s", req.Flag).
		WithHint(fmt.Sprintf("pass --%s <value>", req.Flag))
}

type noInput struct{}

func (noInput) Ask(req Request) (string, error) {
	return "", missing(req)
}

type lineInput struct {
	in  *bufio.Reader
	out io.Writer
}

func (l *lineInput) Ask(req Request) (string, error) {
	if _, err := fmt.Fprintf(l.out, "%s: ", req.Label); err != nil {
		return "", fmt.Errorf("writing prompt: %w", err)
	}
	line, err := l.in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("reading answer for --%s: %w", req.Flag, err)
	}
	answer := strings.TrimSpace(line)
	if answer == "" {
		return "", missing(req)
	}
	return answer, nil
}
