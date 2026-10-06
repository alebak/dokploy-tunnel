package companion

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// repeaterKeySize is the size of the repeater ownership key, in bytes.
const repeaterKeySize = 32

// repeaterKey returns the key that proves which repeaters this companion
// created (repeater.Options.Key). It is kept in the file path, or by
// default in the user's configuration directory, and generated there on
// first start, so that a restarted companion still reaps the repeaters it
// left behind.
//
// A file given by path must be usable. When the default location is not,
// the companion falls back to a key of its own process and warns: it then
// keeps, rather than reaps, the repeaters of earlier runs.
func repeaterKey(path string, log *slog.Logger) ([]byte, error) {
	if path != "" {
		key, err := loadOrCreateKey(path)
		if err != nil {
			return nil, fmt.Errorf("repeater key %s: %w", path, err)
		}
		return key, nil
	}
	dir, err := os.UserConfigDir()
	if err == nil {
		path = filepath.Join(dir, "doktunnel-companion", "repeater.key")
		var key []byte
		if key, err = loadOrCreateKey(path); err == nil {
			return key, nil
		}
	}
	log.Warn("no persistent repeater key: repeaters left behind before a restart will be kept, not reaped; set --repeater-key-file",
		"error", err)
	key := make([]byte, repeaterKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generating a repeater key: %w", err)
	}
	return key, nil
}

// loadOrCreateKey reads the hex key in path, or creates path with a new
// random key, readable by its owner only, when it does not exist.
func loadOrCreateKey(path string) ([]byte, error) {
	key, err := readKey(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return key, err
	}
	// Another process may create it first: either way, read it back.
	if err := createKey(path); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	return readKey(path)
}

// readKey reads the hex key in path.
func readKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(string(bytes.TrimSpace(b)))
	if err != nil {
		return nil, fmt.Errorf("not a hex key: %w", err)
	}
	if len(key) < repeaterKeySize {
		return nil, fmt.Errorf("the key has %d bytes, want at least %d", len(key), repeaterKeySize)
	}
	return key, nil
}

// createKey creates path, which must not exist, with a new random key.
func createKey(path string) error {
	key := make([]byte, repeaterKeySize)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("generating: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(hex.EncodeToString(key) + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("writing: %w", err)
	}
	return nil
}
