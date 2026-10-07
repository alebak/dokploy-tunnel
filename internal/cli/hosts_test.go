package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
	"github.com/alebak/dokploy-tunnel/internal/hostname"
	"github.com/alebak/dokploy-tunnel/internal/hosts"
	"github.com/alebak/dokploy-tunnel/internal/registry"
	"github.com/alebak/dokploy-tunnel/internal/runstate"
)

const fixtureHosts = "127.0.0.1\tlocalhost\r\n" +
	"# keep this comment\r\n" +
	"10.0.0.5  nas.lan nas\r\n"

const fakeExe = "/opt/doktunnel/bin/doktunnel"

// fakeElevator stands in for sudo or UAC: it records each request and runs
// the helper in-process, as the elevated process would, against the same
// fixture files.
type fakeElevator struct {
	h     *hostsHarness
	calls [][]string
	// stdins holds what each call passed on standard input.
	stdins []string
	// noStdin makes it behave like UAC, which cannot pass standard input.
	noStdin bool
	// fail makes Run fail without running the helper, like a wrong
	// password or a declined UAC prompt.
	fail bool
	// block makes Run wait until its context ends, like a password prompt
	// nobody answers.
	block bool
}

func (f *fakeElevator) Run(ctx context.Context, argv []string, stdin io.Reader) error {
	f.calls = append(f.calls, argv)
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	var in []byte
	if stdin != nil {
		if f.noStdin {
			return fmt.Errorf("this elevator cannot pass standard input")
		}
		var err error
		if in, err = io.ReadAll(stdin); err != nil {
			return err
		}
	}
	f.stdins = append(f.stdins, string(in))
	if f.fail {
		return fmt.Errorf("sudo: 3 incorrect password attempts")
	}
	if f.h.hostsPath == "" {
		// The helper would fall back to the real hosts file.
		return fmt.Errorf("refusing to run the helper without a fixture hosts file")
	}
	if argv[0] != fakeExe {
		return fmt.Errorf("unexpected executable %q", argv[0])
	}
	r := f.h.runWithStdin(true, false, string(in), argv[1:]...)
	if r.exit != 0 {
		return fmt.Errorf("helper exited with %d: %s", r.exit, r.stderr)
	}
	return nil
}

func (f *fakeElevator) PipesStdin() bool { return !f.noStdin }

func (f *fakeElevator) Command(argv []string, stdinFile string) string {
	s := "fake-sudo " + strings.Join(argv, " ")
	if stdinFile != "" {
		s += " < " + stdinFile
	}
	return s
}

// fakeLoopback is a lo0 that starts with the given aliases missing.
type fakeLoopback struct {
	missing map[netip.Addr]bool
	added   []netip.Addr
}

func (l *fakeLoopback) Missing(_ context.Context, ips []netip.Addr) ([]netip.Addr, error) {
	var out []netip.Addr
	for _, ip := range ips {
		if l.missing[ip] {
			out = append(out, ip)
		}
	}
	return out, nil
}

func (l *fakeLoopback) Add(_ context.Context, ips []netip.Addr) error {
	for _, ip := range ips {
		delete(l.missing, ip)
		l.added = append(l.added, ip)
	}
	return nil
}

// hostsHarness wires the hosts commands to a fixture hosts file, a temp
// registry, a fake elevator and a fake loopback interface. Nothing touches
// the real hosts file, sudo, UAC or ifconfig.
type hostsHarness struct {
	t            *testing.T
	hostsPath    string
	registryPath string
	elevator     *fakeElevator
	loopback     *fakeLoopback
	env          map[string]string
	// denyWrite makes every unprivileged write fail like /etc/hosts does
	// for a normal user.
	denyWrite bool
	// alive are the PIDs of running forward processes, besides the test
	// process itself in the forward tests.
	alive map[int]bool
}

func newHostsHarness(t *testing.T) *hostsHarness {
	t.Helper()
	dir := t.TempDir()
	h := &hostsHarness{
		t:            t,
		hostsPath:    filepath.Join(dir, "etc", "hosts"),
		registryPath: filepath.Join(dir, "state", "doktunnel", "addresses.json"),
		loopback:     &fakeLoopback{missing: map[netip.Addr]bool{}},
		env:          map[string]string{},
		alive:        map[int]bool{},
	}
	h.elevator = &fakeElevator{h: h}
	if err := os.MkdirAll(filepath.Dir(h.hostsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	h.writeHosts(fixtureHosts)
	return h
}

func (h *hostsHarness) run(terminal bool, args ...string) result {
	h.t.Helper()
	return h.runAs(false, terminal, args...)
}

func (h *hostsHarness) runAs(privileged, terminal bool, args ...string) result {
	h.t.Helper()
	return h.runWithStdin(privileged, terminal, "", args...)
}

func (h *hostsHarness) runWithStdin(privileged, terminal bool, stdin string, args ...string) result {
	h.t.Helper()
	var stdout, stderr bytes.Buffer
	app := &App{
		Root:            NewRoot(),
		Stdin:           strings.NewReader(stdin),
		Stdout:          &stdout,
		Stderr:          &stderr,
		StdinIsTerminal: terminal,
		ConfigPath:      filepath.Join(filepath.Dir(h.registryPath), "config.json"),
		HostsPath:       h.hostsPath,
		RegistryPath:    h.registryPath,
		Elevator:        h.elevator,
		Loopback:        h.loopback,
		Executable:      func() (string, error) { return fakeExe, nil },
		Getenv:          func(k string) string { return h.env[k] },
		ProcessAlive:    func(pid int) bool { return h.alive[pid] },
		WriteHosts: func(path string, data []byte) error {
			if h.denyWrite && !privileged {
				return &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission}
			}
			return hosts.Write(path, data)
		},
	}
	exit := app.Run(args)
	return result{exit: exit, stdout: stdout.String(), stderr: stderr.String()}
}

func (h *hostsHarness) writeHosts(content string) {
	h.t.Helper()
	if err := os.WriteFile(h.hostsPath, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *hostsHarness) readHosts() string {
	h.t.Helper()
	b, err := os.ReadFile(h.hostsPath)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

// lease registers a named service in the registry, as forward will.
func (h *hostsHarness) lease(serviceID string, n hostname.Names) netip.Addr {
	h.t.Helper()
	reg, err := registry.Open(h.registryPath)
	if err != nil {
		h.t.Fatal(err)
	}
	k := registry.Key{Instance: "https://panel.example.com", OrganizationID: "org1", ServiceID: serviceID}
	ip, err := reg.Lease(k)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := reg.SetNames(k, n); err != nil {
		h.t.Fatal(err)
	}
	return ip
}

// liveForward records a running forward process pid that listens on ips,
// as forward does while it runs.
func (h *hostsHarness) liveForward(pid int, ips ...netip.Addr) {
	h.t.Helper()
	h.alive[pid] = true
	p := runstate.Process{PID: pid, StartedAt: time.Now().UTC(), Context: "prod", CompanionURL: "https://panel.example.com/doktunnel"}
	for _, ip := range ips {
		p.Forwards = append(p.Forwards, runstate.Forward{
			Target: runstate.Target{Type: "postgres", ID: "id-" + ip.String(), Name: "svc"}, IP: ip, Port: 5432,
		})
	}
	if _, err := runstate.Write(runstate.Dir(filepath.Dir(h.registryPath)), p); err != nil {
		h.t.Fatal(err)
	}
}

// fixturePID is the forward process leaseFixture records as running.
const fixturePID = 4242

// leaseFixture registers two named services and a running forward of
// both.
func (h *hostsHarness) leaseFixture() {
	h.t.Helper()
	pg := h.lease("cmp_myapp/postgres", hostname.Names{Context: "prod", AppName: "shop-myapp-x1y2z3", ComposeService: "postgres"})
	cache := h.lease("redis_cache", hostname.Names{Context: "prod", AppName: "shop-cache-g7h8i9"})
	h.liveForward(fixturePID, pg, cache)
}

// fixtureEntries is what the privileged helper receives for leaseFixture.
const fixtureEntries = "127.77.0.1\tpostgres.shop-myapp-x1y2z3.internal\n127.77.0.2\tshop-cache-g7h8i9.internal\n"

const fixtureBlock = hosts.BeginLine + "\r\n" +
	"127.77.0.1\tpostgres.shop-myapp-x1y2z3.internal\r\n" +
	"127.77.0.2\tshop-cache-g7h8i9.internal\r\n" +
	hosts.EndLine + "\r\n"

func (h *hostsHarness) pendingPath() string {
	return filepath.Join(filepath.Dir(h.registryPath), "pending-hosts")
}

func TestHostsSync_WritesBlockAndKeepsUnrelatedLines(t *testing.T) {
	h := newHostsHarness(t)
	h.leaseFixture()

	r := h.run(true, "hosts", "sync")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stderr %q)", r.exit, r.stderr)
	}
	if got, want := h.readHosts(), fixtureHosts+fixtureBlock; got != want {
		t.Errorf("hosts file =\n%q\nwant\n%q", got, want)
	}
	if len(h.elevator.calls) != 0 {
		t.Errorf("elevated %d times for a writable file", len(h.elevator.calls))
	}
	if !strings.Contains(r.stdout, "2 added, 0 removed") {
		t.Errorf("stdout = %q, want a summary of the change", r.stdout)
	}
}

func TestHostsSync_ElevatesOnlyThePrivilegedStep(t *testing.T) {
	h := newHostsHarness(t)
	h.leaseFixture()
	h.denyWrite = true

	r := h.run(true, "hosts", "sync")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stderr %q)", r.exit, r.stderr)
	}
	// The entries travel on stdin: root never opens a path the caller chose.
	want := [][]string{{fakeExe, "hosts", "privileged-apply", "--entries-file", "-"}}
	if !slices.EqualFunc(h.elevator.calls, want, slices.Equal) {
		t.Fatalf("elevator calls = %q, want %q", h.elevator.calls, want)
	}
	if want := []string{fixtureEntries}; !slices.Equal(h.elevator.stdins, want) {
		t.Errorf("helper stdin = %q, want %q", h.elevator.stdins, want)
	}
	if got := h.readHosts(); got != fixtureHosts+fixtureBlock {
		t.Errorf("hosts file =\n%q", got)
	}
	if _, err := os.Stat(h.pendingPath()); !os.IsNotExist(err) {
		t.Errorf("pending entries file written although stdin carries the entries: %v", err)
	}
	if !strings.Contains(r.stderr, "administrator privileges") {
		t.Errorf("stderr = %q, want a note before elevating", r.stderr)
	}
}

func TestHostsSync_FailedElevationLeavesFileAlone(t *testing.T) {
	h := newHostsHarness(t)
	h.leaseFixture()
	h.denyWrite = true
	h.elevator.fail = true

	r := h.run(true, "hosts", "sync", "--json")
	if r.exit != clierr.ElevationRequired.ExitCode() {
		t.Fatalf("exit = %d, want %d (stdout %q)", r.exit, clierr.ElevationRequired.ExitCode(), r.stdout)
	}
	if e := decodeError(t, r.stdout); !strings.Contains(e.Hint, "fake-sudo "+fakeExe) {
		t.Errorf("hint = %q, want the command to run by hand", e.Hint)
	}
	if got := h.readHosts(); got != fixtureHosts {
		t.Errorf("hosts file changed: %q", got)
	}
}

func TestHostsSync_NoInputReturnsElevationRequired(t *testing.T) {
	for _, args := range [][]string{
		{"hosts", "sync", "--no-input", "--json"},
		{"hosts", "sync", "--json"}, // stdin is not a terminal
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h := newHostsHarness(t)
			h.leaseFixture()
			h.denyWrite = true

			r := h.run(len(args) == 4, args...)
			if r.exit != clierr.ElevationRequired.ExitCode() {
				t.Fatalf("exit = %d, want %d (stdout %q)", r.exit, clierr.ElevationRequired.ExitCode(), r.stdout)
			}
			e := decodeError(t, r.stdout)
			// The user's own shell, not root, opens the pending file.
			wantCmd := "fake-sudo " + fakeExe + " hosts privileged-apply --entries-file - < " + h.pendingPath()
			if e.Code != clierr.ElevationRequired || !strings.Contains(e.Hint, wantCmd) {
				t.Errorf("error = %+v, want elevation_required with hint containing %q", e, wantCmd)
			}
			if len(h.elevator.calls) != 0 {
				t.Errorf("elevated without input allowed: %q", h.elevator.calls)
			}
			if got := h.readHosts(); got != fixtureHosts {
				t.Errorf("hosts file changed: %q", got)
			}
			pending, err := os.ReadFile(h.pendingPath())
			if err != nil {
				t.Fatalf("pending entries file: %v", err)
			}
			if string(pending) != fixtureEntries {
				t.Errorf("pending entries = %q, want %q", pending, fixtureEntries)
			}

			// Running the hinted command applies exactly the pending change.
			if r := h.runWithStdin(true, false, string(pending), "hosts", "privileged-apply", "--entries-file", "-"); r.exit != 0 {
				t.Fatalf("privileged-apply exit = %d (stderr %q)", r.exit, r.stderr)
			}
			if got := h.readHosts(); got != fixtureHosts+fixtureBlock {
				t.Errorf("hosts file after the hinted command =\n%q", got)
			}
		})
	}
}

func TestHostsSync_UnchangedNeverElevates(t *testing.T) {
	h := newHostsHarness(t)
	h.leaseFixture()
	h.writeHosts(fixtureHosts + fixtureBlock)
	h.denyWrite = true

	r := h.run(false, "hosts", "sync", "--no-input", "--json")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stdout %q)", r.exit, r.stdout)
	}
	if len(h.elevator.calls) != 0 {
		t.Errorf("elevated although nothing changed: %q", h.elevator.calls)
	}
	got := decodeJSON[hostsSyncJSON](t, r.stdout)
	if got.Changed || len(got.Added) != 0 || len(got.Removed) != 0 {
		t.Errorf("result = %+v, want no change", got)
	}
	if h.readHosts() != fixtureHosts+fixtureBlock {
		t.Error("hosts file changed")
	}
}

func TestHostsSync_DryRun(t *testing.T) {
	h := newHostsHarness(t)
	h.leaseFixture()
	h.writeHosts(fixtureHosts + hosts.BeginLine + "\r\n127.77.0.9\told.shop.acme.prod.internal\r\n" + hosts.EndLine + "\r\n")
	before := h.readHosts()
	h.denyWrite = true

	r := h.run(false, "hosts", "sync", "--dry-run", "--no-input")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stderr %q)", r.exit, r.stderr)
	}
	wantDiff := "- 127.77.0.9\told.shop.acme.prod.internal\n" +
		"+ 127.77.0.1\tpostgres.shop-myapp-x1y2z3.internal\n" +
		"+ 127.77.0.2\tshop-cache-g7h8i9.internal\n"
	if !strings.Contains(r.stdout, wantDiff) {
		t.Errorf("stdout =\n%s\nwant it to contain\n%s", r.stdout, wantDiff)
	}

	r = h.run(false, "hosts", "sync", "--dry-run", "--json")
	got := decodeJSON[hostsSyncJSON](t, r.stdout)
	if !got.DryRun || !got.Changed || got.HostsFile != h.hostsPath || len(got.Added) != 2 || len(got.Removed) != 1 ||
		got.Removed[0].Hostname != "old.shop.acme.prod.internal" {
		t.Errorf("JSON = %+v", got)
	}
	if h.readHosts() != before || len(h.elevator.calls) != 0 {
		t.Error("dry run wrote the hosts file or elevated")
	}
	if _, err := os.Stat(h.pendingPath()); !os.IsNotExist(err) {
		t.Errorf("dry run left a pending entries file: %v", err)
	}
}

func TestHostsSync_JSONShape(t *testing.T) {
	h := newHostsHarness(t)
	h.leaseFixture()
	r := h.run(false, "hosts", "sync", "--json")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stdout %q)", r.exit, r.stdout)
	}
	want := `{"hosts_file":"` + jsonEscape(h.hostsPath) + `","dry_run":false,"changed":true,` +
		`"added":[{"ip":"127.77.0.1","hostname":"postgres.shop-myapp-x1y2z3.internal"},` +
		`{"ip":"127.77.0.2","hostname":"shop-cache-g7h8i9.internal"}],"removed":[],"aliases":[]}` + "\n"
	if r.stdout != want {
		t.Errorf("stdout =\n%s\nwant\n%s", r.stdout, want)
	}
}

func TestHostsSync_MalformedMarkersAreRefused(t *testing.T) {
	h := newHostsHarness(t)
	h.leaseFixture()
	content := fixtureHosts + hosts.BeginLine + "\r\n127.77.0.9\told.shop.acme.prod.internal\r\n"
	h.writeHosts(content)

	r := h.run(false, "hosts", "sync", "--json")
	if r.exit != clierr.Internal.ExitCode() {
		t.Fatalf("exit = %d (stdout %q)", r.exit, r.stdout)
	}
	if e := decodeError(t, r.stdout); !strings.Contains(e.Message, "line 4") || !strings.Contains(e.Hint, "doktunnel hosts clean") {
		t.Errorf("error = %+v, want the line and a hint to run hosts clean", e)
	}
	if h.readHosts() != content {
		t.Error("hosts file changed")
	}
}

func TestHostsSync_MissingAliasesElevateEvenWhenHostsAreCurrent(t *testing.T) {
	h := newHostsHarness(t)
	h.leaseFixture()
	h.writeHosts(fixtureHosts + fixtureBlock)
	h.denyWrite = true
	// After a macOS reboot the hosts file is intact but lo0 lost its aliases.
	h.loopback.missing = map[netip.Addr]bool{netip.MustParseAddr("127.77.0.2"): true}

	r := h.run(true, "hosts", "sync", "--json")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stdout %q, stderr %q)", r.exit, r.stdout, r.stderr)
	}
	if len(h.elevator.calls) != 1 {
		t.Fatalf("elevator calls = %q, want one", h.elevator.calls)
	}
	if want := []netip.Addr{netip.MustParseAddr("127.77.0.2")}; !slices.Equal(h.loopback.added, want) {
		t.Errorf("aliases added = %v, want %v", h.loopback.added, want)
	}
	got := decodeJSON[hostsSyncJSON](t, r.stdout)
	if !got.Changed || len(got.Aliases) != 1 || got.Aliases[0] != "127.77.0.2" {
		t.Errorf("JSON = %+v", got)
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q, want nothing in JSON mode", r.stderr)
	}
}

func TestHostsSync_WritesOnlyNamesOfRunningForwards(t *testing.T) {
	h := newHostsHarness(t)
	pg := h.lease("cmp_myapp/postgres", hostname.Names{Context: "prod", AppName: "shop-myapp-x1y2z3", ComposeService: "postgres"})
	h.lease("redis_cache", hostname.Names{Context: "prod", AppName: "shop-cache-g7h8i9"})
	h.liveForward(fixturePID, pg)
	// A forward that was killed left its state file behind.
	h.liveForward(999, netip.MustParseAddr("127.77.0.2"))
	h.alive[999] = false
	h.writeHosts(fixtureHosts + fixtureBlock)

	r := h.run(false, "hosts", "sync", "--json")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stdout %q)", r.exit, r.stdout)
	}
	want := fixtureHosts + hosts.BeginLine + "\r\n127.77.0.1\tpostgres.shop-myapp-x1y2z3.internal\r\n" + hosts.EndLine + "\r\n"
	if got := h.readHosts(); got != want {
		t.Errorf("hosts file =\n%q\nwant\n%q", got, want)
	}
	got := decodeJSON[hostsSyncJSON](t, r.stdout)
	if len(got.Removed) != 1 || got.Removed[0].Hostname != "shop-cache-g7h8i9.internal" || len(got.Added) != 0 {
		t.Errorf("JSON = %+v, want only the stopped forward's name removed", got)
	}

	// Once no forward runs, the section goes.
	h.alive[fixturePID] = false
	if r := h.run(false, "hosts", "sync"); r.exit != 0 {
		t.Fatalf("exit = %d (stderr %q)", r.exit, r.stderr)
	}
	if got := h.readHosts(); got != fixtureHosts {
		t.Errorf("hosts file =\n%q\nwant\n%q", got, fixtureHosts)
	}
}

func TestHostsSync_RemovesEntriesOfForgottenLeases(t *testing.T) {
	h := newHostsHarness(t)
	h.writeHosts(fixtureHosts + fixtureBlock)
	// The registry is empty: every doktunnel entry goes, and the block too.
	r := h.run(false, "hosts", "sync")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stderr %q)", r.exit, r.stderr)
	}
	if got := h.readHosts(); got != fixtureHosts {
		t.Errorf("hosts file =\n%q\nwant\n%q", got, fixtureHosts)
	}
}

func TestHostsSync_TestOverrideNeverElevates(t *testing.T) {
	h := newHostsHarness(t)
	h.leaseFixture()
	override := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(override, []byte(fixtureHosts), 0o644); err != nil {
		t.Fatal(err)
	}
	h.hostsPath = ""
	h.env[hostsFileEnv] = override
	h.denyWrite = true

	r := h.run(true, "hosts", "sync", "--json")
	if r.exit != clierr.PermissionDenied.ExitCode() {
		t.Fatalf("exit = %d (stdout %q)", r.exit, r.stdout)
	}
	if len(h.elevator.calls) != 0 {
		t.Errorf("elevated for an overridden hosts file: %q", h.elevator.calls)
	}

	h.denyWrite = false
	if r := h.run(true, "hosts", "sync"); r.exit != 0 {
		t.Fatalf("exit = %d (stderr %q)", r.exit, r.stderr)
	}
	if got, _ := os.ReadFile(override); string(got) != fixtureHosts+fixtureBlock {
		t.Errorf("override file =\n%q", got)
	}
}

func TestHostsList(t *testing.T) {
	h := newHostsHarness(t)
	h.writeHosts(fixtureHosts + fixtureBlock)

	r := h.run(false, "hosts", "list", "--json")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stdout %q)", r.exit, r.stdout)
	}
	want := `{"hosts_file":"` + jsonEscape(h.hostsPath) + `","entries":[` +
		`{"ip":"127.77.0.1","hostname":"postgres.shop-myapp-x1y2z3.internal"},` +
		`{"ip":"127.77.0.2","hostname":"shop-cache-g7h8i9.internal"}]}` + "\n"
	if r.stdout != want {
		t.Errorf("stdout =\n%s\nwant\n%s", r.stdout, want)
	}

	r = h.run(false, "hosts", "list")
	if !strings.Contains(r.stdout, "127.77.0.1") || !strings.Contains(r.stdout, "postgres.shop-myapp-x1y2z3.internal") {
		t.Errorf("human output = %q", r.stdout)
	}

	h.writeHosts(fixtureHosts)
	r = h.run(false, "hosts", "list", "--json")
	if r.stdout != `{"hosts_file":"`+jsonEscape(h.hostsPath)+`","entries":[]}`+"\n" {
		t.Errorf("stdout without a block = %s", r.stdout)
	}
}

func TestHostsClean(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "well-formed block", content: fixtureHosts + fixtureBlock},
		{name: "dangling begin marker", content: fixtureHosts + hosts.BeginLine + "\r\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHostsHarness(t)
			h.writeHosts(tt.content)
			h.denyWrite = true

			r := h.run(false, "hosts", "clean", "--json")
			if r.exit != clierr.ElevationRequired.ExitCode() {
				t.Fatalf("exit = %d (stdout %q)", r.exit, r.stdout)
			}
			if e := decodeError(t, r.stdout); !strings.Contains(e.Hint, "hosts privileged-apply --clean") {
				t.Errorf("hint = %q", e.Hint)
			}

			r = h.run(true, "hosts", "clean", "--json")
			if r.exit != 0 {
				t.Fatalf("exit = %d (stdout %q)", r.exit, r.stdout)
			}
			if got := h.readHosts(); got != fixtureHosts {
				t.Errorf("hosts file =\n%q\nwant\n%q", got, fixtureHosts)
			}
			if got := decodeJSON[hostsCleanJSON](t, r.stdout); !got.Changed {
				t.Errorf("JSON = %+v, want changed", got)
			}

			// A second clean finds nothing and needs no privileges.
			calls := len(h.elevator.calls)
			r = h.run(false, "hosts", "clean", "--json")
			if r.exit != 0 || len(h.elevator.calls) != calls {
				t.Fatalf("second clean: exit %d, elevations %d", r.exit, len(h.elevator.calls)-calls)
			}
			if got := decodeJSON[hostsCleanJSON](t, r.stdout); got.Changed {
				t.Errorf("second clean JSON = %+v, want unchanged", got)
			}
		})
	}
}

func TestHostsSync_ElevatesThroughAFileWhenStdinCannotBePiped(t *testing.T) {
	h := newHostsHarness(t)
	h.leaseFixture()
	h.denyWrite = true
	h.elevator.noStdin = true // UAC

	r := h.run(true, "hosts", "sync")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stderr %q)", r.exit, r.stderr)
	}
	want := [][]string{{fakeExe, "hosts", "privileged-apply", "--entries-file", h.pendingPath()}}
	if !slices.EqualFunc(h.elevator.calls, want, slices.Equal) {
		t.Fatalf("elevator calls = %q, want %q", h.elevator.calls, want)
	}
	if got := h.readHosts(); got != fixtureHosts+fixtureBlock {
		t.Errorf("hosts file =\n%q", got)
	}
	if _, err := os.Stat(h.pendingPath()); !os.IsNotExist(err) {
		t.Errorf("pending entries file left behind after a successful sync: %v", err)
	}
}

func TestHostsPrivilegedApply_RejectsInvalidEntries(t *testing.T) {
	const secret = "10.0.0.1\tevil.example.com\n"
	dir := t.TempDir()
	badFile := filepath.Join(dir, "entries")
	if err := os.WriteFile(badFile, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		stdin string
		args  []string
	}{
		{name: "invalid entries on stdin", stdin: secret, args: []string{"--entries-file", "-"}},
		{name: "invalid entries in a file", args: []string{"--entries-file", badFile}},
		{name: "oversized stdin", stdin: strings.Repeat("\n", 4<<20+1), args: []string{"--entries-file", "-"}},
		{name: "a directory", args: []string{"--entries-file", dir}},
		{name: "no input", args: nil},
	}
	if runtime.GOOS != "windows" {
		valid := filepath.Join(dir, "valid")
		if err := os.WriteFile(valid, []byte(fixtureEntries), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link")
		if err := os.Symlink(valid, link); err != nil {
			t.Fatal(err)
		}
		tests = append(tests, struct {
			name  string
			stdin string
			args  []string
		}{name: "a symlink to valid entries", args: []string{"--entries-file", link}})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHostsHarness(t)
			r := h.runWithStdin(true, false, tt.stdin, append([]string{"hosts", "privileged-apply"}, tt.args...)...)
			if r.exit != clierr.InvalidArgument.ExitCode() {
				t.Errorf("exit = %d, want %d (stderr %q)", r.exit, clierr.InvalidArgument.ExitCode(), r.stderr)
			}
			if out := r.stdout + r.stderr; strings.Contains(out, "evil") || strings.Contains(out, "10.0.0.1") {
				t.Errorf("output echoes the rejected entries: %q", out)
			}
			if h.readHosts() != fixtureHosts {
				t.Error("hosts file changed")
			}
		})
	}
}

func TestHostsPrivilegedApply_IsHidden(t *testing.T) {
	r := run(t, NewRoot(), "", false, "hosts", "--help")
	if r.exit != 0 {
		t.Fatalf("exit = %d", r.exit)
	}
	for _, name := range []string{"sync", "list", "clean"} {
		if !strings.Contains(r.stdout, name) {
			t.Errorf("hosts help does not list %q:\n%s", name, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "privileged-apply") {
		t.Errorf("hosts help lists the hidden helper:\n%s", r.stdout)
	}
	if skill := run(t, NewRoot(), "", false, "--skill").stdout; strings.Contains(skill, "privileged-apply") {
		t.Error("the skill documents the hidden helper")
	}
}

func jsonEscape(s string) string {
	return strings.ReplaceAll(s, `\`, `\\`)
}
