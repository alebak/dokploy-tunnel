//go:build platform

package platformtest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ifconfigPath is macOS's ifconfig, by absolute path as doktunnel runs it.
const ifconfigPath = "/sbin/ifconfig"

// requirePlatform skips the calling test unless it runs on an opted-in
// GitHub Actions runner; see Enabled.
func requirePlatform(t *testing.T) {
	t.Helper()
	if !Enabled(os.Getenv) {
		t.Skipf("platform test: runs sudo and rewrites the system hosts file, so it only runs on a GitHub Actions runner with %s=1", EnvVar)
	}
}

// report records an observation in the test log and in the job summary, so
// what each runner proved is visible without reading the raw log.
func report(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	t.Log(msg)
	appendSummary(t, "- "+runtime.GOOS+": "+msg)
}

// finding records a platform assumption the runner disproved: a warning
// annotation on the workflow run plus a job summary line. It does not fail
// the test by itself; callers decide whether the finding is fatal.
func finding(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	t.Log("FINDING: " + msg)
	// Workflow commands must start a line, so they bypass t.Log's indent.
	fmt.Printf("::warning title=Platform finding (%s)::%s\n", runtime.GOOS, escapeAnnotation(msg))
	appendSummary(t, "- **FINDING** "+runtime.GOOS+": "+msg)
}

func escapeAnnotation(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
}

func appendSummary(t *testing.T, line string) {
	t.Helper()
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		t.Logf("writing job summary: %v", err)
		return
	}
	defer f.Close()
	if _, err := fmt.Fprintln(f, line); err != nil {
		t.Logf("writing job summary: %v", err)
	}
}

// sudo runs argv as root with stdin as its standard input. GitHub's Linux
// and macOS runners allow passwordless sudo; -n makes any prompt an error.
func sudo(stdin []byte, argv ...string) ([]byte, error) {
	cmd := exec.Command("sudo", append([]string{"-n", "--"}, argv...)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("sudo %s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// bindAndConnect listens on ip and proves a client can connect to it.
func bindAndConnect(ip netip.Addr) error {
	ln, err := net.Listen("tcp", netip.AddrPortFrom(ip, 0).String())
	if err != nil {
		return fmt.Errorf("listening on %s: %w", ip, err)
	}
	defer ln.Close()
	return echo(ln, ln.Addr().String())
}

// echo connects to addr, which must reach ln, and checks that a byte sent
// through the connection comes back.
func echo(ln net.Listener, addr string) error {
	accepted := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			accepted <- err
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		_, err = io.Copy(c, io.LimitReader(c, 1))
		accepted <- err
	}()

	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", addr, err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte{'x'}); err != nil {
		return fmt.Errorf("writing to %s: %w", addr, err)
	}
	b := make([]byte, 1)
	if _, err := io.ReadFull(c, b); err != nil {
		return fmt.Errorf("reading from %s: %w", addr, err)
	}
	if b[0] != 'x' {
		return fmt.Errorf("reading from %s: got %q, want %q", addr, b, "x")
	}
	return <-accepted
}

// hasLo0Alias reports whether ip is configured on macOS's lo0.
func hasLo0Alias(ip netip.Addr) (bool, error) {
	out, err := exec.Command(ifconfigPath, "lo0").CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("reading lo0 addresses: %w: %s", err, strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "inet" && f[1] == ip.String() {
			return true, nil
		}
	}
	return false, nil
}

// removeLo0Alias removes ip from lo0 if it is there, so the runner is left
// as it was found.
func removeLo0Alias(t *testing.T, ip netip.Addr) {
	t.Helper()
	present, err := hasLo0Alias(ip)
	if err != nil {
		t.Errorf("cleanup: %v", err)
		return
	}
	if !present {
		return
	}
	if _, err := sudo(nil, ifconfigPath, "lo0", "-alias", ip.String()); err != nil {
		t.Errorf("cleanup: removing lo0 alias %s: %v", ip, err)
	}
}

// eventually retries fn for up to timeout, since resolver caches may lag a
// hosts file change briefly, and returns its last error.
func eventually(timeout time.Duration, fn func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := fn()
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// exitCode returns the exit code carried by err, 0 for nil, or -1 when the
// process did not run.
func exitCode(err error) int {
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.ExitCode()
	default:
		return -1
	}
}
