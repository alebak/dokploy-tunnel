package lowport

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseAddrs_Valid(t *testing.T) {
	tests := []struct {
		name string
		list string
		want []string
	}{
		{name: "one address", list: "127.77.0.5:80", want: []string{"127.77.0.5:80"}},
		{name: "several addresses", list: "127.77.0.5:80,127.77.0.5:443,127.77.255.254:1", want: []string{"127.77.0.5:80", "127.77.0.5:443", "127.77.255.254:1"}},
		{name: "highest port below the limit", list: "127.77.0.5:1023", want: []string{"127.77.0.5:1023"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseAddrs(tt.list, 1024)
			if err != nil {
				t.Fatalf("ParseAddrs(%q) error: %v", tt.list, err)
			}
			var s []string
			for _, a := range got {
				s = append(s, a.String())
			}
			if !slices.Equal(s, tt.want) {
				t.Fatalf("ParseAddrs(%q) = %v, want %v", tt.list, s, tt.want)
			}
			if back := FormatAddrs(got); back != tt.list {
				t.Fatalf("FormatAddrs = %q, want %q", back, tt.list)
			}
		})
	}
}

func TestParseAddrs_Refused(t *testing.T) {
	many := make([]string, MaxAddrs+1)
	for i := range many {
		many[i] = fmt.Sprintf("127.77.1.%d:80", i+1)
	}
	tests := []struct {
		name  string
		list  string
		limit int
	}{
		{name: "empty list", list: "", limit: 1024},
		{name: "address outside 127.77.0.0/16", list: "127.0.0.1:80", limit: 1024},
		{name: "address that is not loopback", list: "10.0.0.5:80", limit: 1024},
		{name: "IPv6 address", list: "[::1]:80", limit: 1024},
		{name: "port zero", list: "127.77.0.5:0", limit: 1024},
		{name: "port at the unprivileged limit", list: "127.77.0.5:1024", limit: 1024},
		{name: "port above a lowered limit", list: "127.77.0.5:80", limit: 80},
		{name: "high port", list: "127.77.0.5:5432", limit: 1024},
		{name: "duplicate address", list: "127.77.0.5:80,127.77.0.5:80", limit: 1024},
		{name: "empty element", list: "127.77.0.5:80,", limit: 1024},
		{name: "hostname instead of an address", list: "localhost:80", limit: 1024},
		{name: "zone", list: "[fe80::1%lo]:80", limit: 1024},
		{name: "too many addresses", list: strings.Join(many, ","), limit: 1024},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseAddrs(tt.list, tt.limit)
			if err == nil {
				t.Fatalf("ParseAddrs(%q, %d) = %v, want an error", tt.list, tt.limit, got)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("ParseAddrs error %v does not wrap ErrInvalid", err)
			}
		})
	}
}

// TestParseAddrs_ErrorsDoNotEchoInput checks that the helper, which runs as
// root, never repeats what it was handed.
func TestParseAddrs_ErrorsDoNotEchoInput(t *testing.T) {
	_, err := ParseAddrs("127.77.0.5:80,/etc/shadow", 1024)
	if err == nil || strings.Contains(err.Error(), "shadow") {
		t.Fatalf("error %v, want one that does not echo the input", err)
	}
}

func TestValidName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{name: "doktunnel-0123456789abcdef0123456789abcdef", want: true},
		{name: "doktunnel-0123456789ABCDEF0123456789abcdef", want: false},
		{name: "doktunnel-0123", want: false},
		{name: "/tmp/doktunnel-0123456789abcdef0123456789abcdef", want: false},
		{name: "@doktunnel-0123456789abcdef0123456789abcdef", want: false},
		{name: "", want: false},
	}
	for _, tt := range tests {
		if got := ValidName(tt.name); got != tt.want {
			t.Errorf("ValidName(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestNewName_IsValidAndRandom(t *testing.T) {
	a, err := NewName()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewName()
	if err != nil {
		t.Fatal(err)
	}
	if !ValidName(a) || !ValidName(b) || a == b {
		t.Fatalf("NewName() = %q, %q; want two distinct valid names", a, b)
	}
}

func TestReadLimit(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tests := []struct {
		name string
		path string
		want int
	}{
		{name: "default", path: write("default", "1024\n"), want: 1024},
		{name: "lowered", path: write("lowered", "80\n"), want: 80},
		{name: "everything unprivileged", path: write("zero", "0\n"), want: 0},
		{name: "unreadable", path: filepath.Join(dir, "missing"), want: DefaultLimit},
		{name: "garbage", path: write("garbage", "abc\n"), want: DefaultLimit},
		{name: "out of range", path: write("big", "70000\n"), want: DefaultLimit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := readLimit(tt.path); got != tt.want {
				t.Fatalf("readLimit = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFormatAddrs_Empty(t *testing.T) {
	if got := FormatAddrs(nil); got != "" {
		t.Fatalf("FormatAddrs(nil) = %q", got)
	}
	_ = netip.AddrPort{}
}
