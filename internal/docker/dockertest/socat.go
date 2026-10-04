package dockertest

import (
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// socatLinger is how long socat keeps forwarding the other direction after
// one ends, as its -t option defaults to.
const socatLinger = 500 * time.Millisecond

// Socat returns an ExecHandler that behaves like
// `socat -d -d STDIO TCP:<host>:<port>[,options]` run in a repeater: it
// connects to resolve(host) (a real address such as a test listener's,
// or false for a name that does not resolve), logs to stderr in socat's
// format, and copies bytes both ways, half-closing like socat does.
func Socat(resolve func(host string) (addr string, ok bool)) ExecHandler {
	return func(_ string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) int {
		logf := func(level, format string, args ...any) {
			fmt.Fprintf(stderr, "%s socat[7] %s %s\n", time.Now().Format("2006/01/02 15:04:05"), level, fmt.Sprintf(format, args...))
		}
		if len(cmd) == 0 || cmd[0] != "socat" {
			fmt.Fprintf(stderr, "exec: %q: executable file not found in $PATH\n", strings.Join(cmd, " "))
			return 127
		}
		var target string
		for _, arg := range cmd[1:] {
			if rest, ok := strings.CutPrefix(arg, "TCP:"); ok {
				target, _, _ = strings.Cut(rest, ",")
			}
		}
		host, port, err := net.SplitHostPort(target)
		if err != nil {
			logf("E", "bad address %q", target)
			return 1
		}
		addr, ok := resolve(host)
		if !ok {
			logf("E", "getaddrinfo(\"%s\", \"NULL\", {0x20,0,1,6}, {}): Name does not resolve", host)
			return 1
		}
		logf("N", "opening connection to %s:%s", host, port)
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			logf("E", "connect(5, AF=2 %s, 16): Connection refused", addr)
			return 1
		}
		defer conn.Close()
		logf("N", "starting data transfer loop with FDs [0,1] and [5,5]")

		stdinDone := make(chan struct{})
		go func() {
			defer close(stdinDone)
			_, _ = io.Copy(conn, stdin)
			logf("N", "socket 1 (fd 0) is at EOF")
			if tc, ok := conn.(*net.TCPConn); ok {
				_ = tc.CloseWrite()
			}
		}()
		outDone := make(chan struct{})
		go func() {
			defer close(outDone)
			_, _ = io.Copy(stdout, conn)
		}()
		select {
		case <-outDone:
		case <-stdinDone:
			select {
			case <-outDone:
			case <-time.After(socatLinger):
			}
		}
		logf("N", "exiting with status 0")
		return 0
	}
}
