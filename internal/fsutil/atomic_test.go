package fsutil

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// assertOnly fails unless dir holds exactly the named entries, which proves
// no temporary file was left behind.
func assertOnly(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if fmt.Sprint(got) != fmt.Sprint(names) {
		t.Errorf("directory holds %q, want %q", got, names)
	}
}

func TestWriteFileAtomic_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	want := []byte(`{"version":1}` + "\n")

	if err := WriteFileAtomic(path, want, 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading file: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("content = %q, want %q", got, want)
	}
	assertOnly(t, dir, "state.json")
}

func TestWriteFileAtomic_Permissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits; only the read-only flag is honored")
	}
	tests := []struct {
		name string
		perm os.FileMode
	}{
		{"owner only", 0o600},
		{"world readable", 0o644},
		{"read only", 0o400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "file")
			if err := WriteFileAtomic(path, []byte("x"), tt.perm); err != nil {
				t.Fatalf("WriteFileAtomic: %v", err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			if got := info.Mode().Perm(); got != tt.perm {
				t.Errorf("mode = %v, want %v", got, tt.perm)
			}
		})
	}
}

func TestWriteFileAtomic_ReplacesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("old content that is longer than the new one"), 0o600); err != nil {
		t.Fatalf("seeding file: %v", err)
	}

	if err := WriteFileAtomic(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading file: %v", err)
	}
	if string(got) != "new" {
		t.Errorf("content = %q, want %q", got, "new")
	}
	assertOnly(t, dir, "state.json")
}

func TestWriteFileAtomic_Failure(t *testing.T) {
	tests := []struct {
		name string
		// setup prepares dir and returns the path to write.
		setup func(t *testing.T, dir string) string
		// left lists what dir must hold afterwards.
		left []string
	}{
		{
			// The temp file is created and written, then the rename fails
			// because the target is a non-empty directory.
			name: "rename over a directory",
			setup: func(t *testing.T, dir string) string {
				target := filepath.Join(dir, "target")
				if err := os.MkdirAll(filepath.Join(target, "child"), 0o700); err != nil {
					t.Fatal(err)
				}
				return target
			},
			left: []string{"target"},
		},
		{
			name: "missing directory",
			setup: func(t *testing.T, dir string) string {
				return filepath.Join(dir, "missing", "file")
			},
			left: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := tt.setup(t, dir)
			if err := WriteFileAtomic(path, []byte("data"), 0o600); err == nil {
				t.Fatal("WriteFileAtomic succeeded, want an error")
			}
			assertOnly(t, dir, tt.left...)
		})
	}
}

// TestWriteFileAtomic_ConcurrentWriters checks that racing writers never
// leave a mix of two payloads: the file always holds one complete payload.
func TestWriteFileAtomic_ConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	const writers = 16
	payloads := make([][]byte, writers)
	for i := range payloads {
		payloads[i] = bytes.Repeat([]byte{byte('a' + i)}, 64<<10)
	}

	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = WriteFileAtomic(path, payloads[i], 0o600)
		}()
	}
	wg.Wait()

	succeeded := 0
	for i, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case runtime.GOOS == "windows":
			// Windows may refuse to replace a file another rename is
			// replacing at the same instant; callers serialize writers
			// with a lock. Atomicity is what is checked here.
			t.Logf("writer %d: %v", i, err)
		default:
			t.Errorf("writer %d: %v", i, err)
		}
	}
	if succeeded == 0 {
		t.Fatal("no writer succeeded")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading file: %v", err)
	}
	found := false
	for _, p := range payloads {
		if bytes.Equal(got, p) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("file holds %d bytes matching no single payload", len(got))
	}
	assertOnly(t, dir, "state.json")
}
