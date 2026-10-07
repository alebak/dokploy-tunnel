package runstate

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func sampleProcess(pid int) Process {
	return Process{
		PID:          pid,
		StartedAt:    time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
		Context:      "prod",
		CompanionURL: "https://panel.example.com/doktunnel",
		Forwards: []Forward{{
			Target:   Target{Type: "compose_service", ID: "cmp_myapp/postgres", Name: "myapp/postgres"},
			Hostname: "postgres.myapp.shop.acme.prod.internal",
			IP:       netip.MustParseAddr("127.77.0.1"),
			Port:     5432,
		}},
	}
}

func TestWriteRead_RoundTrip(t *testing.T) {
	dir := Dir(t.TempDir())
	p := sampleProcess(4242)

	path, err := Write(dir, p)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if want := filepath.Join(dir, "4242.json"); path != want {
		t.Errorf("Write() path = %q, want %q", path, want)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	p.Version = Version
	if !reflect.DeepEqual(got, p) {
		t.Errorf("Read() = %+v, want %+v", got, p)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("file mode = %o, want 600", perm)
		}
	}
}

func TestWrite_Format(t *testing.T) {
	dir := Dir(t.TempDir())
	path, err := Write(dir, sampleProcess(7))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":1,"pid":7,"started_at":"2026-10-06T12:00:00Z","context":"prod",` +
		`"companion_url":"https://panel.example.com/doktunnel","forwards":[{"target":{"type":"compose_service",` +
		`"id":"cmp_myapp/postgres","name":"myapp/postgres"},"hostname":"postgres.myapp.shop.acme.prod.internal",` +
		`"ip":"127.77.0.1","port":5432}]}` + "\n"
	if string(b) != want {
		t.Errorf("file =\n%s\nwant\n%s", b, want)
	}
}

func TestRemove(t *testing.T) {
	dir := Dir(t.TempDir())
	if _, err := Write(dir, sampleProcess(7)); err != nil {
		t.Fatal(err)
	}
	if err := Remove(dir, 7); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := os.Stat(Path(dir, 7)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file still exists after Remove: %v", err)
	}
	if err := Remove(dir, 7); err != nil {
		t.Errorf("Remove() of a missing file error = %v, want nil", err)
	}
}

func TestList(t *testing.T) {
	dir := Dir(t.TempDir())
	if got, err := List(dir); err != nil || len(got) != 0 {
		t.Fatalf("List() of a missing directory = %v, %v; want nothing", got, err)
	}
	for _, pid := range []int{30, 4} {
		if _, err := Write(dir, sampleProcess(pid)); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{
		"9.json":      "{not json",
		"11.json":     `{"version":99,"pid":11}`,
		"12.json":     `{"version":1,"pid":13}`,
		"notes.txt":   "ignored",
		"abc.json":    "{}",
		"5.json.tmp1": "{}",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := List(dir)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	var pids []int
	var bad []string
	for _, e := range got {
		pids = append(pids, e.PID)
		if e.Err != nil {
			bad = append(bad, filepath.Base(e.Path))
			continue
		}
		if e.Process.PID != e.PID {
			t.Errorf("entry %d holds PID %d", e.PID, e.Process.PID)
		}
	}
	if want := []int{4, 9, 11, 12, 30}; !reflect.DeepEqual(pids, want) {
		t.Errorf("List() PIDs = %v, want %v (sorted, only <pid>.json files)", pids, want)
	}
	if want := []string{"9.json", "11.json", "12.json"}; !reflect.DeepEqual(bad, want) {
		t.Errorf("unreadable entries = %v, want %v", bad, want)
	}
	for _, e := range got {
		if e.PID == 11 && !errors.Is(e.Err, ErrUnsupportedVersion) {
			t.Errorf("version 99 error = %v, want ErrUnsupportedVersion", e.Err)
		}
		if e.PID == 12 && (e.Err == nil || !strings.Contains(e.Err.Error(), "PID")) {
			t.Errorf("PID mismatch error = %v, want one naming the PID", e.Err)
		}
	}
}
