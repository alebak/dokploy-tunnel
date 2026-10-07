// Package runstate records the forwards a running "doktunnel forward"
// process serves, so other doktunnel processes, such as "doktunnel status",
// can list them.
//
// Every forward process writes one file, <dir>/<pid>.json, where dir is
// Dir of the doktunnel state directory, and removes it when it exits. The
// files also decide which hostnames "doktunnel hosts sync" writes. A
// process that is killed cannot remove its file, so readers must check that
// the PID is still alive and treat files of dead processes as stale.
package runstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/fsutil"
)

// Version is the format version of the files Write produces. Bump it on
// incompatible changes.
const Version = 1

// dirName is the directory, inside the state directory, that holds the
// files.
const dirName = "forwards"

// fileExt is the extension of every file.
const fileExt = ".json"

// ErrUnsupportedVersion means a file was written by an incompatible
// doktunnel version.
var ErrUnsupportedVersion = errors.New("forward state file has an unsupported version")

// Process is what one forward process serves.
type Process struct {
	// Version is the file format version; Write sets it to Version.
	Version int `json:"version"`
	// PID is the process ID; the file is named after it.
	PID int `json:"pid"`
	// StartedAt is when the process recorded its forwards, right before it
	// synced the hosts file and started listening.
	StartedAt time.Time `json:"started_at"`
	// Context is the doktunnel context the forwards belong to.
	Context string `json:"context"`
	// CompanionURL is the companion the tunnels are opened through.
	CompanionURL string `json:"companion_url"`
	// Forwards are the local listeners, one per target and port.
	Forwards []Forward `json:"forwards"`
}

// Forward is one local listener.
type Forward struct {
	// Target is the service the listener forwards to.
	Target Target `json:"target"`
	// Hostname is the target's hostname in the hosts file.
	Hostname string `json:"hostname"`
	// IP is the leased loopback address the listener is bound to.
	IP netip.Addr `json:"ip"`
	// Port is both the local port and the container port.
	Port int `json:"port"`
}

// Target identifies a forwarded service.
type Target struct {
	// Type is the Dokploy service type, or "compose_service" for a service
	// inside a compose stack.
	Type string `json:"type"`
	// ID is the Dokploy service ID, or "<compose ID>/<service>".
	ID string `json:"id"`
	// Name is the display name, or "<compose name>/<service>".
	Name string `json:"name"`
}

// Entry is one file found by List.
type Entry struct {
	// PID is the process ID the file is named after.
	PID int
	// Path is the file.
	Path string
	// Process is the file's content when Err is nil.
	Process Process
	// Err tells why the file could not be read; such a file is usually
	// left behind by a crash, or written by another doktunnel version.
	Err error
}

// Dir returns the directory of the files inside the doktunnel state
// directory stateDir.
func Dir(stateDir string) string {
	return filepath.Join(stateDir, dirName)
}

// Path returns the file of the process pid in dir.
func Path(dir string, pid int) string {
	return filepath.Join(dir, strconv.Itoa(pid)+fileExt)
}

// Write records p in dir, creating dir when needed, and returns the file
// written. The file is replaced atomically, so readers never see a partial
// one, and only the user can read it.
func Write(dir string, p Process) (string, error) {
	p.Version = Version
	data, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("encoding forward state: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("writing forward state: %w", err)
	}
	path := Path(dir, p.PID)
	if err := fsutil.WriteFileAtomic(path, append(data, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("writing forward state: %w", err)
	}
	return path, nil
}

// Remove deletes the file of the process pid in dir. A missing file is not
// an error.
func Remove(dir string, pid int) error {
	if err := os.Remove(Path(dir, pid)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing forward state: %w", err)
	}
	return nil
}

// Read reads the file at path.
func Read(path string) (Process, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Process{}, fmt.Errorf("reading forward state: %w", err)
	}
	var p Process
	if err := json.Unmarshal(data, &p); err != nil {
		return Process{}, fmt.Errorf("reading forward state %s: %w", path, err)
	}
	if p.Version != Version {
		return Process{}, fmt.Errorf("%w: %s has version %d, want %d", ErrUnsupportedVersion, path, p.Version, Version)
	}
	return p, nil
}

// List returns the files in dir, ordered by PID. A missing dir holds none.
// Files that are not named <pid>.json are ignored; a file that cannot be
// read, or that holds another PID than its name, is returned with Err set.
func List(dir string) ([]Entry, error) {
	des, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing forward state: %w", err)
	}
	var entries []Entry
	for _, de := range des {
		name, ok := strings.CutSuffix(de.Name(), fileExt)
		if !ok || !de.Type().IsRegular() {
			continue
		}
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 0 || strconv.Itoa(pid) != name {
			continue
		}
		e := Entry{PID: pid, Path: filepath.Join(dir, de.Name())}
		e.Process, e.Err = Read(e.Path)
		if e.Err == nil && e.Process.PID != pid {
			e.Err = fmt.Errorf("forward state %s holds PID %d", e.Path, e.Process.PID)
			e.Process = Process{}
		}
		entries = append(entries, e)
	}
	slices.SortFunc(entries, func(a, b Entry) int { return a.PID - b.PID })
	return entries, nil
}
