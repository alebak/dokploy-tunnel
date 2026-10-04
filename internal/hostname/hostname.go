// Package hostname derives the stable `.internal` hostnames doktunnel writes
// to the hosts file for forwarded services.
//
// A Dokploy service is named <service>.<project>.<org>.<context>.internal and
// a service inside a compose stack <service>.<compose>.<project>.<org>.<context>.internal.
// Every label is built from a display name by Label, so it is DNS-safe, and
// Assign guarantees that two targets never share a hostname, even when their
// names only differ in characters that sanitization drops.
package hostname

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// TLD is the top-level domain of every hostname. ICANN reserves .internal
// for private use, so it never resolves on the public internet.
const TLD = "internal"

const (
	// maxLabel is the longest DNS label (RFC 1035).
	maxLabel = 63
	// maxName is the longest hostname, without the trailing dot.
	maxName = 253
	// shortLabel caps every label when the full-length labels would exceed
	// maxName: five labels of 40 bytes, their dots and the TLD fit in 213.
	shortLabel = 40
	// labelHashLen is the length of the hash that keeps cut labels distinct.
	labelHashLen = 8
)

// suffixLens are the lengths of the ID hash tried, in order, to disambiguate
// colliding hostnames.
var suffixLens = []int{6, 8, 10, 12, 16}

// ErrIncomplete means a required display name is empty.
var ErrIncomplete = errors.New("hostname needs context, organization, project and service names")

// Names are the display names a hostname is built from.
type Names struct {
	// Context is the doktunnel context name.
	Context string `json:"context"`
	// Organization is the Dokploy organization name.
	Organization string `json:"organization"`
	// Project is the Dokploy project name.
	Project string `json:"project"`
	// Compose is the compose stack name for a service inside a compose
	// stack; it is empty for Dokploy services.
	Compose string `json:"compose,omitempty"`
	// Service is the Dokploy service name, or the service name in the
	// compose file.
	Service string `json:"service"`
}

// Hostname returns the plain hostname for n, without collision handling;
// use Assign to name several targets at once.
func (n Names) Hostname() (string, error) {
	return n.hostname("")
}

// hostname builds the hostname for n, appending suffix to the service label
// when it is not empty. Labels are cut shorter when the full-length name
// would exceed maxName.
func (n Names) hostname(suffix string) (string, error) {
	if n.Context == "" || n.Organization == "" || n.Project == "" || n.Service == "" {
		return "", fmt.Errorf("%w: %+v", ErrIncomplete, n)
	}
	for _, max := range []int{maxLabel, shortLabel} {
		labels := []string{labelMax(n.Service, max)}
		if suffix != "" {
			labels[0] = withSuffix(labels[0], suffix, max)
		}
		if n.Compose != "" {
			labels = append(labels, labelMax(n.Compose, max))
		}
		labels = append(labels, labelMax(n.Project, max), labelMax(n.Organization, max), labelMax(n.Context, max), TLD)
		if host := strings.Join(labels, "."); len(host) <= maxName {
			return host, nil
		}
	}
	// Unreachable: shortLabel guarantees the second attempt fits.
	return "", fmt.Errorf("hostname for %+v exceeds %d bytes", n, maxName)
}

// Label turns a display name into a DNS label: lowercase ASCII letters,
// digits and single hyphens, at most 63 bytes, with no leading or trailing
// hyphen. Every other character, including dots, becomes a hyphen. A name
// longer than 63 bytes is cut and ends in a hash of the whole name, so long
// names that share a prefix stay distinct; a name with nothing usable, such
// as one written only in non-Latin script, becomes "x-" and a hash of it.
func Label(name string) string {
	return labelMax(name, maxLabel)
}

func labelMax(name string, max int) string {
	var b strings.Builder
	hyphen := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			hyphen = false
			continue
		}
		if !hyphen && b.Len() > 0 {
			b.WriteByte('-')
			hyphen = true
		}
	}
	label := strings.TrimRight(b.String(), "-")
	if label == "" {
		return "x-" + shortHash(name, labelHashLen)
	}
	if len(label) > max {
		return withSuffix(label, shortHash(name, labelHashLen), max)
	}
	return label
}

// withSuffix appends "-" and suffix to label, cutting label so the result
// stays within max bytes.
func withSuffix(label, suffix string, max int) string {
	if keep := max - len(suffix) - 1; len(label) > keep {
		label = strings.TrimRight(label[:keep], "-")
	}
	return label + "-" + suffix
}

// shortHash returns the first n hex digits of the SHA-256 of s.
func shortHash(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:n]
}

// Valid reports whether host is a DNS-safe hostname under TLD, as Label and
// Assign produce them.
func Valid(host string) bool {
	if len(host) > maxName || !strings.HasSuffix(host, "."+TLD) {
		return false
	}
	for _, l := range strings.Split(host, ".") {
		if !validLabel(l) {
			return false
		}
	}
	return true
}

func validLabel(l string) bool {
	if l == "" || len(l) > maxLabel || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// Target is one service to name.
type Target struct {
	// ID identifies the target uniquely and stably, such as its registry
	// key; it seeds the disambiguation suffix.
	ID string
	// Names are the display names the hostname is built from.
	Names Names
}

// Assign returns the hostname of every target, keyed by ID, and never maps
// two targets to the same hostname.
//
// Targets come in priority order, oldest registration first. Each target
// gets its plain hostname unless an earlier target already claimed it; plain
// names are claimed before any suffix is assigned, so a disambiguated name
// never takes another target's plain name. A target that loses its plain
// name gets "-" and the first 6 hex digits of the SHA-256 of its ID appended
// to the service label, with more digits when that is taken too. The result
// depends only on the targets and their order, and an existing hostname
// stays unchanged when a newer target with the same name is added.
func Assign(targets []Target) (map[string]string, error) {
	byID := make(map[string]string, len(targets))
	owner := make(map[string]string, len(targets))
	var losers []Target
	for _, t := range targets {
		if _, dup := byID[t.ID]; dup {
			return nil, fmt.Errorf("duplicate hostname target ID %q", t.ID)
		}
		host, err := t.Names.Hostname()
		if err != nil {
			return nil, err
		}
		byID[t.ID] = ""
		if _, taken := owner[host]; taken {
			losers = append(losers, t)
			continue
		}
		owner[host], byID[t.ID] = t.ID, host
	}
	for _, t := range losers {
		for _, n := range suffixLens {
			host, err := t.Names.hostname(shortHash(t.ID, n))
			if err != nil {
				return nil, err
			}
			if _, taken := owner[host]; !taken {
				owner[host], byID[t.ID] = t.ID, host
				break
			}
		}
		if byID[t.ID] == "" {
			return nil, fmt.Errorf("cannot find a unique hostname for target %q", t.ID)
		}
	}
	return byID, nil
}
