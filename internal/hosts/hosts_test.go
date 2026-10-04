package hosts

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/hostname"
	"github.com/alebak/dokploy-tunnel/internal/registry"
)

const unrelated = "127.0.0.1\tlocalhost\n" +
	"::1\tlocalhost ip6-localhost\n" +
	"# a comment the user wrote\n" +
	"10.0.0.5  nas.lan   nas\n"

func entries(pairs ...string) []Entry {
	var out []Entry
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, Entry{IP: netip.MustParseAddr(pairs[i]), Hostname: pairs[i+1]})
	}
	return out
}

func block(eol string, lines ...string) string {
	all := append([]string{BeginLine}, lines...)
	all = append(all, EndLine)
	return strings.Join(all, eol) + eol
}

func mustApply(t *testing.T, content string, es []Entry) string {
	t.Helper()
	f, err := Parse([]byte(content))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return string(f.WithEntries(es))
}

func TestWithEntries(t *testing.T) {
	pg := entries("127.77.0.1", "postgres.myapp.shop.acme.prod.internal", "127.77.0.2", "web.shop.acme.prod.internal")
	tests := []struct {
		name    string
		content string
		entries []Entry
		want    string
	}{
		{
			name:    "block is appended after unrelated lines",
			content: unrelated,
			entries: pg,
			want: unrelated + block("\n",
				"127.77.0.1\tpostgres.myapp.shop.acme.prod.internal",
				"127.77.0.2\tweb.shop.acme.prod.internal"),
		},
		{
			name:    "missing final newline is completed before the block",
			content: "127.0.0.1 localhost",
			entries: pg[:1],
			want:    "127.0.0.1 localhost\n" + block("\n", "127.77.0.1\tpostgres.myapp.shop.acme.prod.internal"),
		},
		{
			name:    "existing block is replaced in place",
			content: "127.0.0.1 localhost\n" + block("\n", "127.77.0.9\told.p.o.c.internal") + "10.0.0.5 nas\n",
			entries: pg[:1],
			want:    "127.0.0.1 localhost\n" + block("\n", "127.77.0.1\tpostgres.myapp.shop.acme.prod.internal") + "10.0.0.5 nas\n",
		},
		{
			name:    "no entries removes the block and nothing else",
			content: "127.0.0.1 localhost\n" + block("\n", "127.77.0.9\told.p.o.c.internal") + "10.0.0.5 nas\n",
			entries: nil,
			want:    "127.0.0.1 localhost\n10.0.0.5 nas\n",
		},
		{
			name:    "no entries and no block leaves the file alone",
			content: unrelated,
			entries: nil,
			want:    unrelated,
		},
		{
			name:    "CRLF file gets a CRLF block and keeps its lines",
			content: strings.ReplaceAll(unrelated, "\n", "\r\n"),
			entries: pg[:1],
			want: strings.ReplaceAll(unrelated, "\n", "\r\n") +
				block("\r\n", "127.77.0.1\tpostgres.myapp.shop.acme.prod.internal"),
		},
		{
			name:    "mixed line endings outside the block are kept byte for byte",
			content: "127.0.0.1 localhost\r\n10.0.0.5 nas\n" + block("\r\n", "127.77.0.9\told.p.o.c.internal") + "# tail\n",
			entries: pg[:1],
			want:    "127.0.0.1 localhost\r\n10.0.0.5 nas\n" + block("\r\n", "127.77.0.1\tpostgres.myapp.shop.acme.prod.internal") + "# tail\n",
		},
		{
			name:    "markers are recognized with surrounding whitespace",
			content: "  " + BeginLine + "  \n127.77.0.9 old.p.o.c.internal\n\t" + EndLine + "\n",
			entries: nil,
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mustApply(t, tt.content, tt.entries)
			if got != tt.want {
				t.Errorf("WithEntries =\n%q\nwant\n%q", got, tt.want)
			}
			if again := mustApply(t, got, tt.entries); again != got {
				t.Errorf("WithEntries is not idempotent:\n%q\nthen\n%q", got, again)
			}
		})
	}
}

func TestWithEntries_EmptyFileUsesPlatformLineEnding(t *testing.T) {
	got := mustApply(t, "", entries("127.77.0.1", "db.p.o.c.internal"))
	eol := "\n"
	if runtime.GOOS == "windows" {
		eol = "\r\n"
	}
	if want := block(eol, "127.77.0.1\tdb.p.o.c.internal"); got != want {
		t.Errorf("WithEntries on an empty file = %q, want %q", got, want)
	}
}

func TestParse_MalformedMarkers(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		wantLine int
	}{
		{name: "begin without end", content: "127.0.0.1 localhost\n" + BeginLine + "\n127.77.0.1 a.p.o.c.internal\n", wantLine: 2},
		{name: "end without begin", content: "127.0.0.1 localhost\n" + EndLine + "\n", wantLine: 2},
		{name: "nested begin", content: BeginLine + "\n" + BeginLine + "\n" + EndLine + "\n", wantLine: 2},
		{name: "two blocks", content: block("\n") + block("\n"), wantLine: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.content))
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("Parse error = %v, want ErrMalformed", err)
			}
			var me *MalformedError
			if !errors.As(err, &me) || me.Line != tt.wantLine {
				t.Errorf("Parse error = %#v, want a MalformedError at line %d", err, tt.wantLine)
			}
		})
	}
}

func TestFile_Entries(t *testing.T) {
	content := unrelated + BeginLine + "\r\n" +
		"127.77.0.1\tdb.p.o.c.internal\r\n" +
		"\r\n" +
		"# a note\r\n" +
		"127.77.0.2 web.p.o.c.internal api.p.o.c.internal\r\n" +
		EndLine + "\r\n"
	f, err := Parse([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.Entries()
	if err != nil {
		t.Fatal(err)
	}
	want := entries("127.77.0.1", "db.p.o.c.internal", "127.77.0.2", "web.p.o.c.internal", "127.77.0.2", "api.p.o.c.internal")
	if !equalEntries(got, want) {
		t.Errorf("Entries = %v, want %v", got, want)
	}

	f, err = Parse([]byte(block("\n", "not-an-ip db.p.o.c.internal")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Entries(); !errors.Is(err, ErrMalformed) {
		t.Errorf("Entries with a bad line = %v, want ErrMalformed", err)
	}
}

func TestClean(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "well-formed block",
			content: "127.0.0.1 localhost\n" + block("\n", "127.77.0.1\tdb.p.o.c.internal") + "10.0.0.5 nas\n",
			want:    "127.0.0.1 localhost\n10.0.0.5 nas\n",
		},
		{
			name:    "no block",
			content: unrelated,
			want:    unrelated,
		},
		{
			name:    "dangling begin drops only the marker",
			content: "127.0.0.1 localhost\n" + BeginLine + "\n10.0.0.5 nas\n",
			want:    "127.0.0.1 localhost\n10.0.0.5 nas\n",
		},
		{
			name:    "stray end drops only the marker",
			content: "127.0.0.1 localhost\r\n" + EndLine + "\r\n10.0.0.5 nas\r\n",
			want:    "127.0.0.1 localhost\r\n10.0.0.5 nas\r\n",
		},
		{
			name:    "duplicate blocks are all removed",
			content: block("\n", "127.77.0.1\ta.p.o.c.internal") + "10.0.0.5 nas\n" + block("\n", "127.77.0.2\tb.p.o.c.internal"),
			want:    "10.0.0.5 nas\n",
		},
		{
			name:    "nested begin closes at the first end",
			content: BeginLine + "\n10.0.0.5 nas\n" + BeginLine + "\n127.77.0.1\ta.p.o.c.internal\n" + EndLine + "\n# tail\n",
			want:    "10.0.0.5 nas\n# tail\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(Clean([]byte(tt.content))); got != tt.want {
				t.Errorf("Clean =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestDesired(t *testing.T) {
	key := func(id string) registry.Key {
		return registry.Key{Instance: "https://panel.example.com", OrganizationID: "org1", ServiceID: id}
	}
	names := func(project, compose, service string) hostname.Names {
		return hostname.Names{Context: "prod", Organization: "Acme", Project: project, Compose: compose, Service: service}
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	leases := []registry.Lease{
		// Listed by address, as registry.List returns them; the older lease
		// keeps the plain name even though its address is higher.
		{Key: key("pg-new"), IP: netip.MustParseAddr("127.77.0.1"), CreatedAt: t0.Add(time.Hour), Names: names("shop", "", "db")},
		{Key: key("pg-old"), IP: netip.MustParseAddr("127.77.0.2"), CreatedAt: t0, Names: names("shop", "", "DB")},
		{Key: key("cmp/postgres"), IP: netip.MustParseAddr("127.77.0.3"), CreatedAt: t0, Names: names("shop", "myapp", "postgres")},
		{Key: key("unnamed"), IP: netip.MustParseAddr("127.77.0.4"), CreatedAt: t0},
	}
	got, err := Desired(leases)
	if err != nil {
		t.Fatalf("Desired: %v", err)
	}
	suffixed := got[0].Hostname
	if !strings.HasPrefix(suffixed, "db-") || !strings.HasSuffix(suffixed, ".shop.acme.prod.internal") {
		t.Errorf("newer colliding lease = %q, want a disambiguated db-<hash> name", suffixed)
	}
	want := entries(
		"127.77.0.1", suffixed,
		"127.77.0.2", "db.shop.acme.prod.internal",
		"127.77.0.3", "postgres.myapp.shop.acme.prod.internal",
	)
	if !equalEntries(got, want) {
		t.Errorf("Desired = %v, want %v", got, want)
	}
}

func TestParseEntries_ValidatesHelperInput(t *testing.T) {
	ok := "127.77.0.1\tdb.p.o.c.internal\n127.77.0.2 web.p.o.c.internal\n"
	got, err := ParseEntries([]byte(ok))
	if err != nil || len(got) != 2 {
		t.Fatalf("ParseEntries(valid) = %v, %v", got, err)
	}
	if round, err := ParseEntries(FormatEntries(got)); err != nil || !equalEntries(round, got) {
		t.Errorf("FormatEntries does not round-trip: %v, %v", round, err)
	}

	bad := map[string]string{
		"not loopback":       "10.0.0.5\tdb.p.o.c.internal\n",
		"IPv6":               "::1\tdb.p.o.c.internal\n",
		"not .internal":      "127.77.0.1\tdb.example.com\n",
		"unsafe hostname":    "127.77.0.1\tDB;rm.p.o.c.internal\n",
		"two names per line": "127.77.0.1\ta.p.o.c.internal b.p.o.c.internal\n",
		"duplicate hostname": "127.77.0.1\ta.p.o.c.internal\n127.77.0.2\ta.p.o.c.internal\n",
		"marker smuggled in": EndLine + "\n",
		"too large":          strings.Repeat("127.77.0.1\ta.p.o.c.internal\n", maxEntriesSize/20),
	}
	for name, content := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseEntries([]byte(content)); err == nil {
				t.Errorf("ParseEntries accepted %q", content)
			}
		})
	}
}

func TestDefaultPath(t *testing.T) {
	env := map[string]string{"SystemRoot": `D:\Win`}
	getenv := func(k string) string { return env[k] }
	if got := defaultPath("linux", getenv); got != "/etc/hosts" {
		t.Errorf("linux = %q", got)
	}
	if got := defaultPath("darwin", getenv); got != "/etc/hosts" {
		t.Errorf("darwin = %q", got)
	}
	if got, want := defaultPath("windows", getenv), `D:\Win\System32\drivers\etc\hosts`; got != want {
		t.Errorf("windows = %q, want %q", got, want)
	}
	if got, want := defaultPath("windows", func(string) string { return "" }), `C:\Windows\System32\drivers\etc\hosts`; got != want {
		t.Errorf("windows without SystemRoot = %q, want %q", got, want)
	}
}

func TestReadWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")

	got, err := Read(path)
	if err != nil || len(got) != 0 {
		t.Fatalf("Read(missing) = %q, %v; want empty, nil", got, err)
	}
	if err := os.WriteFile(path, []byte(unrelated), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("new\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new\n" {
		t.Errorf("file = %q, want %q", got, "new\n")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644 kept", info.Mode().Perm())
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".hosts.tmp*")); len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

func TestWrite_FollowsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need extra privileges on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "real-hosts")
	link := filepath.Join(dir, "hosts")
	if err := os.WriteFile(target, []byte(unrelated), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := Write(link, []byte("new\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink was replaced: %v, %v", info, err)
	}
	if got, _ := os.ReadFile(target); string(got) != "new\n" {
		t.Errorf("target = %q, want %q", got, "new\n")
	}
}

func TestWrite_PermissionDenied(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a non-root Unix user to observe permission errors")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")
	if err := os.WriteFile(path, []byte(unrelated), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if err := Write(path, []byte("new\n")); !IsPermission(err) {
		t.Errorf("Write error = %v, want a permission error", err)
	}
	if got, _ := os.ReadFile(path); string(got) != unrelated {
		t.Errorf("file changed to %q", got)
	}
}

func equalEntries(a, b []Entry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
