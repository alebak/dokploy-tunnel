package cli

import (
	"fmt"
	"os"

	"golang.org/x/term"
)

// TerminalSecret returns a function that reads one line from the terminal f
// without echoing it, for use as App.ReadSecret.
func TerminalSecret(f *os.File) func() (string, error) {
	return func() (string, error) {
		b, err := term.ReadPassword(int(f.Fd()))
		if err != nil {
			return "", fmt.Errorf("reading hidden input: %w", err)
		}
		return string(b), nil
	}
}
