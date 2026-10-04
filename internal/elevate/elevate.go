// Package elevate runs doktunnel's privileged helper with administrator
// privileges, so the CLI itself never has to run as root or Administrator.
//
// Running the whole CLI elevated would hide the user's keyring, config and
// address registry behind root's own. Instead, the unprivileged CLI computes
// what must change and only the step that writes the hosts file (and, on
// macOS, adds loopback aliases) runs elevated: the same binary, re-executed
// with a hidden subcommand through sudo on Linux and macOS, or through a UAC
// prompt on Windows.
package elevate

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Elevator runs a command with administrator privileges.
type Elevator interface {
	// Run runs argv, whose first element is an absolute executable path,
	// with administrator privileges and waits for it to finish. It may
	// prompt the user for a password or a confirmation.
	Run(ctx context.Context, argv []string) error
	// Command renders argv as a command line the user can run in their own
	// shell to perform the same step with administrator privileges.
	Command(argv []string) string
}

// Sudo elevates through sudo, which prompts for a password on the terminal
// when it needs one.
type Sudo struct {
	// Stdin, Stdout and Stderr are connected to sudo; nil means the
	// process's own streams, which sudo needs to prompt.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Exec runs the prepared command; nil means (*exec.Cmd).Run. Tests
	// replace it so sudo never runs.
	Exec func(*exec.Cmd) error
}

// Run implements Elevator. "--" keeps sudo from reading argv as options.
func (s Sudo) Run(ctx context.Context, argv []string) error {
	cmd := exec.CommandContext(ctx, "sudo", append([]string{"--"}, argv...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = orStdin(s.Stdin), orWriter(s.Stdout, os.Stdout), orWriter(s.Stderr, os.Stderr)
	if err := run(s.Exec, cmd); err != nil {
		return fmt.Errorf("running the privileged helper with sudo: %w", err)
	}
	return nil
}

// Command implements Elevator with POSIX shell quoting.
func (Sudo) Command(argv []string) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		words[i] = shellQuote(a)
	}
	return "sudo " + strings.Join(words, " ")
}

// UAC elevates through a Windows UAC prompt: PowerShell's Start-Process
// -Verb RunAs starts the helper elevated and waits for its exit code. The
// helper runs in its own hidden window, so its output is not shown; the
// exit code tells success from failure.
type UAC struct {
	// Exec runs the prepared command; nil means (*exec.Cmd).Run. Tests
	// replace it so PowerShell never runs.
	Exec func(*exec.Cmd) error
}

// Run implements Elevator.
func (u UAC) Run(ctx context.Context, argv []string) error {
	script := "$p = Start-Process -FilePath " + psQuote(argv[0])
	if len(argv) > 1 {
		script += " -ArgumentList " + psQuote(windowsArgs(argv[1:]))
	}
	script += " -Verb RunAs -Wait -PassThru -WindowStyle Hidden; exit $p.ExitCode"
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := run(u.Exec, cmd); err != nil {
		return fmt.Errorf("running the privileged helper through UAC (declined, or the helper failed): %w", err)
	}
	return nil
}

// Command implements Elevator with a PowerShell command line that opens the
// same UAC prompt.
func (UAC) Command(argv []string) string {
	s := "Start-Process -Verb RunAs -Wait -FilePath " + psQuote(argv[0])
	if len(argv) > 1 {
		s += " -ArgumentList " + psQuote(windowsArgs(argv[1:]))
	}
	return s
}

func run(execFn func(*exec.Cmd) error, cmd *exec.Cmd) error {
	if execFn != nil {
		return execFn(cmd)
	}
	return cmd.Run()
}

func orStdin(r io.Reader) io.Reader {
	if r == nil {
		return os.Stdin
	}
	return r
}

func orWriter(w, def io.Writer) io.Writer {
	if w == nil {
		return def
	}
	return w
}

// shellQuote quotes s for a POSIX shell unless it only has safe characters.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@%_+=:,./-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// psQuote quotes s as a PowerShell single-quoted string.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// windowsArgs joins args into one Windows command line, quoting each as
// CommandLineToArgvW expects. Start-Process is given a single string because
// it joins an array without quoting its elements.
func windowsArgs(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = windowsArg(a)
	}
	return strings.Join(quoted, " ")
}

// windowsArg quotes one argument following the CommandLineToArgvW rules:
// backslashes are literal unless they precede a double quote.
func windowsArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\v\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, 2*slashes+1))
			b.WriteByte('"')
			slashes = 0
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
			b.WriteByte(c)
			slashes = 0
		}
	}
	b.WriteString(strings.Repeat(`\`, 2*slashes))
	b.WriteByte('"')
	return b.String()
}
