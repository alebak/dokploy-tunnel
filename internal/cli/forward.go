package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
	"github.com/alebak/dokploy-tunnel/internal/dokploy"
	"github.com/alebak/dokploy-tunnel/internal/forward"
	"github.com/alebak/dokploy-tunnel/internal/hostname"
	"github.com/alebak/dokploy-tunnel/internal/output"
	"github.com/alebak/dokploy-tunnel/internal/prompt"
	"github.com/alebak/dokploy-tunnel/internal/registry"
	"github.com/alebak/dokploy-tunnel/internal/runstate"
	"github.com/alebak/dokploy-tunnel/internal/tunnel"
)

// shutdownTimeout bounds how long forward waits for its tunnels to close
// after the stop signal. Windows ends a process a few seconds after its
// console is closed, so it stays below that.
const shutdownTimeout = 3 * time.Second

// forwardJSON is the stable JSON form of "forward", printed once every
// forward listens. It is the state file's content without its version.
type forwardJSON struct {
	Context      string             `json:"context"`
	PID          int                `json:"pid"`
	StartedAt    time.Time          `json:"started_at"`
	CompanionURL string             `json:"companion_url"`
	Forwards     []runstate.Forward `json:"forwards"`
}

// forwardOptions are the flags of "forward".
type forwardOptions struct {
	project string
	all     bool
	// port is the --port value; portSet tells --port 0 from no --port.
	port     int
	portSet  bool
	allPorts bool
}

func newForwardCommand() *Command {
	var opts forwardOptions
	return &Command{
		Name:    "forward",
		Summary: "Forward Dokploy services to stable local hostnames and ports",
		Description: "Forwards each service on its own loopback address, leased from 127.77.0.0/16, and its real port, " +
			"never on 0.0.0.0, and opens one tunnel through the context's companion for every local connection. " +
			"Services are named as 'doktunnel services' lists them: by name, `<compose>/<service>` for a service " +
			"inside a compose stack, or ID; a compose stack itself is not forwardable. Without services or --all " +
			"it asks which to forward, or fails with missing_input when it cannot ask. " +
			"The port is --port, else a database's default port, else the port the companion reports for the " +
			"container; several ports need --port or --all-ports, and none fails with missing_input naming --port. " +
			"The hostnames are written to the hosts file as 'doktunnel hosts sync' does, elevating once only when " +
			"something changed (with --no-input: elevation_required and the command to run). If any address " +
			"cannot be listened on, nothing is forwarded. Runs in the foreground until Ctrl+C or SIGTERM, then " +
			"closes every tunnel and exits 0. Human output: one `<hostname> (<ip>:<port>) → <service>` line per " +
			"forward on stdout, connection events on stderr. JSON output, printed once every forward listens: " +
			"`{\"context\":\"<name>\",\"pid\":<pid>,\"started_at\":\"<RFC 3339>\",\"companion_url\":\"<URL>\",\"forwards\":[...]}`, " +
			"where each forward has `target` (`type`, `id`, `name`), `hostname`, `ip` and `port`; nothing else is " +
			"printed afterwards, not even connection events.",
		Args: "[<service>...]",
		Flags: func(fs *flag.FlagSet) {
			opts = forwardOptions{}
			fs.StringVar(&opts.project, "project", "", "only consider services of the project with this `name or ID`")
			fs.BoolVar(&opts.all, "all", false, "forward every service, of --project when given")
			fs.Func("port", "forward this container `port`; only with a single service", func(v string) error {
				n, err := strconv.Atoi(v)
				if err != nil {
					return errors.New("not a number")
				}
				opts.port, opts.portSet = n, true
				return nil
			})
			fs.BoolVar(&opts.allPorts, "all-ports", false, "forward every port the companion reports, instead of failing when there are several")
		},
		Run: func(env *Env, args []string) error {
			return runForward(env, args, opts)
		},
	}
}

// forwardTarget is a service that can be forwarded.
type forwardTarget struct {
	// typ is the Dokploy type, or typeComposeService.
	typ string
	// id is the Dokploy ID, or "<compose ID>/<service>"; it is the
	// registry's service ID.
	id string
	// name is the display name, or "<compose name>/<service>".
	name string
	// project and environment name where the service lives.
	project, environment string
	// ref addresses the service on the companion.
	ref tunnel.TargetRef
	// names are what the hostname is built from, without the context, and
	// without the appName until withAppNames fills it in.
	names hostname.Names
}

// dokployKey is the detailKey of the Dokploy service t belongs to: the
// service itself, or the compose stack of a service inside one.
func (t forwardTarget) dokployKey() string {
	return string(t.ref.ServiceType) + "/" + t.ref.ServiceID
}

// label describes t in the picker and in messages.
func (t forwardTarget) label() string {
	return fmt.Sprintf("%s (%s %s, %s/%s)", t.name, t.typ, t.id, t.project, t.environment)
}

// targets lists the forwardable services of the catalog in the order
// "services" shows them. A compose stack is replaced by its services. A
// name that could not be read falls back to the ID.
func (c *catalog) targets() []forwardTarget {
	var out []forwardTarget
	for _, p := range c.projects {
		project := orID(p.Name, p.ID)
		for _, e := range p.Environments {
			env := orID(e.Name, e.ID)
			for _, s := range e.Services {
				name := orID(s.Name, s.ID)
				if s.Type != dokploy.ServiceCompose {
					out = append(out, forwardTarget{
						typ: string(s.Type), id: s.ID, name: name, project: project, environment: env,
						ref: tunnel.TargetRef{ServiceType: s.Type, ServiceID: s.ID},
					})
					continue
				}
				for _, svc := range c.inside[s.ID] {
					out = append(out, forwardTarget{
						typ: typeComposeService, id: s.ID + "/" + svc, name: name + "/" + svc, project: project, environment: env,
						ref:   tunnel.TargetRef{ServiceType: dokploy.ServiceCompose, ServiceID: s.ID, ComposeService: svc},
						names: hostname.Names{ComposeService: svc},
					})
				}
			}
		}
	}
	return out
}

// withAppNames returns targets with the appName of each one's Dokploy
// service filled in, reading with at most detailConcurrency calls in flight
// the details the catalog has not read yet. A service whose details cannot
// be read is named after its Dokploy ID instead, with a warning. Only
// cancellation of ctx is returned as an error.
func (c *catalog) withAppNames(ctx context.Context, e *Env, targets []forwardTarget) ([]forwardTarget, error) {
	var missing []forwardTarget
	queued := map[string]bool{}
	for _, t := range targets {
		if k := t.dokployKey(); c.appNames[k] == "" && !queued[k] {
			queued[k] = true
			missing = append(missing, t)
		}
	}
	failed := map[string]error{}
	var mu sync.Mutex
	err := forEachBounded(ctx, missing, func(t forwardTarget) {
		details, err := c.details.Details(ctx, t.ref.ServiceType, t.ref.ServiceID)
		mu.Lock()
		defer mu.Unlock()
		if err == nil && details.AppName == "" {
			err = errors.New("the service details hold no appName")
		}
		if err != nil {
			failed[t.dokployKey()] = err
			return
		}
		if c.appNames == nil {
			c.appNames = map[string]string{}
		}
		c.appNames[t.dokployKey()] = details.AppName
	})
	if err != nil {
		return nil, err
	}
	for _, t := range missing {
		if err := failed[t.dokployKey()]; err != nil {
			e.warn(fmt.Sprintf("%s %s: appName unknown (%v); its hostname uses the Dokploy ID instead", t.ref.ServiceType, t.ref.ServiceID, err))
		}
	}
	out := slices.Clone(targets)
	for i := range out {
		out[i].names.AppName = orID(c.appNames[out[i].dokployKey()], out[i].ref.ServiceID)
	}
	return out, nil
}

func orID(name, id string) string {
	if name == "" {
		return id
	}
	return name
}

// selectTargets picks the targets args name, every target with all, or
// asks for them.
func (c *catalog) selectTargets(env *Env, args []string, all bool) ([]forwardTarget, error) {
	targets := c.targets()
	if all {
		if len(targets) == 0 {
			return nil, clierr.Newf(clierr.NotFound, "no forwardable services in context %q", c.context.Name).
				WithHint("run 'doktunnel services' to list the services the API key can see")
		}
		return targets, nil
	}
	if len(args) == 0 {
		if len(targets) == 0 {
			return nil, clierr.Newf(clierr.NotFound, "no forwardable services in context %q", c.context.Name).
				WithHint("run 'doktunnel services' to list the services the API key can see")
		}
		options := make([]string, len(targets))
		for i, t := range targets {
			options[i] = t.label()
		}
		picked, err := env.Input.Choose(prompt.Choice{
			Label:   "Services to forward",
			Options: options,
			Missing: clierr.New(clierr.MissingInput, "missing value for <service>: name the services to forward").
				WithHint("pass one or more <service> arguments as 'doktunnel services' lists them, or --all"),
		})
		if err != nil {
			return nil, err
		}
		selected := make([]forwardTarget, len(picked))
		for i, idx := range picked {
			selected[i] = targets[idx]
		}
		return selected, nil
	}

	var selected []forwardTarget
	for _, arg := range args {
		t, err := c.match(targets, arg)
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(selected, func(s forwardTarget) bool { return s.id == t.id }) {
			selected = append(selected, t)
		}
	}
	return selected, nil
}

// match returns the one target arg names: by ID first, since IDs are
// unique, and otherwise by display name.
func (c *catalog) match(targets []forwardTarget, arg string) (forwardTarget, error) {
	for _, t := range targets {
		if t.id == arg {
			return t, nil
		}
	}
	var byName []forwardTarget
	for _, t := range targets {
		if t.name == arg {
			byName = append(byName, t)
		}
	}
	switch len(byName) {
	case 1:
		return byName[0], nil
	case 0:
		return forwardTarget{}, c.noMatch(targets, arg)
	}
	labels := make([]string, len(byName))
	for i, t := range byName {
		labels[i] = t.label()
	}
	return forwardTarget{}, clierr.Newf(clierr.InvalidArgument, "%q names several services: %s", arg, strings.Join(labels, "; ")).
		WithHint("pass the service ID instead of its name")
}

// noMatch explains why no target is called arg: it may name a compose
// stack, which is not forwardable, or nothing at all.
func (c *catalog) noMatch(targets []forwardTarget, arg string) error {
	for _, p := range c.projects {
		for _, e := range p.Environments {
			for _, s := range e.Services {
				if s.Type != dokploy.ServiceCompose || (s.ID != arg && s.Name != arg) {
					continue
				}
				var inside []string
				for _, t := range targets {
					if t.ref.ServiceType == dokploy.ServiceCompose && t.ref.ServiceID == s.ID {
						inside = append(inside, t.name)
					}
				}
				if len(inside) == 0 {
					return clierr.Newf(clierr.InvalidArgument, "%q is a compose stack, and its services are unknown", arg).
						WithHint("forward the services inside a compose stack; run 'doktunnel services' to see why they are unknown")
				}
				return clierr.Newf(clierr.InvalidArgument, "%q is a compose stack; forward the services inside it: %s", arg, strings.Join(inside, ", ")).
					WithHint("pass <compose>/<service>, such as " + inside[0])
			}
		}
	}
	return clierr.Newf(clierr.NotFound, "no service named %q or with that ID in context %q", arg, c.context.Name).
		WithHint("run 'doktunnel services' to list the services the API key can see")
}

// portLister asks a companion which ports a service exposes.
type portLister interface {
	Ports(ctx context.Context, ref tunnel.TargetRef) ([]tunnel.Port, error)
}

// resolvePorts returns the ports to forward for t: --port, else the
// database's default port, else what the companion reports. It never
// guesses.
func resolvePorts(ctx context.Context, companion portLister, t forwardTarget, opts forwardOptions) ([]int, error) {
	if opts.portSet {
		return []int{opts.port}, nil
	}
	if t.ref.ComposeService == "" {
		if p := dokploy.DefaultPort(t.ref.ServiceType); p != dokploy.UnknownPort {
			return []int{p}, nil
		}
	}
	reported, err := companion.Ports(ctx, t.ref)
	if err != nil {
		return nil, forward.CLIError(err)
	}
	ports := make([]int, len(reported))
	for i, p := range reported {
		ports[i] = p.Port
	}
	switch {
	case len(ports) == 0:
		return nil, clierr.Newf(clierr.MissingInput, "missing value for --port: the companion reports no exposed port for %s", t.name).
			WithHint("pass --port <n> with the port the service listens on")
	case len(ports) > 1 && !opts.allPorts:
		list := make([]string, len(ports))
		for i, p := range ports {
			list[i] = strconv.Itoa(p)
		}
		return nil, clierr.Newf(clierr.MissingInput, "missing value for --port: %s exposes several ports: %s", t.name, strings.Join(list, ", ")).
			WithHint("pass --port <n> to forward one of them, or --all-ports to forward each")
	}
	return ports, nil
}

// plannedForward is one listener to open.
type plannedForward struct {
	target   forwardTarget
	ip       netip.Addr
	hostname string
	port     int
}

func validateForwardOptions(args []string, opts forwardOptions) error {
	switch {
	case opts.all && len(args) > 0:
		return clierr.New(clierr.InvalidArgument, "pass either service names or --all, not both")
	case opts.portSet && opts.allPorts:
		return clierr.New(clierr.InvalidArgument, "pass either --port or --all-ports, not both")
	case opts.portSet && (opts.port < 1 || opts.port > 65535):
		return clierr.Newf(clierr.InvalidArgument, "invalid --port %d: it must be a number from 1 to 65535", opts.port)
	}
	return nil
}

func runForward(env *Env, args []string, opts forwardOptions) error {
	if err := validateForwardOptions(args, opts); err != nil {
		return err
	}
	ctx := context.Background()
	cat, err := env.loadCatalog(ctx, opts.project)
	if err != nil {
		return err
	}
	selected, err := cat.selectTargets(env, args, opts.all)
	if err != nil {
		return err
	}
	if opts.portSet && len(selected) > 1 {
		return clierr.Newf(clierr.InvalidArgument, "--port applies to a single service, and %d are selected", len(selected)).
			WithHint("forward the services one at a time to pick a port for each")
	}
	if selected, err = cat.withAppNames(ctx, env, selected); err != nil {
		return err
	}

	companionURL := cat.context.CompanionURL
	if companionURL == "" {
		companionURL = defaultCompanionURL(cat.base).String()
	}
	client, err := forward.NewClient(companionURL, cat.key)
	if err != nil {
		return clierr.Newf(clierr.InvalidArgument, "context %q: %v", cat.context.Name, err).
			WithHint(fmt.Sprintf("run 'doktunnel context set-companion %s <URL>'", cat.context.Name))
	}
	ports := make([][]int, len(selected))
	for i, t := range selected {
		if ports[i], err = resolvePorts(ctx, client, t, opts); err != nil {
			return err
		}
	}

	planned, err := env.leaseAndSync(ctx, cat, selected, ports)
	if err != nil {
		return err
	}
	return env.serveForwards(cat.context.Name, companionURL, client, planned)
}

// leaseAndSync leases an address for every target, records what its
// hostname is built from, and syncs the hosts file and loopback aliases.
func (e *Env) leaseAndSync(ctx context.Context, cat *catalog, targets []forwardTarget, ports [][]int) ([]plannedForward, error) {
	regPath, err := e.registryPath()
	if err != nil {
		return nil, err
	}
	reg, err := registry.Open(regPath)
	if err != nil {
		return nil, err
	}
	ips := make([]netip.Addr, len(targets))
	for i, t := range targets {
		k := registry.Key{Instance: cat.context.URL, OrganizationID: cat.context.OrganizationID, ServiceID: t.id}
		if ips[i], err = reg.Lease(k); err != nil {
			return nil, leaseError(regPath, err)
		}
		names := t.names
		names.Context = cat.context.Name
		if _, err := reg.SetNames(k, names); err != nil {
			return nil, leaseError(regPath, err)
		}
	}

	out, desired, err := e.syncHosts(ctx, false)
	if err != nil {
		return nil, err
	}
	if out.Changed && !e.JSON {
		fmt.Fprintf(e.Stderr, "Updated %s: %d added, %d removed.\n", out.HostsFile, len(out.Added), len(out.Removed))
	}
	byIP := make(map[netip.Addr]string, len(desired))
	for _, d := range desired {
		byIP[d.IP] = d.Hostname
	}
	// On macOS an address needs a lo0 alias before it can be listened on;
	// the sync adds them, unless it skipped them for an overridden file.
	missing, err := e.Loopback.Missing(ctx, ips)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		return nil, clierr.Newf(clierr.ElevationRequired, "lo0 has no alias for %s, so it cannot be listened on", missing[0]).
			WithHint(fmt.Sprintf("run: sudo /sbin/ifconfig lo0 alias %s up", missing[0]))
	}

	var planned []plannedForward
	for i, t := range targets {
		for _, port := range ports[i] {
			planned = append(planned, plannedForward{target: t, ip: ips[i], hostname: byIP[ips[i]], port: port})
		}
	}
	return planned, nil
}

func leaseError(path string, err error) error {
	e := clierr.New(clierr.Internal, err.Error())
	var ex *registry.ExhaustedError
	switch {
	case errors.Is(err, registry.ErrCorrupt) || errors.Is(err, registry.ErrUnsupportedVersion):
		e = e.WithHint(fmt.Sprintf("fix or move %s; services get new addresses when forwarded again", path))
	case errors.As(err, &ex):
		e = e.WithHint(fmt.Sprintf("forget unused services in %s", path))
	}
	return e
}

// serveForwards listens on every planned address, records the forwards
// in the state directory, and serves until the stop signal.
func (e *Env) serveForwards(contextName, companionURL string, client *forward.Client, planned []plannedForward) error {
	stop, cancel := e.NotifyContext(context.Background())
	defer cancel()

	events := &eventPrinter{env: e}
	var fwds []*forward.Forwarder
	closeAll := func() {
		for _, f := range fwds {
			_ = f.Close()
		}
	}
	for _, p := range planned {
		addr := netip.AddrPortFrom(p.ip, uint16(p.port)).String()
		f, err := client.Listen(forward.Config{
			Addr:    addr,
			Target:  p.target.ref.Target(p.port),
			OnEvent: events.handler(p),
			Listen:  e.Listen,
		})
		if err != nil {
			closeAll()
			return clierr.Newf(clierr.Internal, "forwarding %s: %v", p.target.name, err).
				WithHint(fmt.Sprintf("another process may already listen on %s; run 'doktunnel status' to see running forwards", addr))
		}
		fwds = append(fwds, f)
	}

	state := runstate.Process{
		PID:          os.Getpid(),
		StartedAt:    time.Now().UTC().Truncate(time.Second),
		Context:      contextName,
		CompanionURL: companionURL,
	}
	for _, p := range planned {
		state.Forwards = append(state.Forwards, runstate.Forward{
			Target:   runstate.Target{Type: p.target.typ, ID: p.target.id, Name: p.target.name},
			Hostname: p.hostname, IP: p.ip, Port: p.port,
		})
	}
	out := forwardJSON{Context: contextName, PID: state.PID, StartedAt: state.StartedAt, CompanionURL: companionURL, Forwards: state.Forwards}
	stateDir, err := e.forwardStateDir()
	if err == nil {
		_, err = runstate.Write(stateDir, state)
	}
	if err != nil {
		closeAll()
		return err
	}
	// Best effort: a file left behind belongs to a dead PID, which status
	// recognizes as stale.
	defer func() { _ = runstate.Remove(stateDir, state.PID) }()

	if err := writeForwards(e, out, planned); err != nil {
		closeAll()
		return err
	}

	var served sync.WaitGroup
	for _, f := range fwds {
		served.Add(1)
		go func() {
			defer served.Done()
			_ = f.Serve(stop)
		}()
	}
	<-stop.Done()
	done := make(chan struct{})
	go func() {
		served.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownTimeout):
		e.warn("some tunnels did not close in time")
	}
	return nil
}

// forwardStateDir is where forward processes record their forwards: next
// to the address registry.
func (e *Env) forwardStateDir() (string, error) {
	regPath, err := e.registryPath()
	if err != nil {
		return "", err
	}
	return runstate.Dir(filepath.Dir(regPath)), nil
}

func writeForwards(e *Env, out forwardJSON, planned []plannedForward) error {
	if e.JSON {
		return output.WriteJSON(e.Stdout, out)
	}
	var b strings.Builder
	for _, p := range planned {
		fmt.Fprintf(&b, "%s (%s) → %s\n", p.hostname, netip.AddrPortFrom(p.ip, uint16(p.port)), p.target.name)
	}
	if _, err := fmt.Fprint(e.Stdout, b.String()); err != nil {
		return err
	}
	_, err := fmt.Fprintln(e.Stderr, "Forwarding; press Ctrl+C to stop.")
	return err
}

// eventPrinter prints connection events on stderr in human mode, one line
// each; JSON mode prints none.
type eventPrinter struct {
	env *Env
	mu  sync.Mutex
}

func (p *eventPrinter) handler(f plannedForward) func(forward.Event) {
	if p.env.JSON {
		return nil
	}
	where := fmt.Sprintf("%s:%d", f.target.name, f.port)
	return func(ev forward.Event) {
		var line string
		switch ev.Kind {
		case forward.EventOpened:
			line = fmt.Sprintf("%s: connection %d opened", where, ev.ID)
		case forward.EventClosed:
			line = fmt.Sprintf("%s: connection %d closed (%s; %d bytes sent, %d received)", where, ev.ID, ev.Reason, ev.Sent, ev.Received)
			if ev.Err != nil {
				line += ": " + ev.Err.Error()
			}
		case forward.EventFailed:
			ce := forward.CLIError(ev.Err)
			line = fmt.Sprintf("%s: connection %d failed: %s [%s]", where, ev.ID, ce.Message, ce.Code)
		case forward.EventAcceptError:
			line = fmt.Sprintf("%s: %v", where, ev.Err)
		default:
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		fmt.Fprintln(p.env.Stderr, line)
	}
}
