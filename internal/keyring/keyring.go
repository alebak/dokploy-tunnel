// Package keyring stores Dokploy API keys in the operating system's credential
// store: the Secret Service on Linux, the Keychain on macOS and the Credential
// Manager on Windows. doktunnel never writes API keys to its own files.
package keyring

import (
	"errors"
	"fmt"
	"maps"
	"sync"

	gokeyring "github.com/zalando/go-keyring"
)

// service groups doktunnel's entries in the OS credential store.
const service = "doktunnel"

// ErrNotFound means no secret is stored under the given name.
var ErrNotFound = errors.New("secret not found in the keyring")

// Keyring stores one secret per name.
type Keyring interface {
	// Get returns the secret stored under name, or ErrNotFound.
	Get(name string) (string, error)
	// Set stores secret under name, replacing any previous value.
	Set(name, secret string) error
	// Delete removes the secret stored under name, or returns ErrNotFound.
	Delete(name string) error
}

// System returns the OS keyring. Entries use the service "doktunnel" and the
// given name as the account.
func System() Keyring {
	return system{}
}

type system struct{}

func (system) Get(name string) (string, error) {
	secret, err := gokeyring.Get(service, name)
	if err != nil {
		return "", wrap("reading", name, err)
	}
	return secret, nil
}

func (system) Set(name, secret string) error {
	if err := gokeyring.Set(service, name, secret); err != nil {
		return wrap("storing", name, err)
	}
	return nil
}

func (system) Delete(name string) error {
	if err := gokeyring.Delete(service, name); err != nil {
		return wrap("deleting", name, err)
	}
	return nil
}

func wrap(action, name string, err error) error {
	if errors.Is(err, gokeyring.ErrNotFound) {
		return fmt.Errorf("%s secret %q: %w", action, name, ErrNotFound)
	}
	return fmt.Errorf("%s secret %q in the OS keyring: %w", action, name, err)
}

// Memory is an in-memory Keyring for tests. The zero value is not usable; use
// NewMemory.
type Memory struct {
	mu      sync.Mutex
	secrets map[string]string
	// Fail, when set, makes every operation return it, to simulate a locked
	// or missing OS keyring.
	Fail error
}

// NewMemory returns an empty in-memory keyring.
func NewMemory() *Memory {
	return &Memory{secrets: make(map[string]string)}
}

// Get implements Keyring.
func (m *Memory) Get(name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return "", m.Fail
	}
	secret, ok := m.secrets[name]
	if !ok {
		return "", ErrNotFound
	}
	return secret, nil
}

// Set implements Keyring.
func (m *Memory) Set(name, secret string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	m.secrets[name] = secret
	return nil
}

// Delete implements Keyring.
func (m *Memory) Delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if _, ok := m.secrets[name]; !ok {
		return ErrNotFound
	}
	delete(m.secrets, name)
	return nil
}

// Secrets returns a copy of the stored secrets, keyed by name.
func (m *Memory) Secrets() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.secrets)
}
