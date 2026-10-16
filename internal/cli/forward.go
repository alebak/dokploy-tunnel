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

// hostsCleanupTimeout bounds how long forward spends removing its hostnames
// from the hosts file on exit, including the time a password prompt waits
// for an answer. It is a variable so tests can shorten it.
var hostsCleanupTimeout = time.Minute

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
	// localPort is the --local-port value; localPortSet tells
	// --local-port 0 from no --local-port.
	localPort    int
	localPortSet bool
}

func newForwardCommand() *Command {
	var opts forwardOptions
	return &Command{
		Name:    "forward",
		Summary: "Forward Dokploy services to stable local hostnames and ports",
		Description: "Forwards each service on its own loopback address, leased from 127.77.0.0/16, and its real port, " +
			"never on 0.0.0.0, and opens one tunnel through the context's companion for every local connection. " +
			"A port below 1024, which needs privileges to listen on, is listened on at port + 8000 (80 on 8080, 443 on 8443), " +
			"or the next number up that the service does not expose itself; the choice is stored in the service's lease and " +
			"reused, and a stored port the service now exposes itself is picked again with a warning. With --port below 1024 " +
			"it asks the companion which ports the service exposes; when the companion cannot answer it warns and picks " +
			"the port without them. --local-port <n> listens on n instead, for a single port, without storing it, and fails " +
			"with invalid_argument when n is the stored local port of another port of the service, a port the companion " +
			"reports the service exposes (it is not asked for --port from 1024 up or a database's default port), or the " +
			"port of a running forward on that address; an n below 1024 needs privileges. Ports from 1024 up keep their number. " +
			"Services are named as 'doktunnel services' lists them: by name, `<compose>/<service>` for a service " +
			"inside a compose stack, or ID; a compose stack itself is not forwardable. Without services or --all " +
			"it asks which to forward, or fails with missing_input when it cannot ask. " +
			"The port is --port, else a database's default port, else the port the companion reports for the " +
			"container; several ports need --port or --all-ports, and none fails with missing_input naming --port. " +
			"The hostnames are written to the hosts file as 'doktunnel hosts sync' does, elevating once only when " +
			"something changed (with --no-input: elevation_required and the command to run). If any address " +
			"cannot be listened on, nothing is forwarded. Runs in the foreground until Ctrl+C or SIGTERM, then " +
			"closes every tunnel, removes its hostnames from the hosts file unless another running forward uses " +
			"them (elevating again when needed; with --no-input, or when elevation fails, it leaves them and warns " +
			"to run 'doktunnel hosts clean'), and exits 0. Human output: one `<hostname> (<ip>:<port>) → <service>` line per " +
			"forward on stdout, `→ <service>:<service port>` when the local port differs, connection events on stderr. " +
			"JSON output, printed once every forward listens: " +
			"`{\"context\":\"<name>\",\"pid\":<pid>,\"started_at\":\"<RFC 3339>\",\"companion_url\":\"<URL>\",\"forwards\":[...]}`, " +
			"where each forward has `target` (`type`, `id`, `name`), `hostname`, `ip`, `port` (the local port) and " +
			"`service_port` (the container port); nothing else is " +
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
			fs.Func("local-port", "listen on this local `port` instead of the one the rule picks; only with a single port, and not stored", func(v string) error {
				n, err := strconv.Atoi(v)
				if err != nil {
					return errors.New("not a number")
				}
				opts.localPort, opts.localPortSet = n, true
				return nil
			})
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
// guesses. exposed are every port the service is known to expose: the
// ports to forward, or all the companion reports.
//
// A --port below registry.FirstUnprivilegedPort is listened on at another
// local port, which must skip the ports the service exposes itself, so
// resolvePorts asks the companion for them too. That is best effort: when
// the companion cannot answer, such as one older than its ports endpoint,
// exposed holds --port alone and unreported is why.
func resolvePorts(ctx context.Context, companion portLister, t forwardTarget, opts forwardOptions) (ports, exposed []int, unreported, err error) {
	if opts.portSet {
		ports = []int{opts.port}
		if opts.port >= registry.FirstUnprivilegedPort {
			return ports, ports, nil, nil
		}
		reported, err := companion.Ports(ctx, t.ref)
		if err != nil {
			return ports, ports, err, nil
		}
		exposed = []int{opts.port}
		for _, p := range reported {
			exposed = append(exposed, p.Port)
		}
		return ports, exposed, nil, nil
	}
	if t.ref.ComposeService == "" {
		if p := dokploy.DefaultPort(t.ref.ServiceType); p != dokploy.UnknownPort {
			return []int{p}, []int{p}, nil, nil
		}
	}
	reported, err := companion.Ports(ctx, t.ref)
	if err != nil {
		return nil, nil, nil, forward.CLIError(err)
	}
	ports = make([]int, len(reported))
	for i, p := range reported {
		ports[i] = p.Port
	}
	switch {
	case len(ports) == 0:
		return nil, nil, nil, clierr.Newf(clierr.MissingInput, "missing value for --port: the companion reports no exposed port for %s", t.name).
			WithHint("pass --port <n> with the port the service listens on")
	case len(ports) > 1 && !opts.allPorts:
		list := make([]string, len(ports))
		for i, p := range ports {
			list[i] = strconv.Itoa(p)
		}
		return nil, nil, nil, clierr.Newf(clierr.MissingInput, "missing value for --port: %s exposes several ports: %s", t.name, strings.Join(list, ", ")).
			WithHint("pass --port <n> to forward one of them, or --all-ports to forward each")
	}
	return ports, ports, nil, nil
}

// plannedForward is one listener to open.
type plannedForward struct {
	target   forwardTarget
	ip       netip.Addr
	hostname string
	// port is the service port the tunnel connects to; localPort is the
	// port the listener is bound to.
	port      int
	localPort int
}

// local is the address the listener is bound to.
func (p plannedForward) local() netip.AddrPort {
	return netip.AddrPortFrom(p.ip, uint16(p.localPort))
}

// targetLabel names the target in output: its name, followed by the
// service port when the listener is bound to another port.
func (p plannedForward) targetLabel() string {
	return targetLabel(p.target.name, p.port, p.localPort)
}

func targetLabel(name string, servicePort, localPort int) string {
	if servicePort == localPort {
		return name
	}
	return fmt.Sprintf("%s:%d", name, servicePort)
}

func validateForwardOptions(args []string, opts forwardOptions) error {
	switch {
	case opts.all && len(args) > 0:
		return clierr.New(clierr.InvalidArgument, "pass either service names or --all, not both")
	case opts.portSet && opts.allPorts:
		return clierr.New(clierr.InvalidArgument, "pass either --port or --all-ports, not both")
	case opts.portSet && (opts.port < 1 || opts.port > 65535):
		return clierr.Newf(clierr.InvalidArgument, "invalid --port %d: it must be a number from 1 to 65535", opts.port)
	case opts.localPortSet && (opts.localPort < 1 || opts.localPort > 65535):
		return clierr.Newf(clierr.InvalidArgument, "invalid --local-port %d: it must be a number from 1 to 65535", opts.localPort)
	case opts.localPortSet && opts.allPorts:
		return clierr.New(clierr.InvalidArgument, "pass either --local-port or --all-ports, not both: --local-port applies to a single port")
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
	if opts.localPortSet && len(selected) > 1 {
		return clierr.Newf(clierr.InvalidArgument, "--local-port applies to a single port of a single service, and %d services are selected", len(selected)).
			WithHint("forward the services one at a time to pick a local port for each")
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
	exposed := make([][]int, len(selected))
	unreported := make([]error, len(selected))
	for i, t := range selected {
		if ports[i], exposed[i], unreported[i], err = resolvePorts(ctx, client, t, opts); err != nil {
			return err
		}
	}
	// With --all-ports refused above, a single service resolves to one
	// port; this only guards that assumption.
	if opts.localPortSet && len(ports[0]) != 1 {
		return clierr.Newf(clierr.InvalidArgument, "--local-port applies to a single port, and %s resolves to %d", selected[0].name, len(ports[0]))
	}

	planned, err := env.leaseTargets(cat, selected, ports, exposed, unreported, opts)
	if err != nil {
		return err
	}
	return env.serveForwards(cat.context.Name, companionURL, client, planned)
}

// leaseTargets leases an address for every target, records what its
// hostname is built from, picks the local port of each of its ports, and
// plans one forward per target and port. unreported[i], when not nil, is
// why the companion did not say which ports targets[i] exposes; a port
// listened on at another local port then gets a warning, since the service
// may expose that local port itself.
func (e *Env) leaseTargets(cat *catalog, targets []forwardTarget, ports, exposed [][]int, unreported []error, opts forwardOptions) ([]plannedForward, error) {
	regPath, err := e.registryPath()
	if err != nil {
		return nil, err
	}
	reg, err := registry.Open(regPath)
	if err != nil {
		return nil, err
	}
	ips := make([]netip.Addr, len(targets))
	mappings := make([][]registry.PortMapping, len(targets))
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
		var fixed map[int]int
		if opts.localPortSet {
			fixed = map[int]int{ports[i][0]: opts.localPort}
			if err := e.checkLocalPortFree(ips[i], ports[i][0], opts.localPort); err != nil {
				return nil, err
			}
		}
		if mappings[i], err = reg.MapPorts(k, ports[i], exposed[i], fixed); err != nil {
			return nil, localPortError(regPath, t, err)
		}
		for _, m := range mappings[i] {
			if m.Replaced != 0 {
				e.warn(fmt.Sprintf("%s: port %d now listens on %d, not %d, because the service exposes %d itself",
					t.name, m.ServicePort, m.LocalPort, m.Replaced, m.Replaced))
			}
			if unreported[i] != nil && m.LocalPort != m.ServicePort {
				e.warn(fmt.Sprintf("%s: the companion did not report the ports the service exposes (%s); port %d listens on %d, which the service may also expose itself",
					t.name, forward.CLIError(unreported[i]).Message, m.ServicePort, m.LocalPort))
			}
		}
	}
	named, err := e.namedEntries()
	if err != nil {
		return nil, err
	}
	byIP := make(map[netip.Addr]string, len(named))
	for _, n := range named {
		byIP[n.IP] = n.Hostname
	}
	var planned []plannedForward
	for i, t := range targets {
		for _, m := range mappings[i] {
			planned = append(planned, plannedForward{target: t, ip: ips[i], hostname: byIP[ips[i]], port: m.ServicePort, localPort: m.LocalPort})
		}
	}
	return planned, nil
}

// checkLocalPortFree fails when a running forward already listens on ip
// and the --local-port value local, which another port of the same
// service would then collide with.
func (e *Env) checkLocalPortFree(ip netip.Addr, port, local int) error {
	running, err := e.activeForwards()
	if err != nil {
		return err
	}
	for _, f := range running.Forwards {
		if f.IP == ip && f.Port == local {
			return clierr.Newf(clierr.InvalidArgument, "invalid --local-port %d for port %d: %s is already forwarded to port %d of %s by doktunnel process %d",
				local, port, netip.AddrPortFrom(ip, uint16(local)), f.ServicePort, f.Target.Name, f.PID).
				WithHint("pick another --local-port, or stop that forward first")
		}
	}
	return nil
}

// localPortError explains a failure to pick the local ports of t.
func localPortError(path string, t forwardTarget, err error) error {
	var ce *registry.PortCollisionError
	switch {
	case errors.As(err, &ce):
		why := fmt.Sprintf("its port %d is already mapped to local port %d", ce.OtherPort, ce.LocalPort)
		if ce.OtherPort == ce.LocalPort {
			why = fmt.Sprintf("it exposes port %d itself", ce.OtherPort)
		}
		return clierr.Newf(clierr.InvalidArgument, "invalid --local-port %d for port %d of %s: %s", ce.LocalPort, ce.ServicePort, t.name, why).
			WithHint("pick another --local-port")
	case errors.Is(err, registry.ErrNoLocalPort):
		return clierr.Newf(clierr.InvalidArgument, "%s: %v", t.name, err).
			WithHint("pass --local-port <n> with a free local port")
	}
	return leaseError(path, err)
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

// serveForwards records the planned forwards in the state directory, syncs
// the hosts file and loopback aliases, listens on every planned address,
// and serves until the stop signal. On the way out it removes the
// hostnames no other running forward uses (see removeOwnHosts).
func (e *Env) serveForwards(contextName, companionURL string, client *forward.Client, planned []plannedForward) error {
	stop, cancel := e.NotifyContext(context.Background())
	defer cancel()

	state := runstate.Process{
		PID:          os.Getpid(),
		StartedAt:    time.Now().UTC().Truncate(time.Second),
		Context:      contextName,
		CompanionURL: companionURL,
	}
	for _, p := range planned {
		state.Forwards = append(state.Forwards, runstate.Forward{
			Target:   runstate.Target{Type: p.target.typ, ID: p.target.id, Name: p.target.name},
			Hostname: p.hostname, IP: p.ip, Port: p.localPort, ServicePort: p.port,
		})
	}
	// The state file comes first: the sync writes the hostnames of running
	// forwards only, this one included.
	stateDir, err := e.forwardStateDir()
	if err == nil {
		_, err = runstate.Write(stateDir, state)
	}
	if err != nil {
		return err
	}
	// Best effort: a file left behind belongs to a dead PID, which status
	// and the sync recognize as stale.
	defer func() { _ = runstate.Remove(stateDir, state.PID) }()

	ctx := context.Background()
	synced, err := e.syncHosts(ctx, syncOptions{})
	if err != nil {
		return err
	}
	if synced.Changed && !e.JSON {
		fmt.Fprintf(e.Stderr, "Updated %s: %d added, %d removed.\n", synced.HostsFile, len(synced.Added), len(synced.Removed))
	}
	// Runs before cancel, so a Ctrl+C at a password prompt on the way out
	// stops the prompt, not doktunnel.
	defer e.removeOwnHosts(stateDir, state.PID)

	// On macOS an address needs a lo0 alias before it can be listened on;
	// the sync adds them, unless it skipped them for an overridden file.
	ips := make([]netip.Addr, len(planned))
	for i, p := range planned {
		ips[i] = p.ip
	}
	missing, err := e.Loopback.Missing(ctx, ips)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return clierr.Newf(clierr.ElevationRequired, "lo0 has no alias for %s, so it cannot be listened on", missing[0]).
			WithHint(fmt.Sprintf("run: sudo /sbin/ifconfig lo0 alias %s up", missing[0]))
	}

	events := &eventPrinter{env: e}
	var fwds []*forward.Forwarder
	closeAll := func() {
		for _, f := range fwds {
			_ = f.Close()
		}
	}
	for _, p := range planned {
		addr := p.local().String()
		f, err := client.Listen(forward.Config{
			Addr:    addr,
			Target:  p.target.ref.Target(p.port),
			OnEvent: events.handler(p),
			Listen:  e.Listen,
		})
		if err != nil {
			closeAll()
			return clierr.Newf(clierr.Internal, "forwarding port %d of %s on %s: %v", p.port, p.target.name, addr, err).
				WithHint(bindHint(addr, p.localPort, err))
		}
		fwds = append(fwds, f)
	}

	out := forwardJSON{Context: contextName, PID: state.PID, StartedAt: state.StartedAt, CompanionURL: companionURL, Forwards: state.Forwards}
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

// bindHint suggests why listening on addr, whose port is localPort, failed
// with err. Only --local-port can pick a port below
// registry.FirstUnprivilegedPort, which most systems keep for privileged
// processes; Windows reports that refusal with an error that is not
// os.ErrPermission, so the hint names both causes when it cannot tell.
func bindHint(addr string, localPort int, err error) string {
	busy := fmt.Sprintf("another process may already listen on %s; run 'doktunnel status' to see running forwards", addr)
	if localPort >= registry.FirstUnprivilegedPort {
		return busy
	}
	privileged := fmt.Sprintf("listening on a port below %d needs privileges; pass --local-port with a port from %d up",
		registry.FirstUnprivilegedPort, registry.FirstUnprivilegedPort)
	if errors.Is(err, os.ErrPermission) {
		return privileged
	}
	return privileged + "; otherwise " + busy
}

// removeOwnHosts removes this process's hostnames from the hosts file as it
// stops: it removes its state file, then syncs the section to the forwards
// still running, so a name another forward uses stays. It elevates as the
// start did, but never fails or blocks the exit: when the change needs
// administrator privileges and prompting is not allowed, the elevation
// fails, or it all takes longer than hostsCleanupTimeout, the entries stay
// and a warning says how to remove them. lo0 aliases always stay, and so do
// the leases, so every service keeps its address.
func (e *Env) removeOwnHosts(stateDir string, pid int) {
	if err := runstate.Remove(stateDir, pid); err != nil {
		e.warn(err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), hostsCleanupTimeout)
	defer cancel()
	type result struct {
		out hostsSyncJSON
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := e.syncHosts(ctx, syncOptions{exiting: true})
		done <- result{out, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-ctx.Done():
		// The elevators stop their prompt when ctx ends, so the sync
		// returns at once; the grace only covers one that does not.
		select {
		case r = <-done:
		case <-time.After(time.Second):
			r.err = fmt.Errorf("gave up after %v", hostsCleanupTimeout)
		}
	}
	if r.err != nil {
		e.warn(fmt.Sprintf("the hostnames of this forward stay in the hosts file (%s); run 'doktunnel hosts sync' to "+
			"remove them, or 'doktunnel hosts clean' to remove the whole doktunnel section", clierr.From(r.err).Message))
		return
	}
	if r.out.Changed && !e.JSON {
		fmt.Fprintf(e.Stderr, "Updated %s: %d added, %d removed.\n", r.out.HostsFile, len(r.out.Added), len(r.out.Removed))
	}
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
		fmt.Fprintf(&b, "%s (%s) → %s\n", p.hostname, p.local(), p.targetLabel())
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
