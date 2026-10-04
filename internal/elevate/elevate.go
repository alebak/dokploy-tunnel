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
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"unicode/utf16"
)

// Elevator runs a command with administrator privileges.
type Elevator interface {
	// Run runs argv, whose first element is an absolute executable path,
	// with administrator privileges and waits for it to finish. It may
	// prompt the user for a password or a confirmation. A non-nil stdin
	// becomes the elevated process's standard input, which needs
	// PipesStdin.
	Run(ctx context.Context, argv []string, stdin io.Reader) error
	// PipesStdin reports whether Run can hand the elevated process a
	// standard input.
	PipesStdin() bool
	// Command renders argv as a command line the user can run in their own
	// shell to perform the same step with administrator privileges. A
	// non-empty stdinFile is redirected to its standard input by the user's
	// own shell, which needs PipesStdin.
	Command(argv []string, stdinFile string) string
}

// Sudo elevates through sudo, which prompts for a password on the terminal
// (/dev/tty), so the helper's standard input stays free for its data.
type Sudo struct {
	// Stdout and Stderr are connected to sudo; nil means the process's own
	// streams.
	Stdout io.Writer
	Stderr io.Writer
	// Exec runs the prepared command; nil means (*exec.Cmd).Run. Tests
	// replace it so sudo never runs.
	Exec func(*exec.Cmd) error
}

// Run implements Elevator. "--" keeps sudo from reading argv as options.
// A nil stdin means the process's own.
func (s Sudo) Run(ctx context.Context, argv []string, stdin io.Reader) error {
	cmd := exec.CommandContext(ctx, "sudo", append([]string{"--"}, argv...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = orStdin(stdin), orWriter(s.Stdout, os.Stdout), orWriter(s.Stderr, os.Stderr)
	if err := run(s.Exec, cmd); err != nil {
		return fmt.Errorf("running the privileged helper with sudo: %w", err)
	}
	return nil
}

// PipesStdin implements Elevator: sudo passes its standard input through.
func (Sudo) PipesStdin() bool { return true }

// Command implements Elevator with POSIX shell quoting.
func (Sudo) Command(argv []string, stdinFile string) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		words[i] = shellQuote(a)
	}
	s := "sudo " + strings.Join(words, " ")
	if stdinFile != "" {
		s += " < " + shellQuote(stdinFile)
	}
	return s
}

// UAC elevates through a Windows UAC prompt: PowerShell's Start-Process
// -Verb RunAs starts the helper elevated and waits for its exit code. The
// helper runs in its own hidden window, so its output is not shown; the
// exit code tells success from failure. Start-Process -Verb RunAs cannot
// redirect the helper's standard input, so UAC never pipes stdin.
type UAC struct {
	// Exec runs the prepared command; nil means (*exec.Cmd).Run. Tests
	// replace it so PowerShell never runs.
	Exec func(*exec.Cmd) error
}

// Run implements Elevator. The script is passed with -EncodedCommand, so
// the Windows command line carrying it is never parsed as PowerShell a
// second time; psQuote still quotes every literal inside it.
func (u UAC) Run(ctx context.Context, argv []string, stdin io.Reader) error {
	if stdin != nil {
		return errors.New("running the privileged helper through UAC: a UAC prompt cannot pass standard input")
	}
	script := "$p = Start-Process -FilePath " + psQuote(argv[0])
	if len(argv) > 1 {
		script += " -ArgumentList " + psQuote(windowsArgs(argv[1:]))
	}
	script += " -Verb RunAs -Wait -PassThru -WindowStyle Hidden; exit $p.ExitCode"
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", encodeCommand(script))
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := run(u.Exec, cmd); err != nil {
		return fmt.Errorf("running the privileged helper through UAC (declined, or the helper failed): %w", err)
	}
	return nil
}

// PipesStdin implements Elevator: UAC cannot pass a standard input.
func (UAC) PipesStdin() bool { return false }

// Command implements Elevator with a PowerShell command line that opens the
// same UAC prompt. stdinFile is not supported and must be empty.
func (UAC) Command(argv []string, _ string) string {
	s := "Start-Process -Verb RunAs -Wait -FilePath " + psQuote(argv[0])
	if len(argv) > 1 {
		s += " -ArgumentList " + psQuote(windowsArgs(argv[1:]))
	}
	return s
}

// encodeCommand encodes script for powershell.exe -EncodedCommand: base64
// of its UTF-16LE form.
func encodeCommand(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	return base64.StdEncoding.EncodeToString(b)
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

// psQuotes are the characters PowerShell accepts as a single quote: the
// ASCII apostrophe and the Unicode quotes U+2018 to U+201B.
const psQuotes = "'\u2018\u2019\u201a\u201b"

// psQuote quotes s as a PowerShell single-quoted string, doubling every
// character that would otherwise end it.
func psQuote(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		if strings.ContainsRune(psQuotes, r) {
			b.WriteRune(r)
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
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
