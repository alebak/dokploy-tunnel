package registry

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Key identifies one Dokploy service across every instance and organization
// the user works with. Display names are deliberately absent: renaming a
// service in Dokploy keeps its ID, so it keeps its address.
type Key struct {
	// Instance is the Dokploy base URL. Only scheme, host and a non-default
	// port are significant; see NormalizeInstanceURL.
	Instance string
	// OrganizationID is the Dokploy organization ID.
	OrganizationID string
	// ServiceID is the Dokploy ID of the application, database or compose
	// service, or "<compose ID>/<service>" for a service inside a compose
	// stack, so each of those keeps its own address.
	ServiceID string
}

// normalize validates k and returns it with a canonical Instance.
func (k Key) normalize() (Key, error) {
	instance, err := NormalizeInstanceURL(k.Instance)
	if err != nil {
		return Key{}, err
	}
	if k.OrganizationID == "" {
		return Key{}, fmt.Errorf("%w: organization ID is empty", ErrInvalidKey)
	}
	if k.ServiceID == "" {
		return Key{}, fmt.Errorf("%w: service ID is empty", ErrInvalidKey)
	}
	k.Instance = instance
	return k, nil
}

// NormalizeInstanceURL reduces a Dokploy URL to lowercase scheme://host[:port],
// dropping a default port (80 for http, 443 for https), any path, query,
// fragment and credentials. Only http and https are accepted.
func NormalizeInstanceURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("%w: instance URL %q: %v", ErrInvalidKey, raw, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("%w: instance URL %q must start with http:// or https://", ErrInvalidKey, raw)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("%w: instance URL %q has no host", ErrInvalidKey, raw)
	}
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	switch {
	case port != "":
		host = net.JoinHostPort(host, port)
	case strings.Contains(host, ":"):
		host = "[" + host + "]"
	}
	return scheme + "://" + host, nil
}
