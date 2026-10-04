// Package fsutil holds file system helpers shared by doktunnel's packages.
package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// WriteFileAtomic writes data to path so that a crash or a concurrent reader
// sees either the old file or the new one, never a partial write.
//
// It writes a temporary file in the same directory, flushes it to disk, sets
// perm on it explicitly (so the umask does not apply), and renames it over
// path. On Windows, os.Rename uses MoveFileEx with MOVEFILE_REPLACE_EXISTING,
// which replaces an existing file; it can still fail while another process
// holds path open without sharing delete access. Where the platform supports
// it, the directory is then flushed so the rename itself survives a crash;
// that step is best effort, since the new contents are already in place.
//
// The directory must exist. The temporary file is removed on any failure.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	// CreateTemp creates the file with mode 0600, so data is never exposed
	// more widely than perm while it is being written.
	tmp, err := os.CreateTemp(dir, "."+base+".tmp*")
	if err != nil {
		return fmt.Errorf("creating temporary file: %w", err)
	}
	defer func() {
		if err != nil {
			tmp.Close()           // may already be closed
			os.Remove(tmp.Name()) // best effort cleanup
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("writing %s: %w", tmp.Name(), err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("flushing %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmp.Name(), err)
	}
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		return fmt.Errorf("setting permissions on %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	syncDir(dir)
	return nil
}

// syncDir flushes the directory entry changes in dir. Windows cannot open a
// directory for flushing, so it is skipped there; errors are ignored because
// the caller's write has already succeeded.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	d.Sync()
	d.Close()
}
