//go:build platform

package platformtest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
	"github.com/alebak/dokploy-tunnel/internal/elevate"
	"github.com/alebak/dokploy-tunnel/internal/hostname"
	"github.com/alebak/dokploy-tunnel/internal/hosts"
	"github.com/alebak/dokploy-tunnel/internal/registry"
)

// Unrelated lines the round trip puts around the doktunnel block; they must
// survive "hosts sync" and "hosts clean" byte for byte.
const (
	lineBefore = "127.0.0.1\tbefore.doktunnel-platform.test"
	lineAfter  = "127.0.0.1\tafter.doktunnel-platform.test"
)

// TestHosts_SyncAndCleanWithPrivileges runs the real doktunnel binary
// against the system hosts file: "hosts sync" must write the block through
// the privileged helper (sudo on Linux and macOS; on Windows the runner is
// already an Administrator, so no elevation is needed), the entries must
// resolve through the system resolver and accept connections, and "hosts
// clean" must leave every unrelated line untouched.
func TestHosts_SyncAndCleanWithPrivileges(t *testing.T) {
	requirePlatform(t)
	exe := buildDoktunnel(t)
	state := t.TempDir()
	env := cliEnv(state)
	path := hosts.DefaultPath()
	original := readHostsFile(t, path)
	eol := lineEnding(original)
	t.Cleanup(func() { restoreHosts(t, path, original) })

	before := withLine(original, lineBefore, eol)
	writeSystemHosts(t, path, before)
	want := seedRegistry(t, filepath.Join(state, "doktunnel", "addresses.json"))
	pending := filepath.Join(state, "doktunnel", "pending-hosts")
	if runtime.GOOS == "darwin" {
		for _, e := range want {
			t.Cleanup(func() { removeLo0Alias(t, e.IP) })
		}
	}

	// Without a terminal, doktunnel must not elevate on its own.
	out, err := runCLI(exe, env, "--no-input", "--json", "hosts", "sync")
	switch code := exitCode(err); {
	case runtime.GOOS == "windows" && code == 0:
		report(t, "'hosts sync --no-input' wrote %s directly: the runner process is already elevated, so no UAC prompt was needed", path)
	case runtime.GOOS == "windows":
		finding(t, "'hosts sync --no-input' exited %d on a runner expected to be elevated: %s", code, out)
		t.Fatalf("hosts sync --no-input: exit %d", code)
	case code != clierr.ElevationRequired.ExitCode() || !strings.Contains(out, string(clierr.ElevationRequired)) || !strings.Contains(out, "privileged-apply"):
		t.Fatalf("hosts sync --no-input: exit %d, output %q; want exit %d with an elevation_required hint", code, out, clierr.ElevationRequired.ExitCode())
	default:
		if got := readHostsFile(t, path); !bytes.Equal(got, before) {
			t.Fatalf("hosts sync --no-input changed %s without privileges", path)
		}
		if _, err := os.Stat(pending); err != nil {
			t.Fatalf("hosts sync --no-input left no pending entries: %v", err)
		}
		report(t, "'hosts sync --no-input' refused to elevate and returned elevation_required with a runnable hint")

		// With a terminal, it runs the helper through sudo itself.
		out, err := runWithTerminal(exe, env, "hosts", "sync")
		if err != nil {
			t.Fatalf("hosts sync on a terminal: %v\n%s", err, out)
		}
		if _, err := os.Stat(pending); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("pending entries still present after an elevated sync (stat err %v)", err)
		}
		report(t, "'hosts sync' on a terminal ran the privileged helper through sudo")
	}

	synced := readHostsFile(t, path)
	f, err := hosts.Parse(before)
	if err != nil {
		t.Fatal(err)
	}
	if wantSynced := f.WithEntries(want); !bytes.Equal(synced, wantSynced) {
		t.Fatalf("%s after sync =\n%q\nwant\n%q", path, synced, wantSynced)
	}
	report(t, "%s holds the doktunnel block with %d entries and keeps the existing lines", path, len(want))
	checkList(t, exe, env, want)
	if runtime.GOOS == "darwin" {
		checkAliases(t, want)
	}
	for _, e := range want {
		checkResolves(t, e)
	}

	after := withLine(synced, lineAfter, eol)
	writeSystemHosts(t, path, after)
	if runtime.GOOS == "windows" {
		out, err = runCLI(exe, env, "--no-input", "hosts", "clean")
	} else {
		out, err = runWithTerminal(exe, env, "hosts", "clean")
	}
	if err != nil {
		t.Fatalf("hosts clean: %v\n%s", err, out)
	}
	if got, wantClean := readHostsFile(t, path), withLine(before, lineAfter, eol); !bytes.Equal(got, wantClean) {
		t.Fatalf("%s after clean =\n%q\nwant\n%q", path, got, wantClean)
	}
	report(t, "'hosts clean' removed the block and kept the unrelated lines before and after it byte for byte")
}

// TestHosts_UACHelper runs the privileged helper through the UAC elevator
// that Windows users get, to learn whether Start-Process -Verb RunAs works
// unattended on a runner. "hosts sync" never reaches it there, because the
// runner process can already write the hosts file.
func TestHosts_UACHelper(t *testing.T) {
	requirePlatform(t)
	if runtime.GOOS != "windows" {
		t.Skip("UAC elevation exists only on Windows")
	}
	exe := buildDoktunnel(t)
	path := hosts.DefaultPath()
	original := readHostsFile(t, path)
	t.Cleanup(func() { restoreHosts(t, path, original) })

	entry := hosts.Entry{IP: netip.MustParseAddr("127.77.0.3"), Hostname: "uac.helper.doktunnel.platform.ci.internal"}
	entriesFile := filepath.Join(t.TempDir(), "pending-hosts")
	if err := os.WriteFile(entriesFile, hosts.FormatEntries([]hosts.Entry{entry}), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	uac := elevate.UAC{}
	if err := uac.Run(ctx, []string{exe, "hosts", "privileged-apply", "--entries-file", entriesFile}, nil); err != nil {
		finding(t, "UAC elevation (Start-Process -Verb RunAs) did not complete unattended on the runner: %v", err)
		t.Fatalf("UAC helper: %v", err)
	}
	f, err := hosts.Parse(original)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := readHostsFile(t, path), f.WithEntries([]hosts.Entry{entry}); !bytes.Equal(got, want) {
		t.Fatalf("%s after the UAC helper =\n%q\nwant\n%q", path, got, want)
	}
	report(t, "the privileged helper ran through UAC (Start-Process -Verb RunAs) unattended and wrote the block from a pending file")

	if err := uac.Run(ctx, []string{exe, "hosts", "privileged-apply", "--clean"}, nil); err != nil {
		t.Fatalf("UAC helper --clean: %v", err)
	}
	if got := readHostsFile(t, path); !bytes.Equal(got, hosts.Clean(f.WithEntries([]hosts.Entry{entry}))) {
		t.Fatalf("%s after the UAC helper --clean =\n%q", path, got)
	}
	report(t, "the privileged helper removed the block through UAC")
}

// buildDoktunnel builds the CLI into a temporary directory.
func buildDoktunnel(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "doktunnel")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	out, err := exec.Command("go", "build", "-o", exe, "github.com/alebak/dokploy-tunnel/cmd/doktunnel").CombinedOutput()
	if err != nil {
		t.Fatalf("building doktunnel: %v\n%s", err, out)
	}
	return exe
}

// cliEnv is the environment of the CLI under test: the address registry
// lives in state, and the hosts file is always the system one.
func cliEnv(state string) []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "DOKTUNNEL_HOSTS_FILE=")
	})
	if runtime.GOOS == "windows" {
		return append(env, "LOCALAPPDATA="+state)
	}
	return append(env, "XDG_STATE_HOME="+state)
}

// seedRegistry leases two named services the way forward will and returns
// the entries "hosts sync" must write for them.
func seedRegistry(t *testing.T, path string) []hosts.Entry {
	t.Helper()
	reg, err := registry.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	services := []struct {
		id    string
		names hostname.Names
	}{
		{"app_web", hostname.Names{Context: "ci", Organization: "Platform Org", Project: "doktunnel", Service: "web"}},
		{"cmp_stack/db", hostname.Names{Context: "ci", Organization: "Platform Org", Project: "doktunnel", Compose: "stack", Service: "db"}},
	}
	for _, s := range services {
		k := registry.Key{Instance: "https://dokploy.example.com", OrganizationID: "org_ci", ServiceID: s.id}
		if _, err := reg.Lease(k); err != nil {
			t.Fatal(err)
		}
		if _, err := reg.SetNames(k, s.names); err != nil {
			t.Fatal(err)
		}
	}
	leases, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	want, err := hosts.Desired(leases)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != len(services) {
		t.Fatalf("seeded %d services, got %d entries", len(services), len(want))
	}
	return want
}

// runCLI runs the CLI without a terminal and returns its combined output.
func runCLI(exe string, env []string, args ...string) (string, error) {
	cmd := exec.Command(exe, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runWithTerminal runs the CLI on a pseudo-terminal through script(1), so it
// prompts and elevates as it would for a person. Its standard input stays
// an open pipe until it exits, so script never forwards an end of file.
func runWithTerminal(exe string, env []string, args ...string) (string, error) {
	argv := append([]string{exe}, args...)
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		quoted := make([]string, len(argv))
		for i, a := range argv {
			quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		cmd = exec.Command("script", "--quiet", "--return", "--command", strings.Join(quoted, " "), "/dev/null")
	case "darwin":
		cmd = exec.Command("script", append([]string{"-q", "/dev/null"}, argv...)...)
	default:
		return "", fmt.Errorf("no pseudo-terminal runner for %s", runtime.GOOS)
	}
	cmd.Env = env
	if _, err := cmd.StdinPipe(); err != nil {
		return "", err
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func checkList(t *testing.T, exe string, env []string, want []hosts.Entry) {
	t.Helper()
	out, err := runCLI(exe, env, "--no-input", "--json", "hosts", "list")
	if err != nil {
		t.Fatalf("hosts list: %v\n%s", err, out)
	}
	var got struct {
		Entries []hosts.Entry `json:"entries"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("hosts list output %q: %v", out, err)
	}
	if !slices.Equal(got.Entries, want) {
		t.Fatalf("hosts list = %v, want %v", got.Entries, want)
	}
}

func checkAliases(t *testing.T, want []hosts.Entry) {
	t.Helper()
	for _, e := range want {
		present, err := hasLo0Alias(e.IP)
		if err != nil {
			t.Fatal(err)
		}
		if !present {
			t.Fatalf("lo0 has no alias for %s after hosts sync", e.IP)
		}
	}
	report(t, "'hosts sync' added lo0 aliases for every entry")
}

// checkResolves proves e resolves through the operating system's resolver
// tool and through Go, by connecting to a listener on e.IP by name.
func checkResolves(t *testing.T, e hosts.Entry) {
	t.Helper()
	var how string
	err := eventually(15*time.Second, func() error {
		var err error
		how, err = systemResolve(e.Hostname, e.IP.String())
		return err
	})
	if err != nil {
		t.Fatalf("resolving %s: %v", e.Hostname, err)
	}
	report(t, "%s resolves to %s through %s", e.Hostname, e.IP, how)

	ln, err := net.Listen("tcp", net.JoinHostPort(e.IP.String(), "0"))
	if err != nil {
		t.Fatalf("listening on %s: %v", e.IP, err)
	}
	defer ln.Close()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	if err := eventually(15*time.Second, func() error { return echo(ln, net.JoinHostPort(e.Hostname, port)) }); err != nil {
		t.Fatalf("connecting to %s by name: %v", e.Hostname, err)
	}
	report(t, "connected to %s:%s by name", e.Hostname, port)
}

// systemResolve looks name up with the platform's own tool and checks that
// it yields ip. It returns the tool that answered.
func systemResolve(name, ip string) (string, error) {
	var tools [][]string
	switch runtime.GOOS {
	case "linux":
		tools = [][]string{{"getent", "hosts", name}}
	case "darwin":
		tools = [][]string{{"dscacheutil", "-q", "host", "-a", "name", name}}
	case "windows":
		tools = [][]string{
			{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "(Resolve-DnsName -Name '" + name + "' -Type A -ErrorAction Stop).IPAddress"},
			{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "[System.Net.Dns]::GetHostAddresses('" + name + "') | ForEach-Object { $_.IPAddressToString }"},
		}
	}
	var errs []error
	for _, tool := range tools {
		out, err := exec.Command(tool[0], tool[1:]...).CombinedOutput()
		if err == nil && slices.Contains(strings.Fields(string(out)), ip) {
			return strings.Join(tool[:min(len(tool), 4)], " "), nil
		}
		errs = append(errs, fmt.Errorf("%s: %v: %q", strings.Join(tool, " "), err, strings.TrimSpace(string(out))))
	}
	return "", errors.Join(errs...)
}

func readHostsFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// writeSystemHosts rewrites the system hosts file in place: with sudo tee on
// Linux and macOS, directly on Windows, where the runner is an
// Administrator.
func writeSystemHosts(t *testing.T, path string, data []byte) {
	t.Helper()
	if runtime.GOOS == "windows" {
		if err := hosts.Write(path, data); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := sudo(data, "tee", path); err != nil {
		t.Fatal(err)
	}
}

// restoreHosts puts the original hosts file back if a test left it changed.
func restoreHosts(t *testing.T, path string, original []byte) {
	t.Helper()
	if b, err := os.ReadFile(path); err == nil && bytes.Equal(b, original) {
		return
	}
	writeSystemHosts(t, path, original)
}

// lineEnding is the line ending content uses, defaulting to the platform's.
func lineEnding(content []byte) string {
	if i := bytes.IndexByte(content, '\n'); i > 0 && content[i-1] == '\r' {
		return "\r\n"
	} else if i >= 0 {
		return "\n"
	}
	if runtime.GOOS == "windows" {
		return "\r\n"
	}
	return "\n"
}

// withLine returns content with line appended, ending the last line first
// if it has no line ending.
func withLine(content []byte, line, eol string) []byte {
	out := slices.Clone(content)
	if len(out) > 0 && !bytes.HasSuffix(out, []byte("\n")) {
		out = append(out, eol...)
	}
	return append(append(out, line...), eol...)
}
