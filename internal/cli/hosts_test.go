package cli

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
	"github.com/alebak/dokploy-tunnel/internal/hostname"
	"github.com/alebak/dokploy-tunnel/internal/hosts"
	"github.com/alebak/dokploy-tunnel/internal/registry"
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
	// fail makes Run fail without running the helper, like a wrong
	// password or a declined UAC prompt.
	fail bool
}

func (f *fakeElevator) Run(_ context.Context, argv []string) error {
	f.calls = append(f.calls, argv)
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
	r := f.h.runAs(true, false, argv[1:]...)
	if r.exit != 0 {
		return fmt.Errorf("helper exited with %d: %s", r.exit, r.stderr)
	}
	return nil
}

func (f *fakeElevator) Command(argv []string) string {
	return "fake-sudo " + strings.Join(argv, " ")
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
	var stdout, stderr bytes.Buffer
	app := &App{
		Root:            NewRoot(),
		Stdin:           strings.NewReader(""),
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

func (h *hostsHarness) leaseFixture() {
	h.t.Helper()
	h.lease("cmp_myapp/postgres", hostname.Names{Context: "prod", Organization: "Acme", Project: "shop", Compose: "myapp", Service: "postgres"})
	h.lease("redis_cache", hostname.Names{Context: "prod", Organization: "Acme", Project: "shop", Service: "cache"})
}

const fixtureBlock = hosts.BeginLine + "\r\n" +
	"127.77.0.1\tpostgres.myapp.shop.acme.prod.internal\r\n" +
	"127.77.0.2\tcache.shop.acme.prod.internal\r\n" +
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
			wantCmd := "fake-sudo " + fakeExe + " hosts privileged-apply --entries-file " + h.pendingPath()
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
			wantPending := "127.77.0.1\tpostgres.myapp.shop.acme.prod.internal\n127.77.0.2\tcache.shop.acme.prod.internal\n"
			if string(pending) != wantPending {
				t.Errorf("pending entries = %q, want %q", pending, wantPending)
			}

			// Running the hinted command applies exactly the pending change.
			if r := h.runAs(true, false, "hosts", "privileged-apply", "--entries-file", h.pendingPath()); r.exit != 0 {
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
		"+ 127.77.0.1\tpostgres.myapp.shop.acme.prod.internal\n" +
		"+ 127.77.0.2\tcache.shop.acme.prod.internal\n"
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
		`"added":[{"ip":"127.77.0.1","hostname":"postgres.myapp.shop.acme.prod.internal"},` +
		`{"ip":"127.77.0.2","hostname":"cache.shop.acme.prod.internal"}],"removed":[],"aliases":[]}` + "\n"
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
		`{"ip":"127.77.0.1","hostname":"postgres.myapp.shop.acme.prod.internal"},` +
		`{"ip":"127.77.0.2","hostname":"cache.shop.acme.prod.internal"}]}` + "\n"
	if r.stdout != want {
		t.Errorf("stdout =\n%s\nwant\n%s", r.stdout, want)
	}

	r = h.run(false, "hosts", "list")
	if !strings.Contains(r.stdout, "127.77.0.1") || !strings.Contains(r.stdout, "postgres.myapp.shop.acme.prod.internal") {
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

func TestHostsPrivilegedApply_RejectsInvalidEntries(t *testing.T) {
	h := newHostsHarness(t)
	bad := filepath.Join(t.TempDir(), "entries")
	if err := os.WriteFile(bad, []byte("10.0.0.1\tevil.example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := h.runAs(true, false, "hosts", "privileged-apply", "--entries-file", bad)
	if r.exit == 0 {
		t.Fatal("privileged-apply accepted entries outside loopback and .internal")
	}
	if h.readHosts() != fixtureHosts {
		t.Error("hosts file changed")
	}
	if r := h.runAs(true, false, "hosts", "privileged-apply"); r.exit != clierr.InvalidArgument.ExitCode() {
		t.Errorf("privileged-apply without input: exit = %d", r.exit)
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
