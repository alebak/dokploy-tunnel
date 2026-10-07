// Package hostname derives the stable `.internal` hostnames doktunnel writes
// to the hosts file for forwarded services.
//
// An application or database is named <appName>.internal, and a service
// inside a compose stack <service>.<appName>.internal, where appName is the
// name Dokploy deploys the service or the stack under, such as
// acme-billing-x1y2z3. Every label is built by Label, so it is DNS-safe, and
// Assign guarantees that two targets never share a hostname: a target whose
// plain name is already taken gets its doktunnel context as an extra label,
// <appName>.<context>.internal, and a hash of its ID when that is taken too.
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
	// maxName is the longest hostname, without the trailing dot. Three
	// labels of maxLabel bytes, their dots and the TLD stay within it.
	maxName = 253
	// labelHashLen is the length of the hash that keeps cut labels distinct.
	labelHashLen = 8
)

// suffixLens are the lengths of the ID hash tried, in order, to disambiguate
// colliding hostnames.
var suffixLens = []int{6, 8, 10, 12, 16}

// reserved are names other software resolves under .internal, such as
// host.docker.internal and metadata.google.internal. doktunnel never claims
// them, nor any name below them, so its hosts file entries cannot shadow
// them.
var reserved = []string{
	"docker." + TLD,
	"containers." + TLD,
	"google." + TLD,
	"lima." + TLD,
	"orb." + TLD,
	"rancher-desktop." + TLD,
}

// ErrIncomplete means a required name is empty.
var ErrIncomplete = errors.New("hostname needs a context name and an appName")

// Names are what a hostname is built from.
type Names struct {
	// Context is the doktunnel context name. It only appears in a hostname
	// whose plain form another target already holds.
	Context string `json:"context"`
	// AppName is the name Dokploy deploys the application, database or
	// compose stack under, or its Dokploy ID when the appName is unknown.
	AppName string `json:"app_name"`
	// ComposeService is the service name in the compose file for a service
	// inside a compose stack; it is empty for applications and databases.
	ComposeService string `json:"compose_service,omitempty"`
}

// Hostname returns the plain hostname for n, without collision handling;
// use Assign to name several targets at once.
func (n Names) Hostname() (string, error) {
	return n.hostname(false, "")
}

// hostname builds the hostname for n, with the context label when
// withContext is set, and suffix appended to the first label when it is not
// empty.
func (n Names) hostname(withContext bool, suffix string) (string, error) {
	if n.Context == "" || n.AppName == "" {
		return "", fmt.Errorf("%w: %+v", ErrIncomplete, n)
	}
	var labels []string
	if n.ComposeService != "" {
		labels = append(labels, Label(n.ComposeService))
	}
	labels = append(labels, Label(n.AppName))
	if withContext {
		labels = append(labels, Label(n.Context))
	}
	if suffix != "" {
		labels[0] = withSuffix(labels[0], suffix, maxLabel)
	}
	return strings.Join(append(labels, TLD), "."), nil
}

// Label turns a name, such as an appName or a compose service name, into a DNS label: lowercase ASCII letters,
// digits and single hyphens, at most 63 bytes, with no leading or trailing
// hyphen. Every other character, including the dots and underscores an\n// appName may hold, becomes a hyphen. A name
// longer than 63 bytes is cut and ends in a hash of the whole name, so long
// names that share a prefix stay distinct; a name with nothing usable, such
// as one written only in non-Latin script, becomes "x-" and a hash of it.
func Label(name string) string {
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
	if len(label) > maxLabel {
		return withSuffix(label, shortHash(name, labelHashLen), maxLabel)
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

// Reserved reports whether host is, or lies below, a name other software
// resolves under .internal, such as host.docker.internal. Assign never
// returns such a name.
func Reserved(host string) bool {
	for _, r := range reserved {
		if host == r || strings.HasSuffix(host, "."+r) {
			return true
		}
	}
	return false
}

// Target is one service to name.
type Target struct {
	// ID identifies the target uniquely and stably, such as its registry
	// key; it seeds the disambiguation suffix.
	ID string
	// Names are what the hostname is built from.
	Names Names
}

// Assign returns the hostname of every target, keyed by ID, and never maps
// two targets to the same hostname.
//
// Targets come in priority order, oldest registration first. Each target
// gets its plain hostname unless an earlier target already claimed it, or it
// is Reserved. Plain names are claimed before any other form, so a
// disambiguated name never takes another target's plain name. A target that
// loses its plain name gets its context as an extra label before the TLD;
// when that is taken too, "-" and the first 6 hex digits of the SHA-256 of
// its ID are appended to its first label, with more digits while that is
// taken. The result depends only on the targets and their order, and an
// existing hostname stays unchanged when a newer target with the same name
// is added.
func Assign(targets []Target) (map[string]string, error) {
	byID := make(map[string]string, len(targets))
	owner := make(map[string]string, len(targets))
	for _, t := range targets {
		if _, dup := byID[t.ID]; dup {
			return nil, fmt.Errorf("duplicate hostname target ID %q", t.ID)
		}
		if _, err := t.Names.Hostname(); err != nil {
			return nil, err
		}
		byID[t.ID] = ""
	}
	// Each round offers every still unnamed target, in order, one more
	// candidate form: plain, then with the context, then with a suffix.
	rounds := []func(Names, string) (string, error){
		func(n Names, _ string) (string, error) { return n.hostname(false, "") },
		func(n Names, _ string) (string, error) { return n.hostname(true, "") },
	}
	for _, l := range suffixLens {
		rounds = append(rounds, func(n Names, id string) (string, error) { return n.hostname(true, shortHash(id, l)) })
	}
	for _, candidate := range rounds {
		for _, t := range targets {
			if byID[t.ID] != "" {
				continue
			}
			host, err := candidate(t.Names, t.ID)
			if err != nil {
				return nil, err
			}
			if _, taken := owner[host]; !taken && !Reserved(host) {
				owner[host], byID[t.ID] = t.ID, host
			}
		}
	}
	for _, t := range targets {
		if byID[t.ID] == "" {
			return nil, fmt.Errorf("cannot find a unique hostname for target %q", t.ID)
		}
	}
	return byID, nil
}
