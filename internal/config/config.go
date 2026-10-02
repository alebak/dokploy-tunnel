// Package config stores doktunnel's contexts: the Dokploy panels and
// organizations the user has registered, and which one is current.
//
// The file holds no secrets. API keys live in the OS keyring (see package
// keyring), keyed by context name.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// SchemaVersion is the config file format version. Bump it on incompatible
// changes and teach Load to migrate older versions.
const SchemaVersion = 1

var (
	// ErrCorrupt means the config file exists but cannot be trusted.
	ErrCorrupt = errors.New("config file is corrupt")
	// ErrUnsupportedVersion means the config file was written by an
	// incompatible doktunnel version.
	ErrUnsupportedVersion = errors.New("config file has an unsupported version")
	// ErrNotFound means no context has the given name.
	ErrNotFound = errors.New("context not found")
	// ErrExists means a context with the given name already exists.
	ErrExists = errors.New("context already exists")
	// ErrNoCurrent means no context was named and none is current.
	ErrNoCurrent = errors.New("no current context")
	// ErrInvalidName means a context name has a disallowed form.
	ErrInvalidName = errors.New("invalid context name")
)

// namePattern keeps names safe as keyring accounts, file names and shell
// words: ASCII letters, digits, '.', '_' and '-', not starting with a symbol.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Context is one Dokploy panel and organization.
type Context struct {
	// Name is the user-chosen alias that identifies the context.
	Name string `json:"name"`
	// URL is the panel base URL, such as https://dokploy.example.com.
	URL string `json:"url"`
	// OrganizationID is the Dokploy organization the API key is bound to.
	OrganizationID string `json:"organization_id"`
	// OrganizationName is that organization's display name when added.
	OrganizationName string `json:"organization_name"`
}

// Config is the on-disk config file.
type Config struct {
	// Version is the file format version, SchemaVersion when written.
	Version int `json:"version"`
	// Current names the context used when none is given; empty means none.
	Current string `json:"current_context,omitempty"`
	// Contexts are the registered contexts, in the order they were added.
	Contexts []Context `json:"contexts"`
}

// New returns an empty config.
func New() *Config {
	return &Config{Version: SchemaVersion, Contexts: []Context{}}
}

// DefaultPath returns the config file location: doktunnel/config.json inside
// os.UserConfigDir (XDG_CONFIG_HOME or ~/.config on Linux,
// ~/Library/Application Support on macOS, %AppData% on Windows).
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locating config directory: %w", err)
	}
	return filepath.Join(dir, "doktunnel", "config.json"), nil
}

// ValidateName reports whether name can be used as a context name.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%w %q: use up to 64 ASCII letters, digits, '.', '_' or '-', starting with a letter or digit",
			ErrInvalidName, name)
	}
	return nil
}

// Load reads the config file at path; a missing file is an empty config.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Contexts == nil {
		c.Contexts = []Context{}
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.Version != SchemaVersion {
		return fmt.Errorf("%w: got %d, want %d", ErrUnsupportedVersion, c.Version, SchemaVersion)
	}
	seen := make(map[string]bool, len(c.Contexts))
	for _, ctx := range c.Contexts {
		if err := ValidateName(ctx.Name); err != nil {
			return fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		if seen[ctx.Name] {
			return fmt.Errorf("%w: context %q appears twice", ErrCorrupt, ctx.Name)
		}
		if ctx.URL == "" {
			return fmt.Errorf("%w: context %q has no url", ErrCorrupt, ctx.Name)
		}
		seen[ctx.Name] = true
	}
	if c.Current != "" && !seen[c.Current] {
		return fmt.Errorf("%w: current context %q does not exist", ErrCorrupt, c.Current)
	}
	return nil
}

// Save writes c to path atomically: a crash leaves either the old or the new
// file, never a truncated one. The file is readable only by its owner.
func (c *Config) Save(path string) error {
	if err := c.validate(); err != nil {
		return fmt.Errorf("refusing to save config: %w", err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	b = append(b, '\n')

	dir, base := filepath.Split(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	// CreateTemp creates the file with mode 0600.
	tmp, err := os.CreateTemp(dir, "."+base+".tmp*")
	if err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("writing config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("writing config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replacing config: %w", err)
	}
	return nil
}

// Find returns the context called name.
func (c *Config) Find(name string) (Context, bool) {
	if i := c.index(name); i >= 0 {
		return c.Contexts[i], true
	}
	return Context{}, false
}

func (c *Config) index(name string) int {
	for i, ctx := range c.Contexts {
		if ctx.Name == name {
			return i
		}
	}
	return -1
}

// Add registers ctx. It fails with ErrInvalidName or ErrExists.
func (c *Config) Add(ctx Context) error {
	if err := ValidateName(ctx.Name); err != nil {
		return err
	}
	if c.index(ctx.Name) >= 0 {
		return fmt.Errorf("%w: %q", ErrExists, ctx.Name)
	}
	c.Contexts = append(c.Contexts, ctx)
	return nil
}

// Use makes the context called name current, or returns ErrNotFound.
func (c *Config) Use(name string) error {
	if c.index(name) < 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	c.Current = name
	return nil
}

// Remove deletes the context called name, or returns ErrNotFound. Removing
// the current context leaves no context current.
func (c *Config) Remove(name string) error {
	i := c.index(name)
	if i < 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	c.Contexts = append(c.Contexts[:i], c.Contexts[i+1:]...)
	if c.Current == name {
		c.Current = ""
	}
	return nil
}

// Resolve returns the context to use: the one called override when it is not
// empty, otherwise the current one. It fails with ErrNotFound or ErrNoCurrent.
func (c *Config) Resolve(override string) (Context, error) {
	name := override
	if name == "" {
		name = c.Current
	}
	if name == "" {
		return Context{}, ErrNoCurrent
	}
	ctx, ok := c.Find(name)
	if !ok {
		return Context{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return ctx, nil
}
