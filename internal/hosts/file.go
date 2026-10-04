package hosts

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
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

// Write replaces the contents of the hosts file at path with data, keeping
// its permissions. A symbolic link at path is followed, so the link itself
// survives; a missing file is created.
//
// The file is rewritten in place: opened for writing, truncated, written and
// synced. A temporary file renamed over it would be atomic, but would be a
// new file without the SELinux label, extended attributes and Windows ACL of
// the system hosts file, so Write does what most tools that edit the hosts
// file do. The trade-off is that the write is not atomic: a reader may see a
// truncated file for a moment, and an I/O error midway leaves it truncated.
// Callers therefore compute and validate data completely before calling
// Write, so no validation error can happen after the file is truncated.
func Write(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("writing hosts file: %w", err)
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("writing hosts file: %w", err)
	}
	return nil
}

// ReadEntriesFile reads and validates the entries in the file at path.
//
// The privileged helper reads its entries from standard input wherever it
// can: sudo passes stdin through and prompts on the terminal, so root never
// opens a path its caller chose. A UAC prompt cannot pass a standard input
// to the elevated process, so on Windows the entries travel in a file in the
// user's own state directory instead, and the helper, running as
// Administrator, must not be turned against other files: a symbolic link,
// device or any other non-regular file is refused, the file must still be
// the one checked when it is opened, and at most maxEntriesSize+1 bytes are
// read. ParseEntries never echoes what it read.
func ReadEntriesFile(path string) ([]Entry, error) {
	checked, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("reading entries: %w", err)
	}
	if !checked.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrMalformed, path)
	}
	f, err := openNoFollow(path)
	if err != nil {
		return nil, fmt.Errorf("reading entries: %w", err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("reading entries: %w", err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(checked, opened) {
		return nil, fmt.Errorf("%w: %s changed while it was opened", ErrMalformed, path)
	}
	return ReadEntries(f)
}
