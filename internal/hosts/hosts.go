// Package hosts reads and rewrites the doktunnel section of the hosts file.
//
// doktunnel owns exactly one block of the hosts file, delimited by BeginLine
// and EndLine. Every function here parses, merges and rewrites only that
// block: unrelated lines are kept byte for byte, including their line
// endings, and the block itself uses the line ending the file already has.
// A file whose markers do not form one well-formed block is never merged;
// Clean is the one operation that repairs it.
package hosts

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"runtime"
	"slices"
	"strings"

	"github.com/alebak/dokploy-tunnel/internal/hostname"
	"github.com/alebak/dokploy-tunnel/internal/registry"
)

// The lines that delimit the doktunnel block. A line is a marker when, with
// surrounding whitespace removed, it starts with beginPrefix or endPrefix.
const (
	// BeginLine opens the doktunnel block.
	BeginLine = "# BEGIN doktunnel (managed block, do not edit; remove with 'doktunnel hosts clean')"
	// EndLine closes the doktunnel block.
	EndLine = "# END doktunnel"

	beginPrefix = "# BEGIN doktunnel"
	endPrefix   = "# END doktunnel"
)

// maxEntriesSize bounds the entries ParseEntries accepts; the full
// 127.77.0.0/16 range with long hostnames stays well below it in practice.
const maxEntriesSize = 4 << 20

// minLabels is the fewest labels a doktunnel hostname has:
// <appName>.internal.
const minLabels = 2

// ErrMalformed is wrapped by every error about a hosts file or entry list
// that doktunnel refuses to merge.
var ErrMalformed = errors.New("malformed doktunnel hosts section")

// MalformedError reports where a hosts file or entry list is malformed.
type MalformedError struct {
	// Line is the 1-based line number.
	Line int
	// Reason describes the problem.
	Reason string
}

func (e *MalformedError) Error() string {
	return fmt.Sprintf("%v: line %d: %s", ErrMalformed, e.Line, e.Reason)
}

// Is makes errors.Is(err, ErrMalformed) match.
func (e *MalformedError) Is(target error) bool { return target == ErrMalformed }

// Entry maps one hostname to one address.
type Entry struct {
	// IP is the leased loopback address.
	IP netip.Addr `json:"ip"`
	// Hostname is the .internal hostname.
	Hostname string `json:"hostname"`
}

// File is a parsed hosts file with at most one doktunnel block.
type File struct {
	// lines are the raw lines, each with its own line ending; the last
	// line may have none.
	lines [][]byte
	// begin and end are the indexes of the marker lines, or -1.
	begin, end int
	// eol is the line ending used for lines doktunnel writes.
	eol string
}

// Parse splits content into lines and locates the doktunnel block. It returns
// a *MalformedError when the markers do not form zero or one block.
func Parse(content []byte) (*File, error) {
	f := &File{lines: splitLines(content), begin: -1, end: -1, eol: detectEOL(content)}
	for i, l := range f.lines {
		switch markerKind(l) {
		case markerBegin:
			switch {
			case f.begin >= 0 && f.end < 0:
				return nil, &MalformedError{Line: i + 1, Reason: "begin marker inside the doktunnel block"}
			case f.end >= 0:
				return nil, &MalformedError{Line: i + 1, Reason: "second doktunnel block"}
			}
			f.begin = i
		case markerEnd:
			if f.begin < 0 || f.end >= 0 {
				return nil, &MalformedError{Line: i + 1, Reason: "end marker without a begin marker"}
			}
			f.end = i
		}
	}
	if f.begin >= 0 && f.end < 0 {
		return nil, &MalformedError{Line: f.begin + 1, Reason: "begin marker without an end marker"}
	}
	return f, nil
}

// HasBlock reports whether the file has a doktunnel block.
func (f *File) HasBlock() bool { return f.begin >= 0 }

// Entries returns the entries in the doktunnel block, one per hostname, in
// file order. Blank lines and comments are skipped.
func (f *File) Entries() ([]Entry, error) {
	if f.begin < 0 {
		return nil, nil
	}
	var out []Entry
	for i := f.begin + 1; i < f.end; i++ {
		line, _, _ := strings.Cut(string(f.lines[i]), "#")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		ip, err := netip.ParseAddr(fields[0])
		if err != nil || len(fields) < 2 {
			return nil, &MalformedError{Line: i + 1, Reason: fmt.Sprintf("%q is not an address and hostname", strings.TrimSpace(line))}
		}
		for _, h := range fields[1:] {
			out = append(out, Entry{IP: ip, Hostname: h})
		}
	}
	return out, nil
}

// WithEntries returns the file with its doktunnel block holding exactly
// entries, in the given order. The block replaces the existing one in place,
// or is appended when there is none; with no entries the block is removed.
// Every other line is kept byte for byte.
func (f *File) WithEntries(entries []Entry) []byte {
	var b bytes.Buffer
	if f.begin >= 0 {
		writeLines(&b, f.lines[:f.begin])
		if len(entries) > 0 {
			f.writeBlock(&b, entries)
		}
		writeLines(&b, f.lines[f.end+1:])
		return b.Bytes()
	}
	writeLines(&b, f.lines)
	if len(entries) == 0 {
		return b.Bytes()
	}
	if b.Len() > 0 && !bytes.HasSuffix(b.Bytes(), []byte("\n")) {
		b.WriteString(f.eol)
	}
	f.writeBlock(&b, entries)
	return b.Bytes()
}

func (f *File) writeBlock(b *bytes.Buffer, entries []Entry) {
	b.WriteString(BeginLine + f.eol)
	for _, e := range entries {
		b.WriteString(e.IP.String() + "\t" + e.Hostname + f.eol)
	}
	b.WriteString(EndLine + f.eol)
}

// Clean returns content without any doktunnel block, even when the markers
// are malformed. A begin marker followed by an end marker removes both and
// every line between them; a marker that is not part of such a pair is
// removed alone, so lines doktunnel cannot prove it wrote are kept.
func Clean(content []byte) []byte {
	lines := splitLines(content)
	var markers []int
	for i, l := range lines {
		if markerKind(l) != markerNone {
			markers = append(markers, i)
		}
	}
	drop := make([]bool, len(lines))
	for j := 0; j < len(markers); j++ {
		i := markers[j]
		drop[i] = true
		if markerKind(lines[i]) == markerBegin && j+1 < len(markers) && markerKind(lines[markers[j+1]]) == markerEnd {
			for k := i; k <= markers[j+1]; k++ {
				drop[k] = true
			}
			j++
		}
	}
	var b bytes.Buffer
	for i, l := range lines {
		if !drop[i] {
			b.Write(l)
		}
	}
	return b.Bytes()
}

// Desired computes the entries for the named leases: their hostnames are
// assigned oldest lease first (see hostname.Assign), and the entries are
// ordered by address. Leases without an appName are skipped: they were never
// named, or were named by an older doktunnel and get their name the next
// time they are forwarded.
func Desired(leases []registry.Lease) ([]Entry, error) {
	named := make([]registry.Lease, 0, len(leases))
	for _, l := range leases {
		if l.Names.AppName != "" {
			named = append(named, l)
		}
	}
	slices.SortStableFunc(named, func(a, b registry.Lease) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(targetID(a.Key), targetID(b.Key))
	})
	targets := make([]hostname.Target, len(named))
	for i, l := range named {
		targets[i] = hostname.Target{ID: targetID(l.Key), Names: l.Names}
	}
	hosts, err := hostname.Assign(targets)
	if err != nil {
		return nil, fmt.Errorf("naming leased services: %w", err)
	}
	out := make([]Entry, len(named))
	for i, l := range named {
		out[i] = Entry{IP: l.IP, Hostname: hosts[targetID(l.Key)]}
	}
	slices.SortFunc(out, func(a, b Entry) int { return a.IP.Compare(b.IP) })
	return out, nil
}

// targetID is the stable identity of a lease, seeding hostname suffixes.
func targetID(k registry.Key) string {
	return k.Instance + "\n" + k.OrganizationID + "\n" + k.ServiceID
}

// FormatEntries renders entries as "<ip>\t<hostname>" lines, the format
// ParseEntries reads.
func FormatEntries(entries []Entry) []byte {
	var b bytes.Buffer
	for _, e := range entries {
		b.WriteString(e.IP.String() + "\t" + e.Hostname + "\n")
	}
	return b.Bytes()
}

// ParseEntries reads entries written by FormatEntries and validates them
// strictly: each line holds one address in registry.DefaultRange and one
// .internal hostname of at least minLabels labels that is not
// hostname.Reserved, and no hostname repeats. The privileged helper uses
// it, so nothing else can reach the hosts file through it: not the
// 127.0.0.1 names other tools rely on, such as host.docker.internal, nor
// names such as metadata.google.internal.
//
// Errors name the line, never its contents: the helper runs as root, and
// whatever it was handed must not leak back through its error messages.
func ParseEntries(content []byte) ([]Entry, error) {
	if len(content) > maxEntriesSize {
		return nil, fmt.Errorf("%w: entry list exceeds %d bytes", ErrMalformed, maxEntriesSize)
	}
	var out []Entry
	seen := map[string]bool{}
	for i, l := range splitLines(content) {
		fields := strings.Fields(string(l))
		if len(fields) == 0 {
			continue
		}
		bad := func(reason string) error {
			return &MalformedError{Line: i + 1, Reason: reason}
		}
		if len(fields) != 2 {
			return nil, bad("want exactly one address and one hostname")
		}
		ip, err := netip.ParseAddr(fields[0])
		if err != nil || !registry.DefaultRange.Contains(ip) {
			return nil, bad(fmt.Sprintf("the address is not in %v", registry.DefaultRange))
		}
		host := fields[1]
		if !hostname.Valid(host) || strings.Count(host, ".")+1 < minLabels || hostname.Reserved(host) {
			return nil, bad(fmt.Sprintf("the hostname is not a doktunnel .%s hostname", hostname.TLD))
		}
		if seen[host] {
			return nil, bad("the hostname repeats an earlier line")
		}
		seen[host] = true
		out = append(out, Entry{IP: ip, Hostname: host})
	}
	return out, nil
}

// ReadEntries reads and validates entries from r, reading at most one byte
// more than ParseEntries accepts, so an endless stream is refused rather
// than exhausting memory.
func ReadEntries(r io.Reader) ([]Entry, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxEntriesSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading entries: %w", err)
	}
	return ParseEntries(b)
}

type marker int

const (
	markerNone marker = iota
	markerBegin
	markerEnd
)

func markerKind(line []byte) marker {
	s := strings.TrimSpace(string(line))
	switch {
	case strings.HasPrefix(s, beginPrefix):
		return markerBegin
	case strings.HasPrefix(s, endPrefix):
		return markerEnd
	}
	return markerNone
}

// splitLines splits content after every "\n", keeping line endings.
func splitLines(content []byte) [][]byte {
	lines := bytes.SplitAfter(content, []byte("\n"))
	if n := len(lines); n > 0 && len(lines[n-1]) == 0 {
		lines = lines[:n-1]
	}
	return lines
}

// detectEOL returns the line ending of the first line of content, or the
// platform's convention when content has no complete line.
func detectEOL(content []byte) string {
	i := bytes.IndexByte(content, '\n')
	switch {
	case i > 0 && content[i-1] == '\r':
		return "\r\n"
	case i >= 0:
		return "\n"
	case runtime.GOOS == "windows":
		return "\r\n"
	default:
		return "\n"
	}
}

func writeLines(b *bytes.Buffer, lines [][]byte) {
	for _, l := range lines {
		b.Write(l)
	}
}
