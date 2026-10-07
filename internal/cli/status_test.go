package cli

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/runstate"
)

// statusHarness runs "status" against a temporary state directory, where
// only the PIDs in alive are running.
type statusHarness struct {
	t            *testing.T
	registryPath string
	alive        map[int]bool
}

func newStatusHarness(t *testing.T) *statusHarness {
	t.Helper()
	return &statusHarness{
		t:            t,
		registryPath: filepath.Join(t.TempDir(), "state", "doktunnel", "addresses.json"),
		alive:        map[int]bool{},
	}
}

func (h *statusHarness) dir() string {
	return runstate.Dir(filepath.Dir(h.registryPath))
}

func (h *statusHarness) write(p runstate.Process, alive bool) string {
	h.t.Helper()
	path, err := runstate.Write(h.dir(), p)
	if err != nil {
		h.t.Fatal(err)
	}
	h.alive[p.PID] = alive
	return path
}

// writeRaw writes a file for pid with arbitrary content.
func (h *statusHarness) writeRaw(pid int, content string, alive bool) string {
	h.t.Helper()
	if err := os.MkdirAll(h.dir(), 0o700); err != nil {
		h.t.Fatal(err)
	}
	path := runstate.Path(h.dir(), pid)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
	h.alive[pid] = alive
	return path
}

func (h *statusHarness) run(args ...string) result {
	h.t.Helper()
	var stdout, stderr bytes.Buffer
	app := &App{
		Root:         NewRoot(),
		Stdin:        strings.NewReader(""),
		Stdout:       &stdout,
		Stderr:       &stderr,
		ConfigPath:   filepath.Join(filepath.Dir(h.registryPath), "config.json"),
		RegistryPath: h.registryPath,
		ProcessAlive: func(pid int) bool { return h.alive[pid] },
	}
	exit := app.Run(args)
	return result{exit: exit, stdout: stdout.String(), stderr: stderr.String()}
}

func statusProcess(pid int, context string, started time.Time, fwds ...runstate.Forward) runstate.Process {
	return runstate.Process{
		PID:          pid,
		StartedAt:    started,
		Context:      context,
		CompanionURL: "https://" + context + ".example.com/doktunnel",
		Forwards:     fwds,
	}
}

func statusForward(name, host, ip string, port int) runstate.Forward {
	return runstate.Forward{
		Target:   runstate.Target{Type: "postgres", ID: "pg_" + name, Name: name},
		Hostname: host,
		IP:       netip.MustParseAddr(ip),
		Port:     port,
	}
}

var statusStart = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func decodeStatus(t *testing.T, stdout string) statusJSON {
	t.Helper()
	var out statusJSON
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("stdout is not status JSON: %v (%q)", err, stdout)
	}
	return out
}

func TestStatus_NoForwards(t *testing.T) {
	tests := []struct {
		name  string
		setup func(h *statusHarness)
	}{
		{name: "no state directory", setup: func(*statusHarness) {}},
		{name: "empty state directory", setup: func(h *statusHarness) {
			if err := os.MkdirAll(h.dir(), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newStatusHarness(t)
			tt.setup(h)

			r := h.run("status")
			if r.exit != 0 || r.stdout != "" || r.stderr != "No active forwards.\n" {
				t.Errorf("human: exit %d, stdout %q, stderr %q; want 0, empty, \"No active forwards.\"", r.exit, r.stdout, r.stderr)
			}

			r = h.run("status", "--json")
			if r.exit != 0 || r.stderr != "" {
				t.Fatalf("json: exit %d, stderr %q", r.exit, r.stderr)
			}
			if want := `{"forwards":[],"warnings":[]}` + "\n"; r.stdout != want {
				t.Errorf("json stdout = %q, want %q", r.stdout, want)
			}
		})
	}
}

func TestStatus_JSONListsLiveForwards(t *testing.T) {
	h := newStatusHarness(t)
	h.write(statusProcess(300, "prod", statusStart,
		statusForward("main-db", "shop-maindb-a1b2c3.internal", "127.77.0.2", 5432),
		statusForward("cache", "shop-cache-g7h8i9.internal", "127.77.0.3", 6379),
	), true)
	h.write(statusProcess(200, "dev", statusStart.Add(time.Hour),
		statusForward("main-db", "shop-maindb-a1b2c3.dev.internal", "127.77.0.9", 5432),
	), true)

	r := h.run("status", "--json")
	if r.exit != 0 || r.stderr != "" {
		t.Fatalf("exit %d, stderr %q", r.exit, r.stderr)
	}
	// Sorted by context, hostname and port.
	want := `{"forwards":[` +
		`{"pid":200,"started_at":"2026-10-06T13:00:00Z","context":"dev","companion_url":"https://dev.example.com/doktunnel","target":{"type":"postgres","id":"pg_main-db","name":"main-db"},"hostname":"shop-maindb-a1b2c3.dev.internal","ip":"127.77.0.9","port":5432},` +
		`{"pid":300,"started_at":"2026-10-06T12:00:00Z","context":"prod","companion_url":"https://prod.example.com/doktunnel","target":{"type":"postgres","id":"pg_cache","name":"cache"},"hostname":"shop-cache-g7h8i9.internal","ip":"127.77.0.3","port":6379},` +
		`{"pid":300,"started_at":"2026-10-06T12:00:00Z","context":"prod","companion_url":"https://prod.example.com/doktunnel","target":{"type":"postgres","id":"pg_main-db","name":"main-db"},"hostname":"shop-maindb-a1b2c3.internal","ip":"127.77.0.2","port":5432}` +
		`],"warnings":[]}` + "\n"
	if r.stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", r.stdout, want)
	}
}

func TestStatus_HumanTable(t *testing.T) {
	h := newStatusHarness(t)
	h.write(statusProcess(300, "prod", statusStart,
		statusForward("main-db", "shop-maindb-a1b2c3.internal", "127.77.0.2", 5432),
	), true)
	h.write(statusProcess(200, "dev", statusStart,
		statusForward("myapp/postgres", "postgres.shop-myapp-x1y2z3.dev.internal", "127.77.0.9", 5432),
	), true)

	r := h.run("status")
	if r.exit != 0 || r.stderr != "" {
		t.Fatalf("exit %d, stderr %q", r.exit, r.stderr)
	}
	since := statusStart.Local().Format(time.DateTime)
	lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("stdout has %d lines, want header and 2 rows:\n%s", len(lines), r.stdout)
	}
	wantRows := [][]string{
		{"CONTEXT", "HOSTNAME", "ADDRESS", "TARGET", "PID", "SINCE"},
		append([]string{"dev", "postgres.shop-myapp-x1y2z3.dev.internal", "127.77.0.9:5432", "myapp/postgres", "200"}, strings.Fields(since)...),
		append([]string{"prod", "shop-maindb-a1b2c3.internal", "127.77.0.2:5432", "main-db", "300"}, strings.Fields(since)...),
	}
	for i, line := range lines {
		if got := strings.Fields(line); !reflect.DeepEqual(got, wantRows[i]) {
			t.Errorf("line %d = %q, want fields %q", i, line, wantRows[i])
		}
	}
	// Columns are aligned.
	if col := strings.Index(lines[0], "HOSTNAME"); strings.Index(lines[1], "postgres.") != col || strings.Index(lines[2], "shop-maindb-") != col {
		t.Errorf("HOSTNAME column is not aligned:\n%s", r.stdout)
	}
}

func TestStatus_RemovesStaleFiles(t *testing.T) {
	h := newStatusHarness(t)
	live := h.write(statusProcess(300, "prod", statusStart,
		statusForward("main-db", "shop-maindb-a1b2c3.internal", "127.77.0.2", 5432),
	), true)
	dead := h.write(statusProcess(400, "prod", statusStart,
		statusForward("cache", "shop-cache-g7h8i9.internal", "127.77.0.3", 6379),
	), false)
	deadInvalid := h.writeRaw(500, "{not json", false)

	r := h.run("status", "--json")
	if r.exit != 0 {
		t.Fatalf("exit %d, stderr %q", r.exit, r.stderr)
	}
	out := decodeStatus(t, r.stdout)
	if len(out.Forwards) != 1 || out.Forwards[0].PID != 300 {
		t.Errorf("forwards = %+v, want only PID 300's", out.Forwards)
	}
	if len(out.Warnings) != 0 {
		t.Errorf("warnings = %q, want none for stale files", out.Warnings)
	}
	for _, path := range []string{dead, deadInvalid} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("stale file %s was not removed (stat error %v)", path, err)
		}
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("live file %s: %v", live, err)
	}
}

func TestStatus_InvalidFilesOfLiveProcessesAreWarnings(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantMsg string
	}{
		{name: "malformed JSON", content: "{not json", wantMsg: "600.json"},
		{name: "other version", content: `{"version":2,"pid":600}`, wantMsg: "unsupported version"},
		{name: "other PID", content: `{"version":1,"pid":601}`, wantMsg: "holds PID 601"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newStatusHarness(t)
			h.write(statusProcess(300, "prod", statusStart,
				statusForward("main-db", "shop-maindb-a1b2c3.internal", "127.77.0.2", 5432),
			), true)
			invalid := h.writeRaw(600, tt.content, true)

			r := h.run("status", "--json")
			if r.exit != 0 || r.stderr != "" {
				t.Fatalf("json: exit %d, stderr %q", r.exit, r.stderr)
			}
			out := decodeStatus(t, r.stdout)
			if len(out.Forwards) != 1 {
				t.Errorf("forwards = %+v, want the valid one", out.Forwards)
			}
			if len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], tt.wantMsg) {
				t.Errorf("warnings = %q, want one mentioning %q", out.Warnings, tt.wantMsg)
			}
			if _, err := os.Stat(invalid); err != nil {
				t.Errorf("invalid file of a live process was removed: %v", err)
			}

			r = h.run("status")
			if r.exit != 0 || !strings.HasPrefix(r.stderr, "warning: ") || !strings.Contains(r.stderr, tt.wantMsg) {
				t.Errorf("human: exit %d, stderr %q; want 0 and a warning", r.exit, r.stderr)
			}
			if !strings.Contains(r.stdout, "shop-maindb-a1b2c3.internal") {
				t.Errorf("human stdout misses the valid forward:\n%s", r.stdout)
			}
		})
	}
}

func TestStatus_ContextFilter(t *testing.T) {
	h := newStatusHarness(t)
	h.write(statusProcess(300, "prod", statusStart,
		statusForward("main-db", "shop-maindb-a1b2c3.internal", "127.77.0.2", 5432),
	), true)
	h.write(statusProcess(200, "dev", statusStart,
		statusForward("main-db", "shop-maindb-a1b2c3.dev.internal", "127.77.0.9", 5432),
	), true)

	for _, args := range [][]string{
		{"status", "--json", "--context", "dev"},
		{"--context", "dev", "status", "--json"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			r := h.run(args...)
			if r.exit != 0 {
				t.Fatalf("exit %d, stderr %q", r.exit, r.stderr)
			}
			out := decodeStatus(t, r.stdout)
			if len(out.Forwards) != 1 || out.Forwards[0].Context != "dev" {
				t.Errorf("forwards = %+v, want only context dev", out.Forwards)
			}
		})
	}

	r := h.run("status", "--context", "staging")
	if r.exit != 0 || r.stdout != "" || r.stderr != "No active forwards.\n" {
		t.Errorf("unknown context: exit %d, stdout %q, stderr %q", r.exit, r.stdout, r.stderr)
	}
}

func TestStatus_RejectsArguments(t *testing.T) {
	h := newStatusHarness(t)
	r := h.run("status", "extra", "--json")
	if r.exit == 0 {
		t.Fatalf("exit 0, want an invalid_argument error")
	}
	if e := decodeError(t, r.stdout); e.Code != "invalid_argument" {
		t.Errorf("error = %+v, want invalid_argument", e)
	}
}

func TestStatus_DefaultLivenessUsesTheRealProcessTable(t *testing.T) {
	h := newStatusHarness(t)
	h.write(statusProcess(os.Getpid(), "prod", statusStart,
		statusForward("main-db", "shop-maindb-a1b2c3.internal", "127.77.0.2", 5432),
	), true)

	var stdout, stderr bytes.Buffer
	app := &App{
		Root:         NewRoot(),
		Stdin:        strings.NewReader(""),
		Stdout:       &stdout,
		Stderr:       &stderr,
		RegistryPath: h.registryPath,
	}
	if exit := app.Run([]string{"status", "--json"}); exit != 0 {
		t.Fatalf("exit %d, stderr %q", exit, stderr.String())
	}
	out := decodeStatus(t, stdout.String())
	if len(out.Forwards) != 1 || out.Forwards[0].PID != os.Getpid() {
		t.Errorf("forwards = %+v, want the test process's", out.Forwards)
	}
}
