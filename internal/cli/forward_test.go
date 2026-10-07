package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
	"github.com/alebak/dokploy-tunnel/internal/dokploy"
	"github.com/alebak/dokploy-tunnel/internal/hostname"
	"github.com/alebak/dokploy-tunnel/internal/hosts"
	"github.com/alebak/dokploy-tunnel/internal/registry"
	"github.com/alebak/dokploy-tunnel/internal/runstate"
	"github.com/alebak/dokploy-tunnel/internal/tunnel"
)

// forwardWait bounds every wait in the forward tests.
const forwardWait = 10 * time.Second

// fakeTunnelCompanion is a loopback companion: its ports endpoint answers
// from ports, keyed by serviceId, and its tunnel endpoint echoes.
type fakeTunnelCompanion struct {
	srv *httptest.Server
	// ports answers the ports endpoint by serviceId; a missing entry is
	// a not_found error.
	ports map[string][]int
	// portsErr, when set, answers every ports request with this status
	// and JSON body.
	portsErr *rejection

	mu         sync.Mutex
	portsCalls []string
	tunnels    []string
	// closes receives the close status of every tunnel the client ended.
	closes chan websocket.StatusCode
}

type rejection struct {
	status int
	body   string
}

func newFakeTunnelCompanion(t *testing.T) *fakeTunnelCompanion {
	t.Helper()
	fc := &fakeTunnelCompanion{ports: map[string][]int{}, closes: make(chan websocket.StatusCode, 64)}
	mux := http.NewServeMux()
	mux.HandleFunc("/doktunnel"+tunnel.PortsPath, fc.handlePorts)
	mux.HandleFunc("/doktunnel"+tunnel.Path, fc.handleTunnel)
	fc.srv = httptest.NewServer(mux)
	t.Cleanup(fc.srv.Close)
	return fc
}

func (fc *fakeTunnelCompanion) url() string { return fc.srv.URL + "/doktunnel" }

func (fc *fakeTunnelCompanion) handlePorts(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get(tunnel.ParamServiceID)
	fc.mu.Lock()
	fc.portsCalls = append(fc.portsCalls, id)
	ports, ok := fc.ports[id]
	rej := fc.portsErr
	fc.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Header.Get(tunnel.HeaderAPIKey) != "key-prod":
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":"unauthenticated","message":"bad key"}`)
	case rej != nil:
		w.WriteHeader(rej.status)
		_, _ = io.WriteString(w, rej.body)
	case !ok:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"code":"not_found","message":"no such service"}`)
	default:
		resp := tunnel.PortsResponse{Ports: []tunnel.Port{}}
		for _, p := range ports {
			resp.Ports = append(resp.Ports, tunnel.Port{Port: p, Protocol: tunnel.ProtocolTCP})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func (fc *fakeTunnelCompanion) handleTunnel(w http.ResponseWriter, r *http.Request) {
	target, err := tunnel.ParseTarget(r.URL.Query())
	if err != nil || r.Header.Get(tunnel.HeaderAPIKey) != "key-prod" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	fc.mu.Lock()
	fc.tunnels = append(fc.tunnels, target.String())
	fc.mu.Unlock()
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ctx := context.Background()
	for {
		typ, msg, err := c.Read(ctx)
		if err != nil {
			fc.closes <- websocket.CloseStatus(err)
			return
		}
		if err := c.Write(ctx, typ, msg); err != nil {
			return
		}
	}
}

// syncBuffer is a bytes.Buffer safe for a command writing while the test
// reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// forwardHarness wires "forward" to the services fixture, a fake companion,
// a fixture hosts file, a temp registry, a fake elevator and loopback, and
// a listen seam. Listeners are opened on 127.0.0.1 with a free port, since
// the leased addresses may not exist (macOS) and the real ports may be
// taken; the address forward asked for is recorded.
type forwardHarness struct {
	*contextHarness
	hosts     *hostsHarness
	companion *fakeTunnelCompanion
	// stop is cancelled to deliver the shutdown signal; it starts
	// cancelled, so a successful forward shuts down as soon as it listens.
	stop       context.Context
	stopCancel context.CancelFunc

	mu sync.Mutex
	// listens maps each requested address to the listener opened for it.
	listens map[string]net.Listener
	order   []string
	// busy are requested addresses whose listen fails, as if taken.
	busy map[string]bool
	// writes are the hosts file contents written without elevation.
	writes []string
}

func newForwardHarness(t *testing.T) *forwardHarness {
	t.Helper()
	h := &forwardHarness{
		contextHarness: newServicesHarness(t),
		hosts:          newHostsHarness(t),
		companion:      newFakeTunnelCompanion(t),
		listens:        map[string]net.Listener{},
	}
	h.mustRun("", "context", "set-companion", "prod", h.companion.url(), "--json")
	h.companion.ports = map[string][]int{
		"cmp_myapp/postgres": {5432},
		"cmp_myapp/pgadmin":  {80, 443},
		"app_web":            {},
	}
	h.stop, h.stopCancel = context.WithCancel(context.Background())
	h.stopCancel()
	return h
}

// keepRunning makes the next forward run until stopCancel is called.
func (h *forwardHarness) keepRunning() {
	h.stop, h.stopCancel = context.WithCancel(context.Background())
	h.t.Cleanup(h.stopCancel)
}

func (h *forwardHarness) listen(network, addr string) (net.Listener, error) {
	if h.busy[addr] {
		return nil, fmt.Errorf("listen %s %s: address already in use", network, addr)
	}
	ln, err := net.Listen(network, "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.listens[addr] = ln
	h.order = append(h.order, addr)
	return ln, nil
}

func (h *forwardHarness) requested() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.order)
}

func (h *forwardHarness) app(stdin string, terminal bool, stdout, stderr io.Writer) *App {
	return &App{
		Root:            NewRoot(),
		Stdin:           strings.NewReader(stdin),
		Stdout:          stdout,
		Stderr:          stderr,
		StdinIsTerminal: terminal,
		ConfigPath:      h.configPath,
		Keyring:         h.keyring,
		NewAPI: func(base *url.URL, apiKey string) dokploy.API {
			return h.api
		},
		Getenv:         func(k string) string { return h.env[k] },
		ProbeCompanion: h.probe,
		HostsPath:      h.hosts.hostsPath,
		RegistryPath:   h.hosts.registryPath,
		Elevator:       h.hosts.elevator,
		Loopback:       h.hosts.loopback,
		Executable:     func() (string, error) { return fakeExe, nil },
		WriteHosts: func(path string, data []byte) error {
			if h.hosts.denyWrite {
				return &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission}
			}
			h.mu.Lock()
			h.writes = append(h.writes, string(data))
			h.mu.Unlock()
			return hosts.Write(path, data)
		},
		ProcessAlive: func(pid int) bool { return pid == os.Getpid() || h.hosts.alive[pid] },
		NotifyContext: func(parent context.Context) (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(parent)
			stop := context.AfterFunc(h.stop, cancel)
			return ctx, func() { stop(); cancel() }
		},
		Listen: h.listen,
	}
}

func (h *forwardHarness) forward(stdin string, terminal bool, args ...string) result {
	h.t.Helper()
	var stdout, stderr bytes.Buffer
	exit := h.app(stdin, terminal, &stdout, &stderr).Run(append([]string{"forward"}, args...))
	return result{exit: exit, stdout: stdout.String(), stderr: stderr.String()}
}

func (h *forwardHarness) stateDir() string {
	return runstate.Dir(filepath.Dir(h.hosts.registryPath))
}

// forwardOut mirrors the documented JSON output of "forward".
type forwardOut struct {
	Context      string    `json:"context"`
	PID          int       `json:"pid"`
	StartedAt    time.Time `json:"started_at"`
	CompanionURL string    `json:"companion_url"`
	Forwards     []struct {
		Target struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"target"`
		Hostname string `json:"hostname"`
		IP       string `json:"ip"`
		Port     int    `json:"port"`
	} `json:"forwards"`
}

// summary renders the forwards as "<id>@<ip>:<port>=<hostname>" strings.
func (o forwardOut) summary() []string {
	var s []string
	for _, f := range o.Forwards {
		s = append(s, fmt.Sprintf("%s@%s:%d=%s", f.Target.ID, f.IP, f.Port, f.Hostname))
	}
	return s
}

func TestForward_SelectsTargetsAndResolvesPorts(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
		// portsCalls are the services the companion was asked about.
		portsCalls []string
	}{
		{
			name: "database by name uses its default port",
			args: []string{"main-db"},
			want: []string{"pg_main@127.77.0.1:5432=shop-maindb-a1b2c3.internal"},
		},
		{
			name: "database by ID",
			args: []string{"pg_main"},
			want: []string{"pg_main@127.77.0.1:5432=shop-maindb-a1b2c3.internal"},
		},
		{
			name:       "compose service by name asks the companion",
			args:       []string{"myapp/postgres"},
			want:       []string{"cmp_myapp/postgres@127.77.0.1:5432=postgres.shop-myapp-x1y2z3.internal"},
			portsCalls: []string{"cmp_myapp/postgres"},
		},
		{
			name:       "compose service by ID",
			args:       []string{"cmp_myapp/postgres"},
			want:       []string{"cmp_myapp/postgres@127.77.0.1:5432=postgres.shop-myapp-x1y2z3.internal"},
			portsCalls: []string{"cmp_myapp/postgres"},
		},
		{
			name: "several services, a repeated one once",
			args: []string{"main-db", "cache", "pg_main"},
			want: []string{
				"pg_main@127.77.0.1:5432=shop-maindb-a1b2c3.internal",
				"redis_cache@127.77.0.2:6379=shop-cache-g7h8i9.internal",
			},
		},
		{
			name:       "--all-ports forwards every exposed port",
			args:       []string{"myapp/pgadmin", "--all-ports"},
			want:       []string{"cmp_myapp/pgadmin@127.77.0.1:80=pgadmin.shop-myapp-x1y2z3.internal", "cmp_myapp/pgadmin@127.77.0.1:443=pgadmin.shop-myapp-x1y2z3.internal"},
			portsCalls: []string{"cmp_myapp/pgadmin"},
		},
		{
			name: "--port wins over the companion",
			args: []string{"myapp/pgadmin", "--port", "8080"},
			want: []string{"cmp_myapp/pgadmin@127.77.0.1:8080=pgadmin.shop-myapp-x1y2z3.internal"},
		},
		{
			name: "--port wins over a database default",
			args: []string{"--port", "6543", "main-db"},
			want: []string{"pg_main@127.77.0.1:6543=shop-maindb-a1b2c3.internal"},
		},
		{
			name: "a service whose name is unknown is named by its ID",
			args: []string{"libsql_edge"},
			want: []string{"libsql_edge@127.77.0.1:8080=libsql-edge.internal"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newForwardHarness(t)
			r := h.forward("", false, append(tt.args, "--json")...)
			if r.exit != 0 {
				t.Fatalf("exit = %d (stdout %q, stderr %q)", r.exit, r.stdout, r.stderr)
			}
			if r.stderr != "" {
				t.Errorf("stderr = %q, want nothing in JSON mode", r.stderr)
			}
			out := decodeJSON[forwardOut](t, r.stdout)
			if got := out.summary(); !slices.Equal(got, tt.want) {
				t.Errorf("forwards = %q, want %q", got, tt.want)
			}
			if out.Context != "prod" || out.PID != os.Getpid() || out.CompanionURL != h.companion.url() || out.StartedAt.IsZero() {
				t.Errorf("output = %+v, want context prod, this PID, the companion URL and a start time", out)
			}
			if got := h.companion.portsCalls; !slices.Equal(got, tt.portsCalls) {
				t.Errorf("ports requests = %q, want %q", got, tt.portsCalls)
			}
			// Every listener is on exactly the leased address and port.
			var want []string
			for _, f := range out.Forwards {
				want = append(want, net.JoinHostPort(f.IP, fmt.Sprint(f.Port)))
			}
			if got := h.requested(); !slices.Equal(got, want) {
				t.Errorf("listened on %q, want %q", got, want)
			}
		})
	}
}

func TestForward_AllForwardsEveryTargetInScope(t *testing.T) {
	h := newForwardHarness(t)
	// app_web exposes nothing by default; give it a port so the whole
	// production environment resolves, and keep only that environment.
	h.companion.ports["app_web"] = []int{3000}
	h.api.projects[0].Environments = h.api.projects[0].Environments[:1]

	r := h.forward("", false, "--all", "--project", "shop", "--json")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stdout %q, stderr %q)", r.exit, r.stdout, r.stderr)
	}
	got := decodeJSON[forwardOut](t, r.stdout).summary()
	// The compose stack itself is not forwardable, and cmp_stack has no
	// readable services.
	want := []string{
		"app_web@127.77.0.1:3000=shop-web-d4e5f6.internal",
		"pg_main@127.77.0.2:5432=shop-maindb-a1b2c3.internal",
		"redis_cache@127.77.0.3:6379=shop-cache-g7h8i9.internal",
	}
	if !slices.Equal(got, want) {
		t.Errorf("forwards = %q, want %q", got, want)
	}
}

func TestForward_Errors(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		setup     func(h *forwardHarness)
		wantCode  clierr.Code
		wantInMsg []string
	}{
		{name: "unknown service", args: []string{"nope"}, wantCode: clierr.NotFound, wantInMsg: []string{"nope", "doktunnel services"}},
		{
			name: "ambiguous name lists the candidates",
			args: []string{"main-db"},
			setup: func(h *forwardHarness) {
				stg := &h.api.projects[0].Environments[1]
				stg.Services = append(stg.Services, dokploy.Service{ID: "pg_stg", Type: dokploy.ServicePostgres, Name: "main-db", Status: "done"})
			},
			wantCode:  clierr.InvalidArgument,
			wantInMsg: []string{"pg_main", "pg_stg", "ID"},
		},
		{name: "a compose stack is not forwardable", args: []string{"myapp"}, wantCode: clierr.InvalidArgument, wantInMsg: []string{"myapp/postgres", "myapp/pgadmin"}},
		{name: "--all with names", args: []string{"--all", "main-db"}, wantCode: clierr.InvalidArgument, wantInMsg: []string{"--all"}},
		{name: "--port with --all-ports", args: []string{"main-db", "--port", "5432", "--all-ports"}, wantCode: clierr.InvalidArgument, wantInMsg: []string{"--all-ports"}},
		{name: "port zero", args: []string{"main-db", "--port", "0"}, wantCode: clierr.InvalidArgument, wantInMsg: []string{"--port"}},
		{name: "port too large", args: []string{"main-db", "--port", "65536"}, wantCode: clierr.InvalidArgument, wantInMsg: []string{"--port"}},
		{name: "--port with several targets", args: []string{"main-db", "cache", "--port", "6000"}, wantCode: clierr.InvalidArgument, wantInMsg: []string{"--port"}},
		{name: "several exposed ports", args: []string{"myapp/pgadmin"}, wantCode: clierr.MissingInput, wantInMsg: []string{"--port", "--all-ports", "80", "443"}},
		{name: "no exposed ports", args: []string{"web"}, wantCode: clierr.MissingInput, wantInMsg: []string{"--port"}},
		{name: "no arguments without a terminal", args: nil, wantCode: clierr.MissingInput, wantInMsg: []string{"<service>", "--all"}},
		{name: "unknown project", args: []string{"--all", "--project", "nope"}, wantCode: clierr.NotFound, wantInMsg: []string{"nope"}},
		{
			name: "companion refuses the ports request",
			args: []string{"myapp/postgres"},
			setup: func(h *forwardHarness) {
				h.companion.portsErr = &rejection{http.StatusForbidden, `{"code":"permission_denied","message":"no access"}`}
			},
			wantCode:  clierr.PermissionDenied,
			wantInMsg: []string{"no access"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newForwardHarness(t)
			if tt.setup != nil {
				tt.setup(h)
			}
			r := h.forward("", false, append(tt.args, "--json")...)
			if want := tt.wantCode.ExitCode(); r.exit != want {
				t.Fatalf("exit = %d, want %d (stdout %q)", r.exit, want, r.stdout)
			}
			e := decodeError(t, r.stdout)
			if e.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", e.Code, tt.wantCode)
			}
			for _, s := range tt.wantInMsg {
				if !strings.Contains(e.Message+" "+e.Hint, s) {
					t.Errorf("error %+v does not mention %q", e, s)
				}
			}
			if got := h.requested(); len(got) != 0 {
				t.Errorf("listened on %q, want nothing after an error", got)
			}
			if _, err := os.Stat(h.hosts.registryPath); err == nil {
				t.Error("the registry was written, want no lease after a selection or port error")
			}
		})
	}
}

func TestForward_InteractivePicker(t *testing.T) {
	h := newForwardHarness(t)
	// The options follow the services list: web, main-db, cache,
	// legacy-db, myapp/postgres, myapp/pgadmin, libsql_edge.
	r := h.forward("2 5\n", true, "--json")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stdout %q, stderr %q)", r.exit, r.stdout, r.stderr)
	}
	for _, want := range []string{"1) web", "2) main-db", "5) myapp/postgres", "7) libsql_edge"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("picker %q does not offer %q", r.stderr, want)
		}
	}
	got := decodeJSON[forwardOut](t, r.stdout).summary()
	want := []string{
		"pg_main@127.77.0.1:5432=shop-maindb-a1b2c3.internal",
		"cmp_myapp/postgres@127.77.0.2:5432=postgres.shop-myapp-x1y2z3.internal",
	}
	if !slices.Equal(got, want) {
		t.Errorf("forwards = %q, want %q", got, want)
	}
}

func TestForward_SyncsHostsFile(t *testing.T) {
	const block = hosts.BeginLine + "\r\n" +
		"127.77.0.1\tshop-maindb-a1b2c3.internal\r\n" +
		hosts.EndLine + "\r\n"

	t.Run("writes the entry, names the lease and removes the entry on exit", func(t *testing.T) {
		h := newForwardHarness(t)
		if r := h.forward("", false, "main-db", "--json"); r.exit != 0 {
			t.Fatalf("exit = %d (stdout %q)", r.exit, r.stdout)
		}
		if want := []string{fixtureHosts + block, fixtureHosts}; !slices.Equal(h.writes, want) {
			t.Errorf("hosts file writes =\n%q\nwant\n%q", h.writes, want)
		}
		if len(h.hosts.elevator.calls) != 0 {
			t.Errorf("elevated %d times, want none for a writable hosts file", len(h.hosts.elevator.calls))
		}
		reg, err := registry.Open(h.hosts.registryPath)
		if err != nil {
			t.Fatal(err)
		}
		leases, err := reg.List()
		if err != nil || len(leases) != 1 {
			t.Fatalf("leases = %+v, %v; want one", leases, err)
		}
		k := leases[0].Key
		if k.Instance != "https://panel.example.com" || k.OrganizationID != "org1" || k.ServiceID != "pg_main" {
			t.Errorf("lease key = %+v, want the panel, org1 and pg_main", k)
		}
		if n := leases[0].Names; n != (hostname.Names{Context: "prod", AppName: "shop-maindb-a1b2c3"}) {
			t.Errorf("lease names = %+v", n)
		}
	})

	t.Run("elevates once when the file is not writable", func(t *testing.T) {
		h := newForwardHarness(t)
		h.hosts.denyWrite = true
		r := h.forward("", true, "main-db")
		if r.exit != 0 {
			t.Fatalf("exit = %d (stderr %q)", r.exit, r.stderr)
		}
		// Once to add the entry, once on exit to remove it.
		if len(h.hosts.elevator.calls) != 2 {
			t.Fatalf("elevated %d times, want twice", len(h.hosts.elevator.calls))
		}
		if want := []string{fixtureEntries1, ""}; !slices.Equal(h.hosts.elevator.stdins, want) {
			t.Errorf("helper stdins = %q, want %q", h.hosts.elevator.stdins, want)
		}
		if got := h.hosts.readHosts(); got != fixtureHosts {
			t.Errorf("hosts file =\n%q\nwant\n%q", got, fixtureHosts)
		}
		if !strings.Contains(r.stderr, "needs administrator privileges") {
			t.Errorf("stderr %q does not explain the elevation", r.stderr)
		}
	})

	t.Run("no input fails with the exact command", func(t *testing.T) {
		h := newForwardHarness(t)
		h.hosts.denyWrite = true
		r := h.forward("", false, "main-db", "--json")
		if want := clierr.ElevationRequired.ExitCode(); r.exit != want {
			t.Fatalf("exit = %d, want %d (stdout %q)", r.exit, want, r.stdout)
		}
		e := decodeError(t, r.stdout)
		if e.Code != clierr.ElevationRequired || !strings.Contains(e.Hint, "fake-sudo "+fakeExe+" hosts privileged-apply") {
			t.Errorf("error = %+v, want elevation_required with the command", e)
		}
		if len(h.hosts.elevator.calls) != 0 || len(h.requested()) != 0 {
			t.Errorf("elevated %d times and listened on %q, want neither", len(h.hosts.elevator.calls), h.requested())
		}
		if entries, _ := runstate.List(h.stateDir()); len(entries) != 0 {
			t.Errorf("state files = %+v, want none", entries)
		}
	})

	t.Run("adds missing lo0 aliases through the helper", func(t *testing.T) {
		h := newForwardHarness(t)
		ip := netip.MustParseAddr("127.77.0.1")
		h.hosts.loopback.missing[ip] = true
		r := h.forward("", true, "main-db")
		if r.exit != 0 {
			t.Fatalf("exit = %d (stderr %q)", r.exit, r.stderr)
		}
		if len(h.hosts.elevator.calls) != 1 || !slices.Equal(h.hosts.loopback.added, []netip.Addr{ip}) {
			t.Errorf("elevated %d times and added aliases %v, want once and %v", len(h.hosts.elevator.calls), h.hosts.loopback.added, ip)
		}
	})
}

// fixtureEntries1 is what the privileged helper receives to add main-db.
const fixtureEntries1 = "127.77.0.1\tshop-maindb-a1b2c3.internal\n"

func TestForward_ExitKeepsNamesOfOtherRunningForwards(t *testing.T) {
	h := newForwardHarness(t)
	// Another forward process runs cache, and once ran main-db too.
	cache := h.hosts.lease("redis_cache", hostname.Names{Context: "prod", AppName: "shop-cache-g7h8i9"})
	main := h.hosts.lease("pg_main", hostname.Names{Context: "prod", AppName: "shop-maindb-a1b2c3"})
	h.hosts.liveForward(77, cache)
	h.hosts.writeHosts(fixtureHosts + hosts.BeginLine + "\r\n127.77.0.1\tshop-cache-g7h8i9.internal\r\n" + hosts.EndLine + "\r\n")

	if r := h.forward("", false, "main-db", "cache", "--json"); r.exit != 0 {
		t.Fatalf("exit = %d (stdout %q)", r.exit, r.stdout)
	}
	if main != netip.MustParseAddr("127.77.0.2") {
		t.Fatalf("main-db leased %v", main)
	}
	both := fixtureHosts + hosts.BeginLine + "\r\n127.77.0.1\tshop-cache-g7h8i9.internal\r\n127.77.0.2\tshop-maindb-a1b2c3.internal\r\n" + hosts.EndLine + "\r\n"
	onlyCache := fixtureHosts + hosts.BeginLine + "\r\n127.77.0.1\tshop-cache-g7h8i9.internal\r\n" + hosts.EndLine + "\r\n"
	if want := []string{both, onlyCache}; !slices.Equal(h.writes, want) {
		t.Errorf("hosts file writes =\n%q\nwant\n%q", h.writes, want)
	}
	if _, err := os.Stat(runstate.Path(h.stateDir(), os.Getpid())); !os.IsNotExist(err) {
		t.Errorf("state file still exists after exit: %v", err)
	}
}

func TestForward_ExitWithoutElevationLeavesEntries(t *testing.T) {
	block := fixtureHosts + hosts.BeginLine + "\r\n127.77.0.1\tshop-maindb-a1b2c3.internal\r\n" + hosts.EndLine + "\r\n"
	tests := []struct {
		name     string
		terminal bool
		args     []string
		setup    func(h *forwardHarness)
		calls    int
	}{
		{name: "--no-input", terminal: true, args: []string{"--no-input"}},
		{name: "no terminal", terminal: false},
		{name: "elevation refused", terminal: true, setup: func(h *forwardHarness) { h.hosts.elevator.fail = true }, calls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newForwardHarness(t)
			// The section already holds main-db, so starting needs no
			// privileges, but removing it on exit does.
			h.hosts.lease("pg_main", hostname.Names{Context: "prod", AppName: "shop-maindb-a1b2c3"})
			h.hosts.writeHosts(block)
			h.hosts.denyWrite = true
			if tt.setup != nil {
				tt.setup(h)
			}
			r := h.forward("", tt.terminal, append([]string{"main-db"}, tt.args...)...)
			if r.exit != 0 {
				t.Fatalf("exit = %d, want 0 (stdout %q, stderr %q)", r.exit, r.stdout, r.stderr)
			}
			if got := h.hosts.readHosts(); got != block {
				t.Errorf("hosts file =\n%q\nwant the entry left in place", got)
			}
			if len(h.hosts.elevator.calls) != tt.calls {
				t.Errorf("elevated %d times, want %d", len(h.hosts.elevator.calls), tt.calls)
			}
			if !strings.Contains(r.stderr, "warning: ") || !strings.Contains(r.stderr, "doktunnel hosts clean") {
				t.Errorf("stderr = %q, want a warning naming doktunnel hosts clean", r.stderr)
			}
			if _, err := os.Stat(h.hosts.pendingPath()); !os.IsNotExist(err) {
				t.Errorf("exit left a pending entries file: %v", err)
			}
		})
	}
}

func TestForward_ExitCleanupIsBounded(t *testing.T) {
	defer func(d time.Duration) { hostsCleanupTimeout = d }(hostsCleanupTimeout)
	hostsCleanupTimeout = 50 * time.Millisecond

	h := newForwardHarness(t)
	h.hosts.lease("pg_main", hostname.Names{Context: "prod", AppName: "shop-maindb-a1b2c3"})
	block := fixtureHosts + hosts.BeginLine + "\r\n127.77.0.1\tshop-maindb-a1b2c3.internal\r\n" + hosts.EndLine + "\r\n"
	h.hosts.writeHosts(block)
	h.hosts.denyWrite = true
	// Nobody answers the password prompt on exit.
	h.hosts.elevator.block = true

	start := time.Now()
	r := h.forward("", true, "main-db")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stderr %q)", r.exit, r.stderr)
	}
	if took := time.Since(start); took > forwardWait/2 {
		t.Errorf("forward took %v to exit, want it bounded by the cleanup timeout", took)
	}
	if got := h.hosts.readHosts(); got != block {
		t.Errorf("hosts file =\n%q\nwant the entry left in place", got)
	}
	if !strings.Contains(r.stderr, "doktunnel hosts clean") {
		t.Errorf("stderr = %q, want a warning naming doktunnel hosts clean", r.stderr)
	}
}

func TestForward_HumanOutput(t *testing.T) {
	h := newForwardHarness(t)
	r := h.forward("", false, "myapp/postgres", "main-db")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stderr %q)", r.exit, r.stderr)
	}
	want := "postgres.shop-myapp-x1y2z3.internal (127.77.0.1:5432) → myapp/postgres\n" +
		"shop-maindb-a1b2c3.internal (127.77.0.2:5432) → main-db\n"
	if r.stdout != want {
		t.Errorf("stdout =\n%s\nwant\n%s", r.stdout, want)
	}
	if !strings.Contains(r.stderr, "Ctrl+C") {
		t.Errorf("stderr %q does not tell how to stop", r.stderr)
	}
}

func TestForward_EndToEnd(t *testing.T) {
	h := newForwardHarness(t)
	h.keepRunning()
	var stdout, stderr syncBuffer
	exited := make(chan int, 1)
	go func() {
		exited <- h.app("", false, &stdout, &stderr).Run([]string{"forward", "myapp/postgres", "--json"})
	}()

	// The JSON result is printed once every forward listens.
	deadline := time.Now().Add(forwardWait)
	for !strings.HasSuffix(stdout.String(), "\n") {
		select {
		case code := <-exited:
			t.Fatalf("forward exited with %d before listening (stdout %q, stderr %q)", code, stdout.String(), stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("forward did not print its result")
		}
		time.Sleep(5 * time.Millisecond)
	}
	out := decodeJSON[forwardOut](t, stdout.String())
	if got := out.summary(); len(got) != 1 || got[0] != "cmp_myapp/postgres@127.77.0.1:5432=postgres.shop-myapp-x1y2z3.internal" {
		t.Fatalf("forwards = %q", got)
	}

	// The state file describes the running forward.
	state, err := runstate.Read(runstate.Path(h.stateDir(), os.Getpid()))
	if err != nil {
		t.Fatalf("reading the state file: %v", err)
	}
	if state.Context != "prod" || state.CompanionURL != h.companion.url() || len(state.Forwards) != 1 ||
		state.Forwards[0].Hostname != "postgres.shop-myapp-x1y2z3.internal" || state.Forwards[0].Port != 5432 ||
		state.Forwards[0].IP != netip.MustParseAddr("127.77.0.1") ||
		state.Forwards[0].Target != (runstate.Target{Type: "compose_service", ID: "cmp_myapp/postgres", Name: "myapp/postgres"}) {
		t.Errorf("state = %+v", state)
	}

	// While it runs, the hosts file holds its hostname.
	running := fixtureHosts + hosts.BeginLine + "\r\n127.77.0.1\tpostgres.shop-myapp-x1y2z3.internal\r\n" + hosts.EndLine + "\r\n"
	if got := h.hosts.readHosts(); got != running {
		t.Errorf("hosts file while forwarding =\n%q\nwant\n%q", got, running)
	}

	h.mu.Lock()
	ln := h.listens["127.77.0.1:5432"]
	h.mu.Unlock()
	echo := func() net.Conn {
		t.Helper()
		conn, err := net.DialTimeout("tcp", ln.Addr().String(), forwardWait)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(forwardWait))
		if _, err := conn.Write([]byte("SELECT 1")); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 8)
		if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "SELECT 1" {
			t.Fatalf("echo = %q, %v", buf, err)
		}
		return conn
	}
	waitClose := func(want websocket.StatusCode) {
		t.Helper()
		select {
		case got := <-h.companion.closes:
			if got != want {
				t.Errorf("tunnel closed with %d, want %d", got, want)
			}
		case <-time.After(forwardWait):
			t.Fatalf("the tunnel was not closed (want %d)", want)
		}
	}

	// A client that hangs up ends its tunnel normally.
	echo().Close()
	waitClose(websocket.StatusNormalClosure)

	// The shutdown signal closes open tunnels as going away, removes the
	// state file and exits 0.
	open := echo()
	defer open.Close()
	h.stopCancel()
	waitClose(websocket.StatusGoingAway)
	select {
	case code := <-exited:
		if code != 0 {
			t.Errorf("exit = %d, want 0 (stdout %q)", code, stdout.String())
		}
	case <-time.After(forwardWait):
		t.Fatal("forward did not exit after the signal")
	}
	if _, err := os.Stat(runstate.Path(h.stateDir(), os.Getpid())); !os.IsNotExist(err) {
		t.Errorf("state file still exists after shutdown: %v", err)
	}
	if got := h.hosts.readHosts(); got != fixtureHosts {
		t.Errorf("hosts file after shutdown =\n%q\nwant its hostname removed", got)
	}
	if strings.Count(stdout.String(), "\n") != 1 {
		t.Errorf("stdout = %q, want only the JSON result", stdout.String())
	}
	if _, err := ln.Accept(); err == nil {
		t.Error("the listener still accepts after shutdown")
	}
}

func TestForward_BindFailureForwardsNothing(t *testing.T) {
	h := newForwardHarness(t)
	h.busy = map[string]bool{"127.77.0.2:6379": true}

	r := h.forward("", false, "main-db", "cache", "--json")
	if want := clierr.Internal.ExitCode(); r.exit != want {
		t.Fatalf("exit = %d, want %d (stdout %q)", r.exit, want, r.stdout)
	}
	e := decodeError(t, r.stdout)
	if !strings.Contains(e.Message, "already in use") || !strings.Contains(e.Hint, "doktunnel status") {
		t.Errorf("error = %+v, want the bind failure and a hint naming doktunnel status", e)
	}
	h.mu.Lock()
	first := h.listens["127.77.0.1:5432"]
	h.mu.Unlock()
	if first == nil {
		t.Fatal("main-db was not listened on before cache failed")
	}
	if _, err := first.Accept(); err == nil {
		t.Error("main-db still listens, want every listener closed when one fails")
	}
	if entries, _ := runstate.List(h.stateDir()); len(entries) != 0 {
		t.Errorf("state files = %+v, want none", entries)
	}
}

func TestForward_UnknownAppNameFallsBackToTheID(t *testing.T) {
	h := newForwardHarness(t)
	r := h.forward("", false, "libsql_edge")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stderr %q)", r.exit, r.stderr)
	}
	if !strings.HasPrefix(r.stdout, "libsql-edge.internal (127.77.0.1:8080) → libsql_edge\n") {
		t.Errorf("stdout = %q, want the hostname built from the ID", r.stdout)
	}
	if !strings.Contains(r.stderr, "warning: libsql libsql_edge: appName unknown") {
		t.Errorf("stderr = %q, want a warning about the unknown appName", r.stderr)
	}
}

func TestForward_ReadsEachAppNameOnce(t *testing.T) {
	h := newForwardHarness(t)
	h.companion.ports["cmp_myapp/pgadmin"] = []int{80}
	if r := h.forward("", false, "myapp/postgres", "myapp/pgadmin", "main-db", "--json"); r.exit != 0 {
		t.Fatalf("exit = %d (stdout %q)", r.exit, r.stdout)
	}
	// pg_main was read while listing services; the stack once for both
	// of its services.
	want := map[string]int{"postgres/pg_main": 1, "libsql/libsql_edge": 1, "compose/cmp_myapp": 1}
	got := map[string]int{}
	for _, c := range h.api.detailCalls {
		got[c]++
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("detail calls = %v, want %v", got, want)
	}
}

func TestForward_MigratesNamesOfAnOlderVersion(t *testing.T) {
	h := newForwardHarness(t)
	// An older doktunnel leased main-db 127.77.0.7 under its display names
	// and wrote that name to the hosts file.
	old := `{"version":1,"leases":[{"instance":"https://panel.example.com","organization_id":"org1","service_id":"pg_main",` +
		`"ip":"127.77.0.7","created_at":"2026-01-01T00:00:00Z",` +
		`"names":{"context":"prod","organization":"Acme","project":"shop","service":"main-db"}}]}`
	if err := os.MkdirAll(filepath.Dir(h.hosts.registryPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.hosts.registryPath, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	h.hosts.writeHosts(fixtureHosts + hosts.BeginLine + "\r\n127.77.0.7\tmain-db.shop.acme.prod.internal\r\n" + hosts.EndLine + "\r\n")

	r := h.forward("", false, "main-db", "--json")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stdout %q)", r.exit, r.stdout)
	}
	if got := decodeJSON[forwardOut](t, r.stdout).summary(); !slices.Equal(got, []string{"pg_main@127.77.0.7:5432=shop-maindb-a1b2c3.internal"}) {
		t.Errorf("forwards = %q, want the old address under the new name", got)
	}
	// One sync replaced the old name.
	want := fixtureHosts + hosts.BeginLine + "\r\n127.77.0.7\tshop-maindb-a1b2c3.internal\r\n" + hosts.EndLine + "\r\n"
	if len(h.writes) == 0 || h.writes[0] != want {
		t.Errorf("hosts file writes =\n%q\nwant first\n%q", h.writes, want)
	}
}
