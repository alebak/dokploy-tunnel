package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
	"github.com/alebak/dokploy-tunnel/internal/hosts"
	"github.com/alebak/dokploy-tunnel/internal/lowport"
	"github.com/alebak/dokploy-tunnel/internal/output"
	"github.com/alebak/dokploy-tunnel/internal/registry"
	"github.com/alebak/dokploy-tunnel/internal/runstate"
)

// hostsFileEnv points the unprivileged hosts commands at another file. It
// exists for tests and is never honored by the privileged helper, so it
// cannot redirect a write made with administrator privileges.
const hostsFileEnv = "DOKTUNNEL_HOSTS_FILE"

// pendingFileName holds the entries handed to the privileged helper when
// they cannot travel on its standard input, next to the address registry.
const pendingFileName = "pending-hosts"

// entriesStdin is the --entries-file value that reads standard input.
const entriesStdin = "-"

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
		Summary: "Write the hostnames of running forwards to the hosts file",
		Description: "Computes the doktunnel section from the forwards running now, the hostnames of the services " +
			"every running 'doktunnel forward' process listens for, and writes it only when it differs from the " +
			"current one, so names of stopped forwards are removed and, with no forward running, the section is " +
			"too; on macOS it also adds missing lo0 aliases. " +
			"Hostnames are `<appName>.internal` for an application or database, and " +
			"`<service>.<appName>.internal` for a service inside a compose stack, where appName is the name Dokploy " +
			"deploys the service or stack under (its Dokploy ID when it cannot be read); a name an older lease " +
			"already holds gets the context as an extra label, `<appName>.<context>.internal`. " +
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
	var entriesFile, ports, socket string
	var clean bool
	return &Command{
		Name:    "privileged-apply",
		Summary: "Apply hosts changes with administrator privileges (internal)",
		Hidden:  true,
		Flags: func(fs *flag.FlagSet) {
			entriesFile, ports, socket, clean = "", "", "", false
			fs.StringVar(&entriesFile, "entries-file", "", "write the doktunnel section with the entries in this `file` (- for standard input)")
			fs.BoolVar(&clean, "clean", false, "remove the doktunnel section")
			fs.StringVar(&ports, "ports", "", "listen on these comma-separated `addresses` (<ip>:<port>, below the unprivileged port limit) and hand the listeners to --socket (Linux)")
			fs.StringVar(&socket, "socket", "", "the forward process's channel `name` for --ports")
		},
		Run: func(env *Env, args []string) error {
			if err := noArgs(args); err != nil {
				return err
			}
			switch {
			case clean && (entriesFile != "" || ports != "" || socket != ""):
				return clierr.New(clierr.InvalidArgument, "--clean takes no other flag")
			case !clean && entriesFile == "" && ports == "":
				return clierr.New(clierr.InvalidArgument, "pass --entries-file, --ports or --clean")
			case (ports == "") != (socket == ""):
				return clierr.New(clierr.InvalidArgument, "pass --ports and --socket together")
			}
			return runHostsPrivilegedApply(env, privilegedRequest{entriesFile: entriesFile, clean: clean, ports: ports, socket: socket})
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

// namedEntries computes the hostname of every named lease in the address
// registry. Hostnames are assigned over all of them, running or not, so a
// service keeps its name whichever forwards run.
func (e *Env) namedEntries() ([]hosts.Entry, error) {
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

// liveEntries computes the doktunnel section: the entries of namedEntries
// whose address a running forward process listens on, as its state file
// records. A state file that cannot be read, such as one written by another
// doktunnel version, keeps nothing.
func (e *Env) liveEntries() ([]hosts.Entry, error) {
	named, err := e.namedEntries()
	if err != nil {
		return nil, err
	}
	dir, err := e.forwardStateDir()
	if err != nil {
		return nil, err
	}
	procs, err := runstate.List(dir)
	if err != nil {
		return nil, clierr.New(clierr.Internal, err.Error())
	}
	live := map[netip.Addr]bool{}
	for _, p := range procs {
		if p.Err != nil || !e.ProcessAlive(p.PID) {
			continue
		}
		for _, f := range p.Process.Forwards {
			live[f.IP] = true
		}
	}
	out := []hosts.Entry{}
	for _, entry := range named {
		if live[entry.IP] {
			out = append(out, entry)
		}
	}
	return out, nil
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
	out, err := env.syncHosts(context.Background(), syncOptions{dryRun: dryRun})
	if err != nil {
		return err
	}
	if env.JSON {
		return output.WriteJSON(env.Stdout, out)
	}
	return writeSync(env, out)
}

// syncOptions tune syncHosts.
type syncOptions struct {
	// dryRun changes nothing.
	dryRun bool
	// exiting is set when a forward process removes its own hostnames as
	// it stops: lo0 aliases are left alone, and when prompting is not
	// allowed the sync fails without leaving a pending entries file.
	exiting bool
	// ports, when not nil, are addresses below the unprivileged port limit
	// that forward is about to listen on: the privileged helper opens them
	// in the same elevation, and their listeners land in ports.listeners.
	ports *lowPorts
}

// lowPorts are addresses only the privileged helper may listen on.
type lowPorts struct {
	addrs []netip.AddrPort
	// listeners are the helper's listeners, keyed by address, once the
	// sync has run.
	listeners map[netip.AddrPort]net.Listener
}

func (p *lowPorts) needed() bool { return p != nil && len(p.addrs) > 0 }

// closeAll closes every listener the helper handed over.
func (p *lowPorts) closeAll() {
	if p == nil {
		return
	}
	for _, ln := range p.listeners {
		_ = ln.Close()
	}
}

// receiveTimeout bounds how long forward waits for the helper's listeners
// once the helper has exited successfully; by then they are already queued.
const receiveTimeout = 10 * time.Second

// syncHosts brings the doktunnel section of the hosts file, and on macOS the
// lo0 aliases, in line with the running forwards (see liveEntries),
// elevating once and only when something has to change or opts.ports needs
// the helper. It returns what changed in the hosts file and aliases.
func (e *Env) syncHosts(ctx context.Context, opts syncOptions) (hostsSyncJSON, error) {
	target := e.hostsTarget()
	desired, err := e.liveEntries()
	if err != nil {
		return hostsSyncJSON{}, err
	}
	content, f, current, err := readHosts(target.path)
	if err != nil {
		return hostsSyncJSON{}, err
	}
	next := f.WithEntries(desired)
	hostsChanged := !bytes.Equal(next, content)

	var missing []netip.Addr
	if !target.override && !opts.exiting {
		if missing, err = e.Loopback.Missing(ctx, entryIPs(desired)); err != nil {
			return hostsSyncJSON{}, err
		}
	}
	out := hostsSyncJSON{
		HostsFile: target.path,
		DryRun:    opts.dryRun,
		Changed:   hostsChanged || len(missing) > 0,
		Added:     subtract(desired, current),
		Removed:   subtract(current, desired),
		Aliases:   []string{},
	}
	for _, ip := range missing {
		out.Aliases = append(out.Aliases, ip.String())
	}

	if !opts.dryRun && (out.Changed || opts.ports.needed()) {
		if err := e.applySync(ctx, opts, target, next, desired, hostsChanged, len(missing) > 0); err != nil {
			return hostsSyncJSON{}, err
		}
	}
	return out, nil
}

// applySync writes next to the hosts file directly when the user may, and
// otherwise runs the privileged helper, which also adds missing aliases and
// opens opts.ports, all in one elevation.
func (e *Env) applySync(ctx context.Context, opts syncOptions, target hostsTarget, next []byte, desired []hosts.Entry, hostsChanged, aliases bool) error {
	if opts.ports.needed() && e.NoInput {
		// Fail before writing anything: the ports cannot be opened.
		return clierr.Newf(clierr.ElevationRequired, "listening on %s needs administrator privileges, and prompting is not allowed",
			formatAddrs(opts.ports.addrs)).WithHint(e.portsHint())
	}
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
	if !hostsChanged && !aliases && !opts.ports.needed() {
		return nil
	}

	var steps []string
	switch {
	case hostsChanged:
		steps = append(steps, "updating "+target.path)
	case aliases:
		steps = append(steps, "adding lo0 aliases")
	}
	withEntries := len(steps) > 0
	if opts.ports.needed() {
		steps = append(steps, "listening on "+formatAddrs(opts.ports.addrs))
		var entries []byte
		if withEntries {
			entries = hosts.FormatEntries(desired)
		}
		if err := e.runPrivilegedPorts(ctx, strings.Join(steps, " and "), withEntries, entries, opts.ports); err != nil {
			return err
		}
		if !withEntries {
			return nil
		}
		return e.checkSynced(target, desired)
	}
	what := steps[0]
	if opts.exiting && e.NoInput {
		return clierr.Newf(clierr.ElevationRequired, "%s needs administrator privileges, and prompting is not allowed", what)
	}
	if err := e.runPrivilegedEntries(ctx, what, hosts.FormatEntries(desired)); err != nil {
		return err
	}
	return e.checkSynced(target, desired)
}

// checkSynced checks the hosts file after the privileged helper ran: its
// own output may be hidden (UAC), so its result is read back.
func (e *Env) checkSynced(target hostsTarget, desired []hosts.Entry) error {
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

// runPrivilegedEntries runs the privileged helper with entries, the output
// of hosts.FormatEntries.
//
// The helper runs as root, so it should never open a path its caller chose:
// a restricted sudoers rule for it would otherwise let the caller point root
// at any file. Where the elevator passes a standard input (sudo, which
// prompts on the terminal instead), the entries travel on it and no file is
// written. A UAC prompt cannot pass one, so on Windows the entries go in a
// pending file in the user's own state directory, which the helper reads
// with the checks of hosts.ReadEntriesFile. When prompting is not allowed
// the hint must still be runnable, so the entries also go in the pending
// file, which the user's own shell redirects into the helper.
func (e *Env) runPrivilegedEntries(ctx context.Context, what string, entries []byte) error {
	regPath, err := e.registryPath()
	if err != nil {
		return err
	}
	pending := filepath.Join(filepath.Dir(regPath), pendingFileName)
	if e.Elevator.PipesStdin() && !e.NoInput {
		if err := e.removePendingEntries(); err != nil {
			return err
		}
		return e.runPrivileged(ctx, what, bytes.NewReader(entries), "", "--entries-file", entriesStdin)
	}

	if err := os.MkdirAll(filepath.Dir(pending), 0o700); err != nil {
		return fmt.Errorf("writing pending hosts entries: %w", err)
	}
	if err := os.WriteFile(pending, entries, 0o600); err != nil {
		return fmt.Errorf("writing pending hosts entries: %w", err)
	}
	if e.Elevator.PipesStdin() {
		err = e.runPrivileged(ctx, what, nil, pending, "--entries-file", entriesStdin)
	} else {
		err = e.runPrivileged(ctx, what, nil, "", "--entries-file", pending)
	}
	if err != nil {
		return err
	}
	if err := os.Remove(pending); err != nil {
		return fmt.Errorf("removing pending hosts entries: %w", err)
	}
	return nil
}

// runPrivilegedPorts runs the privileged helper to listen on ports.addrs
// and, with withEntries, to apply entries too, in one elevation, then takes
// the helper's listeners into ports.listeners. The listeners can only reach
// this process while it runs the helper itself, so when the elevation
// fails there is no command to suggest instead (see portsHint); applySync
// already refused when prompting is not allowed.
func (e *Env) runPrivilegedPorts(ctx context.Context, what string, withEntries bool, entries []byte, ports *lowPorts) error {
	if !e.Elevator.PipesStdin() {
		return clierr.Newf(clierr.Internal, "%s needs administrator privileges, which this system's elevator cannot provide", what)
	}
	recv, err := e.PortBroker.Open()
	if err != nil {
		return clierr.Newf(clierr.Internal, "%s: %v", what, err)
	}
	defer recv.Close()

	var args []string
	var stdin io.Reader
	if withEntries {
		if err := e.removePendingEntries(); err != nil {
			return err
		}
		args = append(args, "--entries-file", entriesStdin)
		stdin = bytes.NewReader(entries)
	}
	args = append(args, "--ports", lowport.FormatAddrs(ports.addrs), "--socket", recv.Name())

	type received struct {
		listeners map[netip.AddrPort]net.Listener
		err       error
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan received, 1)
	go func() {
		ls, err := recv.Receive(rctx, ports.addrs)
		done <- received{ls, err}
	}()

	argv, command, err := e.helperCommand("", args...)
	if err != nil {
		cancel()
		<-done
		return err
	}
	if !e.JSON {
		fmt.Fprintf(e.Stderr, "doktunnel: %s needs administrator privileges; running: %s\n", what, command)
	}
	runErr := e.Elevator.Run(ctx, argv, stdin)
	if runErr != nil {
		cancel()
	} else {
		// The helper sent everything before it exited; this only bounds
		// a receive that would otherwise never end.
		stop := time.AfterFunc(receiveTimeout, cancel)
		defer stop.Stop()
	}
	r := <-done
	if runErr != nil {
		for _, ln := range r.listeners {
			_ = ln.Close()
		}
		return clierr.Newf(clierr.ElevationRequired, "%s: %v", what, runErr).WithHint(e.portsHint())
	}
	if r.err != nil {
		return clierr.Newf(clierr.Internal, "%s: receiving the listeners from the privileged helper: %v", what, r.err)
	}
	ports.listeners = r.listeners
	return nil
}

// portsHint is the hint of an elevation_required error for low ports: a
// command the user runs separately cannot hand the listeners back.
func (e *Env) portsHint() string {
	return fmt.Sprintf("ports below %d need administrator privileges on Linux; run 'doktunnel forward' from a terminal, "+
		"without --no-input, and enter your password when asked", e.PortLimit())
}

// removePendingEntries removes the pending entries file a no-input run left
// behind, which is stale once the entries travel on standard input.
func (e *Env) removePendingEntries() error {
	regPath, err := e.registryPath()
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(filepath.Dir(regPath), pendingFileName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing pending hosts entries: %w", err)
	}
	return nil
}

// helperCommand returns the privileged helper's argv for args and the
// command line the user would run for it.
func (e *Env) helperCommand(stdinFile string, args ...string) ([]string, string, error) {
	exe, err := e.Executable()
	if err != nil {
		return nil, "", fmt.Errorf("locating the doktunnel binary: %w", err)
	}
	argv := append([]string{exe, "hosts", "privileged-apply"}, args...)
	return argv, e.Elevator.Command(argv, stdinFile), nil
}

// formatAddrs renders addrs for messages.
func formatAddrs(addrs []netip.AddrPort) string {
	return strings.ReplaceAll(lowport.FormatAddrs(addrs), ",", ", ")
}

// runPrivileged re-runs this binary's privileged helper with administrator
// privileges, or explains how to when prompting is not allowed. stdin, when
// not nil, is the helper's standard input; stdinFile is the file the hint
// redirects into it.
func (e *Env) runPrivileged(ctx context.Context, what string, stdin io.Reader, stdinFile string, args ...string) error {
	argv, command, err := e.helperCommand(stdinFile, args...)
	if err != nil {
		return err
	}
	if e.NoInput {
		return clierr.Newf(clierr.ElevationRequired, "%s needs administrator privileges, and prompting is not allowed", what).
			WithHint("run: " + command)
	}
	if !e.JSON {
		// With --json stderr stays silent; sudo or UAC still prompt.
		fmt.Fprintf(e.Stderr, "doktunnel: %s needs administrator privileges; running: %s\n", what, command)
	}
	if err := e.Elevator.Run(ctx, argv, stdin); err != nil {
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
			if err := env.runPrivileged(ctx, "cleaning "+target.path, nil, "", "--clean"); err != nil {
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

// privilegedRequest is what the privileged helper is asked to do.
type privilegedRequest struct {
	// entriesFile holds the entries to write, or is entriesStdin.
	entriesFile string
	// clean removes the doktunnel section.
	clean bool
	// ports are addresses to listen on and hand to the forward process
	// over the channel called socket (see package lowport).
	ports, socket string
}

// runHostsPrivilegedApply is the privileged helper. It trusts nothing it is
// given: the entries, read from standard input or from a regular file (see
// runPrivilegedEntries), must hold only leased loopback addresses and
// doktunnel .internal hostnames, errors never echo them, and the target is
// always the system hosts file. Ports must be below the unprivileged port
// limit on leased loopback addresses, and their listeners only go to a
// process of the user who ran sudo. Everything is validated, and every port
// bound, before the hosts file is written, since hosts.Write truncates it
// first and a port in use must change nothing.
func runHostsPrivilegedApply(env *Env, req privilegedRequest) error {
	ctx := context.Background()
	path := env.privilegedHostsPath()
	if req.clean {
		content, err := hosts.Read(path)
		if err != nil {
			return err
		}
		if next := hosts.Clean(content); !bytes.Equal(next, content) {
			return env.WriteHosts(path, next)
		}
		return nil
	}

	var addrs []netip.AddrPort
	var peerUID int
	if req.ports != "" {
		var err error
		if addrs, err = lowport.ParseAddrs(req.ports, env.PortLimit()); err != nil {
			return clierr.Newf(clierr.InvalidArgument, "--ports: %v", err)
		}
		if !lowport.ValidName(req.socket) {
			return clierr.New(clierr.InvalidArgument, "--socket: not a doktunnel channel name")
		}
		if peerUID, err = invokingUID(env); err != nil {
			return err
		}
	}

	var entries []hosts.Entry
	if req.entriesFile != "" {
		var err error
		source := req.entriesFile
		if req.entriesFile == entriesStdin {
			source = "standard input"
			entries, err = hosts.ReadEntries(env.Stdin)
		} else {
			entries, err = hosts.ReadEntriesFile(req.entriesFile)
		}
		if err != nil {
			return clierr.Newf(clierr.InvalidArgument, "%s: %v", source, err)
		}
	}

	var bound []lowport.Bound
	if len(addrs) > 0 {
		var err error
		if bound, err = env.PortBroker.Bind(addrs); err != nil {
			return clierr.New(clierr.Internal, err.Error())
		}
		// The forward process holds its own copies once they are sent.
		defer func() {
			for _, b := range bound {
				_ = b.Listener.Close()
			}
		}()
	}

	if req.entriesFile != "" {
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
		if err := env.Loopback.Add(ctx, missing); err != nil {
			return err
		}
	}

	if len(bound) > 0 {
		if err := env.PortBroker.Send(req.socket, peerUID, bound); err != nil {
			return clierr.New(clierr.Internal, err.Error())
		}
	}
	return nil
}

// invokingUID is the user the helper acts for: the one sudo records in
// SUDO_UID, or the helper's own when it was not run through sudo.
func invokingUID(env *Env) (int, error) {
	s := env.Getenv("SUDO_UID")
	if s == "" {
		return os.Getuid(), nil
	}
	uid, err := strconv.Atoi(s)
	if err != nil || uid < 0 {
		return 0, clierr.New(clierr.InvalidArgument, "SUDO_UID is not a user ID")
	}
	return uid, nil
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
