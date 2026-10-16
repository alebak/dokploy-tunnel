package cli

import (
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
	"github.com/alebak/dokploy-tunnel/internal/runstate"
)

const pgadminHost = "pgadmin.shop-myapp-x1y2z3.internal"

func TestForward_RemapsPortsBelow1024(t *testing.T) {
	tests := []struct {
		name  string
		ports map[string][]int
		args  []string
		want  []string
	}{
		{
			name: "80 listens on 8080",
			args: []string{"myapp/pgadmin", "--port", "80"},
			want: []string{"cmp_myapp/pgadmin@127.77.0.1:8080→80=" + pgadminHost},
		},
		{
			name:  "a single reported 443 listens on 8443",
			ports: map[string][]int{"cmp_myapp/pgadmin": {443}},
			args:  []string{"myapp/pgadmin"},
			want:  []string{"cmp_myapp/pgadmin@127.77.0.1:8443→443=" + pgadminHost},
		},
		{
			name:  "a service exposing 80 and 8080 keeps 8080 and moves 80 to 8081",
			ports: map[string][]int{"cmp_myapp/pgadmin": {80, 8080}},
			args:  []string{"myapp/pgadmin", "--all-ports"},
			want: []string{
				"cmp_myapp/pgadmin@127.77.0.1:8081→80=" + pgadminHost,
				"cmp_myapp/pgadmin@127.77.0.1:8080=" + pgadminHost,
			},
		},
		{
			name:  "several services on 80 each listen on 8080 of their own address",
			ports: map[string][]int{"cmp_myapp/pgadmin": {80}, "app_web": {80}},
			args:  []string{"myapp/pgadmin", "web"},
			want: []string{
				"cmp_myapp/pgadmin@127.77.0.1:8080→80=" + pgadminHost,
				"app_web@127.77.0.2:8080→80=shop-web-d4e5f6.internal",
			},
		},
		{
			name: "--local-port overrides the rule",
			args: []string{"myapp/pgadmin", "--port", "80", "--local-port", "9000"},
			want: []string{"cmp_myapp/pgadmin@127.77.0.1:9000→80=" + pgadminHost},
		},
		{
			name: "--local-port also applies to ports from 1024 up",
			args: []string{"main-db", "--local-port", "15432"},
			want: []string{"pg_main@127.77.0.1:15432→5432=shop-maindb-a1b2c3.internal"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newForwardHarness(t)
			for id, ports := range tt.ports {
				h.companion.ports[id] = ports
			}
			r := h.forward("", false, append(tt.args, "--json")...)
			if r.exit != 0 {
				t.Fatalf("exit = %d (stdout %q, stderr %q)", r.exit, r.stdout, r.stderr)
			}
			out := decodeJSON[forwardOut](t, r.stdout)
			if got := out.summary(); !slices.Equal(got, tt.want) {
				t.Errorf("forwards = %q, want %q", got, tt.want)
			}
			var want []string
			for _, f := range out.Forwards {
				want = append(want, net.JoinHostPort(f.IP, strconv.Itoa(f.Port)))
			}
			if got := h.requested(); !slices.Equal(got, want) {
				t.Errorf("listened on %q, want %q", got, want)
			}
		})
	}
}

// The chosen local port is stored in the lease, so a later forward reuses
// it even when the service no longer exposes the port that pushed it up,
// and whatever order the companion reports the ports in.
func TestForward_LocalPortIsStableAcrossRuns(t *testing.T) {
	h := newForwardHarness(t)
	runs := []struct {
		ports []int
		args  []string
	}{
		{ports: []int{80, 8080}, args: []string{"--all-ports"}},
		{ports: []int{80}},
		{ports: []int{8080, 80}, args: []string{"--all-ports"}},
	}
	for i, run := range runs {
		h.companion.ports["cmp_myapp/pgadmin"] = run.ports
		r := h.forward("", false, append([]string{"myapp/pgadmin", "--json"}, run.args...)...)
		if r.exit != 0 {
			t.Fatalf("run %d: exit = %d (stdout %q)", i, r.exit, r.stdout)
		}
		got := decodeJSON[forwardOut](t, r.stdout).summary()
		if !slices.Contains(got, "cmp_myapp/pgadmin@127.77.0.1:8081→80="+pgadminHost) {
			t.Errorf("run %d: forwards = %q, want 80 on the stored 8081", i, got)
		}
	}
	b, err := os.ReadFile(h.hosts.registryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"local_ports": {`) || !strings.Contains(string(b), `"80": 8081`) {
		t.Errorf("registry = %s, want local_ports {\"80\": 8081}", b)
	}
}

func TestForward_StoredLocalPortNowExposedIsRecomputed(t *testing.T) {
	h := newForwardHarness(t)
	if r := h.forward("", false, "myapp/pgadmin", "--port", "80", "--json"); r.exit != 0 {
		t.Fatalf("exit = %d (stdout %q)", r.exit, r.stdout)
	}
	h.companion.ports["cmp_myapp/pgadmin"] = []int{80, 8080}
	r := h.forward("", false, "myapp/pgadmin", "--all-ports")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stderr %q)", r.exit, r.stderr)
	}
	want := pgadminHost + " (127.77.0.1:8081) → myapp/pgadmin:80\n" +
		pgadminHost + " (127.77.0.1:8080) → myapp/pgadmin\n"
	if r.stdout != want {
		t.Errorf("stdout =\n%s\nwant\n%s", r.stdout, want)
	}
	if !strings.Contains(r.stderr, "warning: myapp/pgadmin: port 80 now listens on 8081, not 8080, because the service exposes 8080 itself") {
		t.Errorf("stderr = %q, want a warning about the moved port", r.stderr)
	}
}

func TestForward_LocalPortIsNotStored(t *testing.T) {
	h := newForwardHarness(t)
	if r := h.forward("", false, "myapp/pgadmin", "--port", "80", "--local-port", "9000", "--json"); r.exit != 0 {
		t.Fatalf("exit = %d (stdout %q)", r.exit, r.stdout)
	}
	r := h.forward("", false, "myapp/pgadmin", "--port", "80", "--json")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stdout %q)", r.exit, r.stdout)
	}
	if got := decodeJSON[forwardOut](t, r.stdout).summary(); !slices.Equal(got, []string{"cmp_myapp/pgadmin@127.77.0.1:8080→80=" + pgadminHost}) {
		t.Errorf("forwards = %q, want the rule's 8080 after a one-off --local-port", got)
	}
}

func TestForward_LocalPortErrors(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		setup     func(h *forwardHarness)
		wantCode  clierr.Code
		wantInMsg []string
	}{
		{name: "zero", args: []string{"main-db", "--local-port", "0"}, wantCode: clierr.InvalidArgument, wantInMsg: []string{"--local-port"}},
		{name: "too large", args: []string{"main-db", "--local-port", "65536"}, wantCode: clierr.InvalidArgument, wantInMsg: []string{"--local-port"}},
		{name: "with --all-ports", args: []string{"myapp/pgadmin", "--all-ports", "--local-port", "9000"}, wantCode: clierr.InvalidArgument, wantInMsg: []string{"--local-port", "--all-ports"}},
		{name: "with several services", args: []string{"main-db", "cache", "--local-port", "9000"}, wantCode: clierr.InvalidArgument, wantInMsg: []string{"--local-port", "single"}},
		{
			name: "the stored local port of another port",
			args: []string{"myapp/pgadmin", "--port", "80", "--local-port", "8443"},
			setup: func(h *forwardHarness) {
				if r := h.forward("", false, "myapp/pgadmin", "--all-ports", "--json"); r.exit != 0 {
					h.t.Fatalf("setup: exit = %d (stdout %q)", r.exit, r.stdout)
				}
			},
			wantCode:  clierr.InvalidArgument,
			wantInMsg: []string{"--local-port 8443", "port 80", "its port 443 is already mapped to local port 8443"},
		},
		{
			name: "a port the companion reports the service exposes",
			args: []string{"myapp/pgadmin", "--port", "80", "--local-port", "8080"},
			setup: func(h *forwardHarness) {
				h.companion.ports["cmp_myapp/pgadmin"] = []int{80, 8080}
			},
			wantCode:  clierr.InvalidArgument,
			wantInMsg: []string{"--local-port 8080", "port 80", "it exposes port 8080 itself"},
		},
		{
			name: "a port a running forward listens on",
			args: []string{"myapp/pgadmin", "--port", "80", "--local-port", "9000"},
			setup: func(h *forwardHarness) {
				// Lease pgadmin 127.77.0.1, then record another process
				// forwarding its port 3000 on 127.77.0.1:9000.
				if r := h.forward("", false, "myapp/pgadmin", "--port", "3000", "--json"); r.exit != 0 {
					h.t.Fatalf("setup: exit = %d (stdout %q)", r.exit, r.stdout)
				}
				const pid = 999001
				h.hosts.alive[pid] = true
				_, err := runstate.Write(h.stateDir(), runstate.Process{
					PID: pid, StartedAt: time.Now().UTC(), Context: "prod", CompanionURL: h.companion.url(),
					Forwards: []runstate.Forward{{
						Target:   runstate.Target{Type: "compose_service", ID: "cmp_myapp/pgadmin", Name: "myapp/pgadmin"},
						Hostname: pgadminHost, IP: netip.MustParseAddr("127.77.0.1"), Port: 9000, ServicePort: 3000,
					}},
				})
				if err != nil {
					h.t.Fatal(err)
				}
			},
			wantCode:  clierr.InvalidArgument,
			wantInMsg: []string{"--local-port 9000", "127.77.0.1:9000", "port 80", "port 3000", "999001"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newForwardHarness(t)
			if tt.setup != nil {
				tt.setup(h)
			}
			before := len(h.requested())
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
			if got := h.requested(); len(got) != before {
				t.Errorf("listened on %q, want nothing after an error", got[before:])
			}
		})
	}
}

func TestForward_HumanOutputShowsTheServicePort(t *testing.T) {
	h := newForwardHarness(t)
	r := h.forward("", false, "myapp/pgadmin", "--all-ports")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stderr %q)", r.exit, r.stderr)
	}
	want := pgadminHost + " (127.77.0.1:8080) → myapp/pgadmin:80\n" +
		pgadminHost + " (127.77.0.1:8443) → myapp/pgadmin:443\n"
	if r.stdout != want {
		t.Errorf("stdout =\n%s\nwant\n%s", r.stdout, want)
	}
}

// A remapped listener tunnels to the service port, not the local one, and
// the state file records both.
func TestForward_RemappedPortTunnelsToTheServicePort(t *testing.T) {
	h := newForwardHarness(t)
	h.keepRunning()
	var stdout, stderr syncBuffer
	exited := make(chan int, 1)
	go func() {
		exited <- h.app("", false, &stdout, &stderr).Run([]string{"forward", "myapp/pgadmin", "--port", "80"})
	}()
	deadline := time.Now().Add(2 * forwardWait)
	for !strings.Contains(stderr.String(), "Ctrl+C") {
		select {
		case code := <-exited:
			t.Fatalf("forward exited with %d before listening (stdout %q, stderr %q)", code, stdout.String(), stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("forward did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}

	state, err := runstate.Read(runstate.Path(h.stateDir(), os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	if f := state.Forwards; len(f) != 1 || f[0].Port != 8080 || f[0].ServicePort != 80 {
		t.Errorf("state forwards = %+v, want port 8080 and service port 80", f)
	}

	h.mu.Lock()
	ln := h.listens["127.77.0.1:8080"]
	h.mu.Unlock()
	if ln == nil {
		t.Fatalf("not listening on 127.77.0.1:8080; requested %q", h.requested())
	}
	conn, err := net.DialTimeout("tcp", ln.Addr().String(), forwardWait)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(forwardWait))
	if _, err := conn.Write([]byte("GET /")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	h.companion.mu.Lock()
	tunnels := slices.Clone(h.companion.tunnels)
	h.companion.mu.Unlock()
	if len(tunnels) != 1 || !strings.HasSuffix(tunnels[0], ":80") {
		t.Errorf("tunnels = %q, want one to port 80", tunnels)
	}
	for !strings.Contains(stderr.String(), "myapp/pgadmin:80: connection 1 opened") {
		if time.Now().After(deadline) {
			t.Fatalf("stderr = %q, want events named after the service port", stderr.String())
		}
		time.Sleep(5 * time.Millisecond)
	}

	h.stopCancel()
	select {
	case <-exited:
	case <-time.After(forwardWait):
		t.Fatal("forward did not exit")
	}
}

// A --port below 1024 asks the companion which ports the service exposes,
// so its local port skips them; a port from 1024 up never asks.
func TestForward_LowPortAsksTheCompanionForExposedPorts(t *testing.T) {
	tests := []struct {
		name       string
		ports      []int
		portsErr   *rejection
		args       []string
		want       []string
		portsCalls []string
	}{
		{
			name:       "a service exposing 80 and 8080 maps --port 80 to 8081",
			ports:      []int{80, 8080},
			args:       []string{"myapp/pgadmin", "--port", "80"},
			want:       []string{"cmp_myapp/pgadmin@127.77.0.1:8081→80=" + pgadminHost},
			portsCalls: []string{"cmp_myapp/pgadmin"},
		},
		{
			name:       "a companion without the ports endpoint falls back to the rule",
			ports:      []int{80, 8080},
			portsErr:   &rejection{http.StatusNotFound, "404 page not found\n"},
			args:       []string{"myapp/pgadmin", "--port", "80"},
			want:       []string{"cmp_myapp/pgadmin@127.77.0.1:8080→80=" + pgadminHost},
			portsCalls: []string{"cmp_myapp/pgadmin"},
		},
		{
			name:       "a refused ports request falls back to the rule",
			portsErr:   &rejection{http.StatusForbidden, `{"code":"permission_denied","message":"no access"}`},
			args:       []string{"myapp/pgadmin", "--port", "80"},
			want:       []string{"cmp_myapp/pgadmin@127.77.0.1:8080→80=" + pgadminHost},
			portsCalls: []string{"cmp_myapp/pgadmin"},
		},
		{
			name:  "--port from 1024 up does not ask",
			ports: []int{80, 8080},
			args:  []string{"myapp/pgadmin", "--port", "8080"},
			want:  []string{"cmp_myapp/pgadmin@127.77.0.1:8080=" + pgadminHost},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newForwardHarness(t)
			if tt.ports != nil {
				h.companion.ports["cmp_myapp/pgadmin"] = tt.ports
			}
			h.companion.portsErr = tt.portsErr
			r := h.forward("", false, append(tt.args, "--json")...)
			if r.exit != 0 {
				t.Fatalf("exit = %d (stdout %q, stderr %q)", r.exit, r.stdout, r.stderr)
			}
			if r.stderr != "" {
				t.Errorf("stderr = %q, want nothing in JSON mode", r.stderr)
			}
			if got := decodeJSON[forwardOut](t, r.stdout).summary(); !slices.Equal(got, tt.want) {
				t.Errorf("forwards = %q, want %q", got, tt.want)
			}
			if got := h.companion.portsCalls; !slices.Equal(got, tt.portsCalls) {
				t.Errorf("ports requests = %q, want %q", got, tt.portsCalls)
			}
		})
	}
}

func TestForward_LowPortWarnsWhenTheCompanionDoesNotReportPorts(t *testing.T) {
	h := newForwardHarness(t)
	h.companion.portsErr = &rejection{http.StatusNotFound, "404 page not found\n"}
	r := h.forward("", false, "myapp/pgadmin", "--port", "80")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stderr %q)", r.exit, r.stderr)
	}
	if want := pgadminHost + " (127.77.0.1:8080) → myapp/pgadmin:80\n"; r.stdout != want {
		t.Errorf("stdout = %q, want %q", r.stdout, want)
	}
	const warning = "warning: myapp/pgadmin: the companion did not report the ports the service exposes"
	if strings.Count(r.stderr, "warning:") != 1 || !strings.Contains(r.stderr, warning) || !strings.Contains(r.stderr, "port 80 listens on 8080") {
		t.Errorf("stderr = %q, want one warning starting %q and naming port 80 on 8080", r.stderr, warning)
	}
}

func TestForward_LowLocalPortBindFailureHints(t *testing.T) {
	tests := []struct {
		name         string
		denied, busy bool
		wantInHint   []string
		notInHint    string
	}{
		{name: "permission denied names privileges only", denied: true, wantInHint: []string{"below 1024", "--local-port"}, notInHint: "another process"},
		// Windows refuses a low port with an error that is not
		// os.ErrPermission, so an unknown failure names both causes.
		{name: "another failure names both causes", busy: true, wantInHint: []string{"below 1024", "--local-port", "another process", "doktunnel status"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newForwardHarness(t)
			const addr = "127.77.0.1:80"
			h.denied = map[string]bool{addr: tt.denied}
			h.busy = map[string]bool{addr: tt.busy}
			r := h.forward("", false, "main-db", "--local-port", "80", "--json")
			if want := clierr.Internal.ExitCode(); r.exit != want {
				t.Fatalf("exit = %d, want %d (stdout %q)", r.exit, want, r.stdout)
			}
			e := decodeError(t, r.stdout)
			if !strings.Contains(e.Message, "forwarding port 5432 of main-db on "+addr) {
				t.Errorf("message = %q, want the bind failure", e.Message)
			}
			for _, s := range tt.wantInHint {
				if !strings.Contains(e.Hint, s) {
					t.Errorf("hint = %q, want it to mention %q", e.Hint, s)
				}
			}
			if tt.notInHint != "" && strings.Contains(e.Hint, tt.notInHint) {
				t.Errorf("hint = %q, want no %q", e.Hint, tt.notInHint)
			}
		})
	}
}
