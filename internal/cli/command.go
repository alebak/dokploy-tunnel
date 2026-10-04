// Package cli implements the doktunnel command tree, its global flags and the
// mapping from errors to output and exit codes.
//
// The tree is plain data (Command values) so other code, such as a generator
// for agent documentation, can walk commands, summaries and flags.
package cli

import (
	"flag"
	"io"
	"net/url"

	"github.com/alebak/dokploy-tunnel/internal/dokploy"
	"github.com/alebak/dokploy-tunnel/internal/elevate"
	"github.com/alebak/dokploy-tunnel/internal/hosts"
	"github.com/alebak/dokploy-tunnel/internal/keyring"
	"github.com/alebak/dokploy-tunnel/internal/prompt"
)

// Command is a node in the command tree. A command with Subcommands and no Run
// is a group: invoked alone, it prints its help.
type Command struct {
	// Name is the word that selects the command on the command line.
	Name string
	// Summary is a one-line description shown in command listings.
	Summary string
	// Description is optional longer help text shown by --help.
	Description string
	// Args names the positional arguments in usage lines, such as "<name>".
	Args string
	// Flags, when set, defines the command's own flags on fs. It is called
	// on every parse, so it binds fresh variables or resets their defaults.
	// Only leaf commands (with Run and no Subcommands) may declare flags.
	Flags func(fs *flag.FlagSet)
	// Run executes the command with its positional arguments.
	Run func(env *Env, args []string) error
	// Subcommands are the child commands, in display order.
	Subcommands []*Command
	// Hidden keeps the command out of help listings and the agent skill.
	// It is for internal entry points, such as the privileged helper,
	// that users never type themselves.
	Hidden bool
}

// Find returns the direct subcommand called name, or nil.
func (c *Command) Find(name string) *Command {
	for _, sub := range c.Subcommands {
		if sub.Name == name {
			return sub
		}
	}
	return nil
}

// Globals holds the flags accepted by every command.
type Globals struct {
	// JSON selects machine-readable output on stdout.
	JSON bool
	// NoInput forbids interactive prompts. It is implied when stdin is not a
	// terminal.
	NoInput bool
	// Context names the context to use instead of the current one.
	Context string
}

// BindGlobalFlags defines the global flags on fs, storing values in g.
func BindGlobalFlags(fs *flag.FlagSet, g *Globals) {
	fs.BoolVar(&g.JSON, "json", false, "print machine-readable JSON on stdout, including errors")
	fs.BoolVar(&g.NoInput, "no-input", false, "never prompt; fail with missing_input instead (implied when stdin is not a terminal)")
	fs.StringVar(&g.Context, "context", "", "use the context `name` instead of the current one")
}

// merge applies the global flags parsed at a deeper command level: boolean
// flags stay set once set, and the last --context wins.
func (g *Globals) merge(other Globals) {
	g.JSON = g.JSON || other.JSON
	g.NoInput = g.NoInput || other.NoInput
	if other.Context != "" {
		g.Context = other.Context
	}
}

// Env is what a running command receives.
type Env struct {
	// Globals are the effective global flags; NoInput already accounts for a
	// non-terminal stdin.
	Globals
	// Stdin is the standard input, shared with Input.
	Stdin io.Reader
	// Stdout receives command results.
	Stdout io.Writer
	// Stderr receives diagnostics and prompts.
	Stderr io.Writer
	// Input obtains values the user did not pass as flags.
	Input prompt.Input
	// ConfigPath is the config file; empty means config.DefaultPath().
	ConfigPath string
	// Keyring stores API keys; it may be nil when none is available.
	Keyring keyring.Keyring
	// NewAPI returns a Dokploy API client for a panel and API key.
	NewAPI func(base *url.URL, apiKey string) dokploy.API
	// ReadSecret reads a line without echo; nil means it is not possible.
	ReadSecret func() (string, error)
	// Getenv reads environment variables.
	Getenv func(key string) string
	// HostsPath is the hosts file; empty means the system hosts file.
	HostsPath string
	// RegistryPath is the address registry; empty means the default.
	RegistryPath string
	// Elevator runs the privileged helper.
	Elevator elevate.Elevator
	// Loopback manages loopback aliases.
	Loopback hosts.Loopback
	// Executable returns the path of the running binary.
	Executable func() (string, error)
	// WriteHosts replaces the hosts file.
	WriteHosts func(path string, data []byte) error
}
