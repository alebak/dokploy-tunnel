package hosts

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// DefaultPath returns the system hosts file: /etc/hosts on Linux and macOS,
// %SystemRoot%\System32\drivers\etc\hosts on Windows.
func DefaultPath() string {
	return defaultPath(runtime.GOOS, os.Getenv)
}

func defaultPath(goos string, getenv func(string) string) string {
	if goos != "windows" {
		return "/etc/hosts"
	}
	root := getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	// Built by hand: filepath.Join would use the host's separator in tests.
	return root + `\System32\drivers\etc\hosts`
}

// Read returns the contents of the hosts file at path; a missing file reads
// as empty.
func Read(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading hosts file: %w", err)
	}
	return b, nil
}

// IsPermission reports whether err means the hosts file needs elevated
// privileges to change.
func IsPermission(err error) bool {
	return errors.Is(err, fs.ErrPermission)
}

// Write replaces the hosts file at path with data, keeping its permissions.
//
// The new contents go to a temporary file in the same directory that is then
// renamed over the old one, so readers see either the old or the new file.
// When the rename is impossible, as for a hosts file bind-mounted into a
// container or held open on Windows, the file is rewritten in place. A
// symbolic link at path is followed, so the link itself survives.
func Write(path string, data []byte) error {
	real, err := filepath.EvalSymlinks(path)
	switch {
	case err == nil:
		path = real
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("resolving hosts file: %w", err)
	}
	mode := fs.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		// The directory may be read-only while the file is writable.
		return writeInPlace(path, data, mode)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := writeTemp(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		if ierr := writeInPlace(path, data, mode); ierr != nil {
			return errors.Join(fmt.Errorf("replacing hosts file: %w", err), ierr)
		}
	}
	return nil
}

func writeTemp(tmp *os.File, data []byte, mode fs.FileMode) error {
	_, err := tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("writing hosts file: %w", err)
	}
	return nil
}

func writeInPlace(path string, data []byte, mode fs.FileMode) error {
	if err := os.WriteFile(path, data, mode); err != nil {
		return fmt.Errorf("writing hosts file: %w", err)
	}
	return nil
}
