package registry

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"runtime"
)

// fileName is the lease file name inside the doktunnel state directory.
const fileName = "addresses.json"

// DefaultPath returns the lease file location: $XDG_STATE_HOME/doktunnel or
// ~/.local/state/doktunnel on Linux and macOS, %LOCALAPPDATA%\doktunnel on
// Windows.
func DefaultPath() (string, error) {
	dir, err := stateDir(runtime.GOOS, os.Getenv, os.UserHomeDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fileName), nil
}

func stateDir(goos string, getenv func(string) string, home func() (string, error)) (string, error) {
	if goos == "windows" {
		local := getenv("LOCALAPPDATA")
		if local == "" {
			return "", errors.New("locating state directory: %LOCALAPPDATA% is not set")
		}
		return filepath.Join(local, "doktunnel"), nil
	}
	// The XDG spec says relative paths are invalid and must be ignored.
	if xdg := getenv("XDG_STATE_HOME"); path.IsAbs(xdg) {
		return filepath.Join(xdg, "doktunnel"), nil
	}
	h, err := home()
	if err != nil {
		return "", errors.Join(errors.New("locating state directory"), err)
	}
	return filepath.Join(h, ".local", "state", "doktunnel"), nil
}
