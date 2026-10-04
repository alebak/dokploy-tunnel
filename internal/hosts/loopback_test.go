package hosts

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

const ifconfigLo0 = `lo0: flags=8049<UP,LOOPBACK,RUNNING,MULTICAST> mtu 16384
	options=1203<RXCSUM,TXCSUM,TXSTATUS,SW_TIMESTAMP>
	inet 127.0.0.1 netmask 0xff000000
	inet6 ::1 prefixlen 128
	inet 127.77.0.1 netmask 0xff000000
	nd6 options=201<PERFORMNUD,DAD>
`

type fakeRunner struct {
	calls  []string
	output []byte
	err    error
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	return f.output, f.err
}

func addrs(s ...string) []netip.Addr {
	out := make([]netip.Addr, len(s))
	for i, a := range s {
		out[i] = netip.MustParseAddr(a)
	}
	return out
}

func TestLoopbackFor(t *testing.T) {
	for goos, wantAliases := range map[string]bool{"darwin": true, "linux": false, "windows": false} {
		_, isIfconfig := loopbackFor(goos, nil).(ifconfigLoopback)
		if isIfconfig != wantAliases {
			t.Errorf("loopbackFor(%q) manages aliases = %v, want %v", goos, isIfconfig, wantAliases)
		}
	}
}

func TestNoLoopback(t *testing.T) {
	missing, err := noLoopback{}.Missing(context.Background(), addrs("127.77.0.1"))
	if err != nil || len(missing) != 0 {
		t.Errorf("Missing = %v, %v; want none", missing, err)
	}
	if err := (noLoopback{}).Add(context.Background(), addrs("127.77.0.1")); err != nil {
		t.Errorf("Add = %v", err)
	}
}

func TestIfconfigLoopback_Missing(t *testing.T) {
	r := &fakeRunner{output: []byte(ifconfigLo0)}
	l := ifconfigLoopback{run: r.run}
	missing, err := l.Missing(context.Background(), addrs("127.77.0.1", "127.77.0.2", "127.77.0.3"))
	if err != nil {
		t.Fatal(err)
	}
	if want := addrs("127.77.0.2", "127.77.0.3"); !slices.Equal(missing, want) {
		t.Errorf("Missing = %v, want %v", missing, want)
	}
	if want := []string{"ifconfig lo0"}; !slices.Equal(r.calls, want) {
		t.Errorf("calls = %q, want %q", r.calls, want)
	}

	r.err = errors.New("boom")
	if _, err := l.Missing(context.Background(), addrs("127.77.0.2")); err == nil {
		t.Error("Missing ignored an ifconfig failure")
	}
}

func TestIfconfigLoopback_Add(t *testing.T) {
	r := &fakeRunner{}
	l := ifconfigLoopback{run: r.run}
	if err := l.Add(context.Background(), addrs("127.77.0.2", "127.77.0.3")); err != nil {
		t.Fatal(err)
	}
	want := []string{"ifconfig lo0 alias 127.77.0.2 up", "ifconfig lo0 alias 127.77.0.3 up"}
	if !slices.Equal(r.calls, want) {
		t.Errorf("calls = %q, want %q", r.calls, want)
	}
}
