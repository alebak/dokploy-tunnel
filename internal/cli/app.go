package cli

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
	"github.com/alebak/dokploy-tunnel/internal/dokploy"
	"github.com/alebak/dokploy-tunnel/internal/keyring"
	"github.com/alebak/dokploy-tunnel/internal/output"
	"github.com/alebak/dokploy-tunnel/internal/prompt"
	"github.com/alebak/dokploy-tunnel/internal/version"
)

// App runs a command tree against a set of standard streams.
type App struct {
	// Root is the top of the command tree; its Name is the binary name.
	Root *Command
	// Stdin is read when a command prompts for a missing value.
	Stdin io.Reader
	// Stdout receives results, help, and errors in JSON mode.
	Stdout io.Writer
	// Stderr receives human-readable errors and prompts.
	Stderr io.Writer
	// StdinIsTerminal reports whether prompting is possible at all; when it
	// is false, --no-input is implied.
	StdinIsTerminal bool
	// ConfigPath is the config file; empty means config.DefaultPath().
	ConfigPath string
	// Keyring stores API keys. Commands that need it fail when it is nil.
	Keyring keyring.Keyring
	// NewAPI returns a Dokploy API client; nil means dokploy.New.
	NewAPI func(base *url.URL, apiKey string) dokploy.API
	// ReadSecret reads one line from the terminal without echoing it; nil
	// means secrets cannot be prompted for.
	ReadSecret func() (string, error)
	// Getenv reads environment variables; nil means os.Getenv.
	Getenv func(key string) string
}

// Run executes the command selected by args (without the program name) and
// returns the process exit code.
func (a *App) Run(args []string) int {
	var g Globals
	err := a.dispatch(a.Root, []string{a.Root.Name}, args, &g)
	if err == nil {
		return 0
	}
	e := clierr.From(err)
	// --json may not have been parsed yet when parsing itself failed, so the
	// raw arguments are checked too.
	output.WriteError(a.Stdout, a.Stderr, g.JSON || requestsJSON(args), e)
	return e.Code.ExitCode()
}

func (a *App) dispatch(cmd *Command, path, args []string, g *Globals) error {
	fs := flag.NewFlagSet(strings.Join(path, " "), flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var local Globals
	BindGlobalFlags(fs, &local)
	var root rootFlags
	if cmd == a.Root {
		bindRootFlags(fs, &root)
	}
	if cmd.Flags != nil {
		cmd.Flags(fs)
	}

	var rest []string
	var err error
	if len(cmd.Subcommands) > 0 {
		// Groups stop at the first positional argument: it names a subcommand.
		err = fs.Parse(args)
		rest = fs.Args()
	} else {
		rest, err = parseInterspersed(fs, args)
	}
	g.merge(local)
	if errors.Is(err, flag.ErrHelp) {
		return a.writeHelp(cmd, path, fs)
	}
	if err != nil {
		return clierr.New(clierr.InvalidArgument, err.Error()).
			WithHint(fmt.Sprintf("run '%s --help' for usage", strings.Join(path, " ")))
	}
	if root.skill {
		// The skill is always Markdown, so --json does not apply to it.
		return writeSkill(a.Stdout, a.Root)
	}
	if root.version {
		_, err := fmt.Fprintln(a.Stdout, version.String(a.Root.Name))
		return err
	}

	if len(cmd.Subcommands) > 0 && len(rest) > 0 {
		sub := cmd.Find(rest[0])
		if sub == nil {
			return clierr.Newf(clierr.InvalidArgument, "unknown command %q for %q", rest[0], strings.Join(path, " ")).
				WithHint(fmt.Sprintf("run '%s --help' to list commands", strings.Join(path, " ")))
		}
		return a.dispatch(sub, append(path, sub.Name), rest[1:], g)
	}
	if cmd.Run == nil {
		return a.writeHelp(cmd, path, fs)
	}

	return cmd.Run(a.env(*g), rest)
}

// env builds the environment a command runs in.
func (a *App) env(g Globals) *Env {
	// Prompts and commands that read stdin directly share one buffer, so
	// neither loses input the other has buffered.
	stdin := bufio.NewReader(a.Stdin)
	env := &Env{
		Globals:    g,
		Stdin:      stdin,
		Stdout:     a.Stdout,
		Stderr:     a.Stderr,
		ConfigPath: a.ConfigPath,
		Keyring:    a.Keyring,
		NewAPI:     a.NewAPI,
		ReadSecret: a.ReadSecret,
		Getenv:     a.Getenv,
	}
	env.NoInput = env.NoInput || !a.StdinIsTerminal
	env.Input = prompt.New(!env.NoInput, stdin, a.Stderr)
	if env.NewAPI == nil {
		env.NewAPI = func(base *url.URL, apiKey string) dokploy.API { return dokploy.New(base, apiKey) }
	}
	if env.Getenv == nil {
		env.Getenv = os.Getenv
	}
	return env
}

// parseInterspersed parses flags that appear before, between or after
// positional arguments, so "forward api --json" works like
// "forward --json api". Arguments after "--" are always positional.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		consumed := len(args) - len(rest)
		if consumed > 0 && args[consumed-1] == "--" {
			return append(positional, rest...), nil
		}
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// requestsJSON reports whether args contain an enabled --json flag before any
// "--" terminator.
func requestsJSON(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if !strings.HasPrefix(arg, "-") || name != "json" {
			continue
		}
		if !hasValue {
			return true
		}
		if on, err := strconv.ParseBool(value); err == nil && on {
			return true
		}
	}
	return false
}

func (a *App) writeHelp(cmd *Command, path []string, fs *flag.FlagSet) error {
	var b strings.Builder
	if cmd.Summary != "" {
		fmt.Fprintf(&b, "%s\n\n", cmd.Summary)
	}
	if cmd.Description != "" {
		fmt.Fprintf(&b, "%s\n\n", cmd.Description)
	}
	name := strings.Join(path, " ")
	b.WriteString("Usage:\n")
	if len(cmd.Subcommands) > 0 {
		fmt.Fprintf(&b, "  %s [flags] <command>\n", name)
	} else {
		fmt.Fprintf(&b, "  %s\n", usageLine(name, cmd))
	}

	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	if len(cmd.Subcommands) > 0 {
		b.WriteString("\nCommands:\n")
		for _, sub := range cmd.Subcommands {
			fmt.Fprintf(tw, "  %s\t%s\n", sub.Name, sub.Summary)
		}
		if err := tw.Flush(); err != nil {
			return fmt.Errorf("formatting help: %w", err)
		}
	}

	b.WriteString("\nFlags:\n")
	fs.VisitAll(func(f *flag.Flag) {
		valueName, usage := flag.UnquoteUsage(f)
		if valueName == "value" || valueName == "" {
			fmt.Fprintf(tw, "  --%s\t%s\n", f.Name, usage)
			return
		}
		fmt.Fprintf(tw, "  --%s <%s>\t%s\n", f.Name, valueName, usage)
	})
	fmt.Fprintf(tw, "  -h, --help\tshow help\n")
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("formatting help: %w", err)
	}
	if len(cmd.Subcommands) > 0 {
		fmt.Fprintf(&b, "\nRun '%s <command> --help' for more information about a command.\n", name)
	}

	if _, err := io.WriteString(a.Stdout, b.String()); err != nil {
		return fmt.Errorf("writing help: %w", err)
	}
	return nil
}

// usageLine renders the usage of the leaf command cmd invoked as name. A
// command that declares neither flags nor arguments may still take any
// arguments, so it shows a generic placeholder.
func usageLine(name string, cmd *Command) string {
	switch {
	case cmd.Args != "":
		return name + " [flags] " + cmd.Args
	case cmd.Flags != nil:
		return name + " [flags]"
	default:
		return name + " [flags] [args]"
	}
}
