package companion

import (
	"bytes"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRepeaterKey_PersistsAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "repeater.key")
	first, err := repeaterKey(path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	if len(first) != repeaterKeySize {
		t.Fatalf("key has %d bytes, want %d", len(first), repeaterKeySize)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("key file mode = %v, want 0600", perm)
		}
	}
	second, err := repeaterKey(path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("a restart generated another key")
	}
}

func TestRepeaterKey_ReadsAnAdministratorsKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repeater.key")
	want := bytes.Repeat([]byte{0xab}, repeaterKeySize)
	if err := os.WriteFile(path, []byte(hex.EncodeToString(want)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := repeaterKey(path, slog.New(slog.DiscardHandler))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("repeaterKey = %x, %v; want %x", got, err, want)
	}
}

func TestRepeaterKey_AGivenFileMustBeUsable(t *testing.T) {
	dir := t.TempDir()
	tests := map[string]string{
		"not hex":   "zz",
		"too short": hex.EncodeToString([]byte("short")),
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := repeaterKey(path, slog.New(slog.DiscardHandler)); err == nil {
				t.Error("a malformed key file was accepted")
			}
		})
	}
	t.Run("not creatable", func(t *testing.T) {
		blocker := filepath.Join(dir, "a-file")
		if err := os.WriteFile(blocker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := repeaterKey(filepath.Join(blocker, "repeater.key"), slog.New(slog.DiscardHandler)); err == nil {
			t.Error("a key file that cannot be created was accepted")
		}
	})
}

// setConfigDir points os.UserConfigDir at dir on every OS, or makes it
// fail when dir is empty.
func setConfigDir(t *testing.T, dir string) {
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("AppData", dir)
}

func TestRepeaterKey_DefaultsToTheUserConfigDir(t *testing.T) {
	setConfigDir(t, t.TempDir())
	first, err := repeaterKey("", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	second, err := repeaterKey("", slog.New(slog.DiscardHandler))
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("restart = %x, %v; want the same key %x", second, err, first)
	}
}

func TestRepeaterKey_WithoutADefaultLocationFallsBackToAProcessKey(t *testing.T) {
	setConfigDir(t, "")
	var logs bytes.Buffer
	key, err := repeaterKey("", slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("repeaterKey: %v", err)
	}
	if len(key) != repeaterKeySize {
		t.Errorf("key has %d bytes, want %d", len(key), repeaterKeySize)
	}
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("no warning about the key that does not survive a restart:\n%s", &logs)
	}
}
