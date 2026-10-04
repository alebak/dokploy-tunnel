package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
	"github.com/alebak/dokploy-tunnel/internal/hosts"
	"github.com/alebak/dokploy-tunnel/internal/output"
	"github.com/alebak/dokploy-tunnel/internal/registry"
)

// hostsFileEnv points the unprivileged hosts commands at another file. It
// exists for tests and is never honored by the privileged helper, so it
// cannot redirect a write made with administrator privileges.
const hostsFileEnv = "DOKTUNNEL_HOSTS_FILE"

// pendingFileName holds the entries handed to the privileged helper, next
// to the address registry.
const pendingFileName = "pending-hosts"

// cleanHint tells the user how to recover from malformed markers.
const cleanHint = "run 'doktunnel hosts clean' to remove the doktunnel section, then 'doktunnel hosts sync' to write it again"

// hostsSyncJSON is the stable JSON form of "hosts sync".
type hostsSyncJSON struct {
	HostsFile string        `json:"hosts_file"`
	DryRun    bool          `json:"dry_run"`
	Changed   bool          `json:"changed"`
	Added     []hosts.Entry `json:"added"`
	Removed   []hosts.Entry `json:"removed"`
	Aliases   []string      `json:"aliases"`
}

// hostsListJSON is the stable JSON form of "hosts list".
type hostsListJSON struct {
	HostsFile string        `json:"hosts_file"`
	Entries   []hosts.Entry `json:"entries"`
}

// hostsCleanJSON is the stable JSON form of "hosts clean".
type hostsCleanJSON struct {
	HostsFile string        `json:"hosts_file"`
	Changed   bool          `json:"changed"`
	Removed   []hosts.Entry `json:"removed"`
}

func newHostsCommand() *Command {
	return &Command{
		Name:    "hosts",
		Summary: "Manage local hostname entries for forwarded services",
		Description: "doktunnel keeps one marked section in the hosts file, between '# BEGIN doktunnel' and " +
			"'# END doktunnel' lines, and never touches any other line. Writing it needs administrator " +
			"privileges, but doktunnel itself never runs as root: only the step that writes the file (and, on " +
			"macOS, adds lo0 aliases) is re-run elevated, through sudo on Linux and macOS or a UAC prompt on " +
			"Windows, and only when something changed. With --no-input, or when stdin is not a terminal, it " +
			"fails with elevation_required instead, and the hint is the exact command to run.",
		Subcommands: []*Command{
			newHostsSyncCommand(),
			{
				Name:    "list",
				Summary: "List the hostname entries in the doktunnel section of the hosts file",
				Description: "JSON output: `{\"hosts_file\":\"<path>\",\"entries\":[{\"ip\":\"<address>\",\"hostname\":\"<name>\"}]}`, " +
					"in file order; `entries` is empty when there is no doktunnel section.",
				Run: func(env *Env, args []string) error {
					if err := noArgs(args); err != nil {
						return err
					}
					return runHostsList(env)
				},
			},
			{
				Name:    "clean",
				Summary: "Remove the doktunnel section from the hosts file",
				Description: "Removes the doktunnel section and nothing else, even when its markers are malformed: a " +
					"marker that does not pair with another is removed alone. JSON output: " +
					"`{\"hosts_file\":\"<path>\",\"changed\":<bool>,\"removed\":[{\"ip\":\"<address>\",\"hostname\":\"<name>\"}]}`; `removed` is empty when the markers were malformed.",
				Run: func(env *Env, args []string) error {
					if err := noArgs(args); err != nil {
						return err
					}
					return runHostsClean(env)
				},
			},
			newHostsPrivilegedApplyCommand(),
		},
	}
}

func newHostsSyncCommand() *Command {
	var dryRun bool
	return &Command{
		Name:    "sync",
		Summary: "Write the hostnames of registered services to the hosts file",
		Description: "Computes the doktunnel section from the services registered in the address registry and " +
			"writes it only when it differs from the current one; on macOS it also adds missing lo0 aliases. " +
			"Hostnames are `<service>.<project>.<org>.<context>.internal`, or " +
			"`<service>.<compose>.<project>.<org>.<context>.internal` for a service inside a compose stack. " +
			"JSON output: `{\"hosts_file\":\"<path>\",\"dry_run\":<bool>,\"changed\":<bool>,\"added\":[...],\"removed\":[...],\"aliases\":[...]}`, " +
			"where `added` and `removed` hold `{\"ip\",\"hostname\"}` entries and `aliases` the lo0 addresses added " +
			"(or, with --dry-run, to add). `changed` is false when nothing had to change.",
		Flags: func(fs *flag.FlagSet) {
			dryRun = false
			fs.BoolVar(&dryRun, "dry-run", false, "print the changes without writing anything or asking for privileges")
		},
		Run: func(env *Env, args []string) error {
			if err := noArgs(args); err != nil {
				return err
			}
			return runHostsSync(env, dryRun)
		},
	}
}

func newHostsPrivilegedApplyCommand() *Command {
	var entriesFile string
	var clean bool
	return &Command{
		Name:    "privileged-apply",
		Summary: "Apply hosts changes with administrator privileges (internal)",
		Hidden:  true,
		Flags: func(fs *flag.FlagSet) {
			entriesFile, clean = "", false
			fs.StringVar(&entriesFile, "entries-file", "", "write the doktunnel section with the entries in this `file`")
			fs.BoolVar(&clean, "clean", false, "remove the doktunnel section")
		},
		Run: func(env *Env, args []string) error {
			if err := noArgs(args); err != nil {
				return err
			}
			if (entriesFile == "") == !clean {
				return clierr.New(clierr.InvalidArgument, "pass exactly one of --entries-file and --clean")
			}
			return runHostsPrivilegedApply(env, entriesFile, clean)
		},
	}
}

func noArgs(args []string) error {
	if len(args) > 0 {
		return clierr.Newf(clierr.InvalidArgument, "unexpected argument %q", args[0])
	}
	return nil
}

// hostsTarget is the hosts file a command works on.
type hostsTarget struct {
	path string
	// override is set when the path comes from hostsFileEnv; such a file is
	// never written with elevated privileges.
	override bool
}

func (e *Env) hostsTarget() hostsTarget {
	if e.HostsPath != "" {
		return hostsTarget{path: e.HostsPath}
	}
	if p := e.Getenv(hostsFileEnv); p != "" {
		return hostsTarget{path: p, override: true}
	}
	return hostsTarget{path: hosts.DefaultPath()}
}

// privilegedHostsPath is the file the privileged helper writes. It ignores
// hostsFileEnv on purpose.
func (e *Env) privilegedHostsPath() string {
	if e.HostsPath != "" {
		return e.HostsPath
	}
	return hosts.DefaultPath()
}

func (e *Env) registryPath() (string, error) {
	if e.RegistryPath != "" {
		return e.RegistryPath, nil
	}
	p, err := registry.DefaultPath()
	if err != nil {
		return "", clierr.New(clierr.Internal, err.Error()).
			WithHint("set XDG_STATE_HOME (Linux and macOS) or LOCALAPPDATA (Windows)")
	}
	return p, nil
}

// desiredEntries computes the doktunnel section from the address registry.
func (e *Env) desiredEntries() ([]hosts.Entry, error) {
	path, err := e.registryPath()
	if err != nil {
		return nil, err
	}
	reg, err := registry.Open(path)
	if err != nil {
		return nil, err
	}
	leases, err := reg.List()
	if err != nil {
		e := clierr.New(clierr.Internal, err.Error())
		if errors.Is(err, registry.ErrCorrupt) || errors.Is(err, registry.ErrUnsupportedVersion) {
			e = e.WithHint(fmt.Sprintf("fix or move %s; services get new addresses when forwarded again", path))
		}
		return nil, e
	}
	return hosts.Desired(leases)
}

// readHosts reads and parses the hosts file at path.
func readHosts(path string) ([]byte, *hosts.File, []hosts.Entry, error) {
	content, err := hosts.Read(path)
	if err != nil {
		return nil, nil, nil, err
	}
	f, err := hosts.Parse(content)
	if err != nil {
		return nil, nil, nil, malformedError(path, err)
	}
	current, err := f.Entries()
	if err != nil {
		return nil, nil, nil, malformedError(path, err)
	}
	return content, f, current, nil
}

func malformedError(path string, err error) error {
	return clierr.Newf(clierr.Internal, "%s: %v", path, err).WithHint(cleanHint)
}

func runHostsSync(env *Env, dryRun bool) error {
	ctx := context.Background()
	target := env.hostsTarget()
	desired, err := env.desiredEntries()
	if err != nil {
		return err
	}
	content, f, current, err := readHosts(target.path)
	if err != nil {
		return err
	}
	next := f.WithEntries(desired)
	hostsChanged := !bytes.Equal(next, content)

	var missing []netip.Addr
	if !target.override {
		if missing, err = env.Loopback.Missing(ctx, entryIPs(desired)); err != nil {
			return err
		}
	}
	out := hostsSyncJSON{
		HostsFile: target.path,
		DryRun:    dryRun,
		Changed:   hostsChanged || len(missing) > 0,
		Added:     subtract(desired, current),
		Removed:   subtract(current, desired),
		Aliases:   []string{},
	}
	for _, ip := range missing {
		out.Aliases = append(out.Aliases, ip.String())
	}

	if !dryRun && out.Changed {
		if err := env.applySync(ctx, target, next, desired, hostsChanged, len(missing) > 0); err != nil {
			return err
		}
	}
	if env.JSON {
		return output.WriteJSON(env.Stdout, out)
	}
	return writeSync(env, out)
}

// applySync writes next to the hosts file directly when the user may, and
// otherwise runs the privileged helper, which also adds missing aliases.
func (e *Env) applySync(ctx context.Context, target hostsTarget, next []byte, desired []hosts.Entry, hostsChanged, aliases bool) error {
	if hostsChanged {
		err := e.WriteHosts(target.path, next)
		switch {
		case err == nil:
			hostsChanged = false
		case !hosts.IsPermission(err):
			return err
		case target.override:
			return clierr.Newf(clierr.PermissionDenied, "%v", err).
				WithHint(fmt.Sprintf("%s is set; doktunnel never elevates to write such a file", hostsFileEnv))
		}
	}
	if !hostsChanged && !aliases {
		return nil
	}

	regPath, err := e.registryPath()
	if err != nil {
		return err
	}
	pending := filepath.Join(filepath.Dir(regPath), pendingFileName)
	if err := os.MkdirAll(filepath.Dir(pending), 0o700); err != nil {
		return fmt.Errorf("writing pending hosts entries: %w", err)
	}
	if err := os.WriteFile(pending, hosts.FormatEntries(desired), 0o600); err != nil {
		return fmt.Errorf("writing pending hosts entries: %w", err)
	}
	what := "updating " + target.path
	if !hostsChanged {
		what = "adding lo0 aliases"
	}
	if err := e.runPrivileged(ctx, what, "--entries-file", pending); err != nil {
		return err
	}
	if err := os.Remove(pending); err != nil {
		return fmt.Errorf("removing pending hosts entries: %w", err)
	}

	// The helper's own output may be hidden (UAC), so check its result.
	after, err := hosts.Read(target.path)
	if err != nil {
		return err
	}
	if f, err := hosts.Parse(after); err != nil || !bytes.Equal(f.WithEntries(desired), after) {
		return clierr.Newf(clierr.Internal, "%s does not hold the expected entries after the privileged helper ran", target.path).
			WithHint("run 'doktunnel hosts sync --dry-run' to see what differs")
	}
	return nil
}

// runPrivileged re-runs this binary's privileged helper with administrator
// privileges, or explains how to when prompting is not allowed.
func (e *Env) runPrivileged(ctx context.Context, what string, args ...string) error {
	exe, err := e.Executable()
	if err != nil {
		return fmt.Errorf("locating the doktunnel binary: %w", err)
	}
	argv := append([]string{exe, "hosts", "privileged-apply"}, args...)
	command := e.Elevator.Command(argv)
	if e.NoInput {
		return clierr.Newf(clierr.ElevationRequired, "%s needs administrator privileges, and prompting is not allowed", what).
			WithHint("run: " + command)
	}
	if !e.JSON {
		// With --json stderr stays silent; sudo or UAC still prompt.
		fmt.Fprintf(e.Stderr, "doktunnel: %s needs administrator privileges; running: %s\n", what, command)
	}
	if err := e.Elevator.Run(ctx, argv); err != nil {
		return clierr.Newf(clierr.ElevationRequired, "%s: %v", what, err).
			WithHint("run it yourself: " + command)
	}
	return nil
}

func writeSync(env *Env, out hostsSyncJSON) error {
	var err error
	switch {
	case !out.Changed:
		_, err = fmt.Fprintf(env.Stdout, "%s is up to date.\n", out.HostsFile)
	case out.DryRun:
		var b bytes.Buffer
		fmt.Fprintf(&b, "Changes to the doktunnel section of %s:\n", out.HostsFile)
		for _, e := range out.Removed {
			fmt.Fprintf(&b, "- %s\t%s\n", e.IP, e.Hostname)
		}
		for _, e := range out.Added {
			fmt.Fprintf(&b, "+ %s\t%s\n", e.IP, e.Hostname)
		}
		for _, ip := range out.Aliases {
			fmt.Fprintf(&b, "+ lo0 alias %s\n", ip)
		}
		_, err = env.Stdout.Write(b.Bytes())
	default:
		_, err = fmt.Fprintf(env.Stdout, "Updated %s: %d added, %d removed.\n", out.HostsFile, len(out.Added), len(out.Removed))
		if err == nil && len(out.Aliases) > 0 {
			_, err = fmt.Fprintf(env.Stdout, "Added %d lo0 aliases.\n", len(out.Aliases))
		}
	}
	return err
}

func runHostsList(env *Env) error {
	target := env.hostsTarget()
	_, _, current, err := readHosts(target.path)
	if err != nil {
		return err
	}
	if current == nil {
		current = []hosts.Entry{}
	}
	if env.JSON {
		return output.WriteJSON(env.Stdout, hostsListJSON{HostsFile: target.path, Entries: current})
	}
	if len(current) == 0 {
		_, err := fmt.Fprintf(env.Stderr, "No doktunnel entries in %s.\n", target.path)
		return err
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "IP\tHOSTNAME")
	for _, e := range current {
		fmt.Fprintf(tw, "%s\t%s\n", e.IP, e.Hostname)
	}
	return tw.Flush()
}

func runHostsClean(env *Env) error {
	ctx := context.Background()
	target := env.hostsTarget()
	content, err := hosts.Read(target.path)
	if err != nil {
		return err
	}
	next := hosts.Clean(content)
	out := hostsCleanJSON{HostsFile: target.path, Changed: !bytes.Equal(next, content), Removed: []hosts.Entry{}}
	// Malformed markers leave nothing reliable to report, but are cleaned.
	if f, err := hosts.Parse(content); err == nil {
		if es, err := f.Entries(); err == nil && es != nil {
			out.Removed = es
		}
	}

	if out.Changed {
		err := env.WriteHosts(target.path, next)
		switch {
		case err == nil:
		case !hosts.IsPermission(err):
			return err
		case target.override:
			return clierr.Newf(clierr.PermissionDenied, "%v", err).
				WithHint(fmt.Sprintf("%s is set; doktunnel never elevates to write such a file", hostsFileEnv))
		default:
			if err := env.runPrivileged(ctx, "cleaning "+target.path, "--clean"); err != nil {
				return err
			}
			if after, err := hosts.Read(target.path); err != nil || !bytes.Equal(hosts.Clean(after), after) {
				return clierr.Newf(clierr.Internal, "%s still has a doktunnel section after the privileged helper ran", target.path)
			}
		}
	}

	if env.JSON {
		return output.WriteJSON(env.Stdout, out)
	}
	if !out.Changed {
		_, err := fmt.Fprintf(env.Stdout, "%s has no doktunnel section.\n", target.path)
		return err
	}
	_, err = fmt.Fprintf(env.Stdout, "Removed the doktunnel section from %s.\n", target.path)
	return err
}

// runHostsPrivilegedApply is the privileged helper. It trusts nothing it is
// given: the entries file must hold only loopback addresses and .internal
// hostnames, and the target is always the system hosts file.
func runHostsPrivilegedApply(env *Env, entriesFile string, clean bool) error {
	ctx := context.Background()
	path := env.privilegedHostsPath()
	if clean {
		content, err := hosts.Read(path)
		if err != nil {
			return err
		}
		if next := hosts.Clean(content); !bytes.Equal(next, content) {
			return env.WriteHosts(path, next)
		}
		return nil
	}

	raw, err := os.ReadFile(entriesFile)
	if err != nil {
		return clierr.Newf(clierr.InvalidArgument, "reading entries: %v", err)
	}
	entries, err := hosts.ParseEntries(raw)
	if err != nil {
		return clierr.Newf(clierr.InvalidArgument, "%s: %v", entriesFile, err)
	}
	content, f, _, err := readHosts(path)
	if err != nil {
		return err
	}
	if next := f.WithEntries(entries); !bytes.Equal(next, content) {
		if err := env.WriteHosts(path, next); err != nil {
			return err
		}
	}
	missing, err := env.Loopback.Missing(ctx, entryIPs(entries))
	if err != nil {
		return err
	}
	return env.Loopback.Add(ctx, missing)
}

func entryIPs(entries []hosts.Entry) []netip.Addr {
	ips := make([]netip.Addr, len(entries))
	for i, e := range entries {
		ips[i] = e.IP
	}
	return ips
}

// subtract returns the entries of a that are not in b, in a's order.
func subtract(a, b []hosts.Entry) []hosts.Entry {
	in := make(map[hosts.Entry]bool, len(b))
	for _, e := range b {
		in[e] = true
	}
	out := []hosts.Entry{}
	for _, e := range a {
		if !in[e] {
			out = append(out, e)
		}
	}
	return out
}
