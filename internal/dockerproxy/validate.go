package dockerproxy

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/alebak/dokploy-tunnel/internal/docker"
	"github.com/alebak/dokploy-tunnel/internal/repeater"
)

// The repeater's shape, as package repeater creates it. These copies must
// match it; TestProxy_RunsTheCompanionsRepeater fails when they drift.
const (
	repeaterNamePrefix = "doktunnel-repeater-"
	repeaterUser       = "65534:65534"
	maxPidsLimit       = 256
	maxMemoryBytes     = 64 << 20
	// maxConnectTimeout bounds socat's connect-timeout, in seconds.
	maxConnectTimeout = 300
	// maxLabelBytes bounds a label value.
	maxLabelBytes = 1024
)

// labelRepeater marks a repeater container.
const labelRepeater = repeater.LabelRepeater

// repeaterLabels are the labels every repeater carries, and the only ones
// it may carry: labels such as Traefik's would change how other services
// treat the container.
var repeaterLabels = []string{
	repeater.LabelRepeater,
	repeater.LabelOwner,
	repeater.LabelTarget,
	repeater.LabelNetwork,
	repeater.LabelCreatedAt,
	repeater.LabelOwnership,
}

var repeaterName = regexp.MustCompile(`^` + repeaterNamePrefix + `[A-Za-z0-9_.-]{1,64}$`)

// isRepeaterName reports whether name, without a leading slash, is a
// repeater's.
func isRepeaterName(name string) bool {
	return repeaterName.MatchString(name)
}

// checkRepeater checks that cfg is exactly a repeater's configuration:
// image running sleep as nobody, joining one network, with no capabilities,
// a read-only root file system, an init and bounded resources. Fields cfg
// cannot hold were already refused when it was decoded.
func checkRepeater(cfg docker.ContainerConfig, image string) error {
	switch {
	case cfg.Image != image:
		return deny("image %q is not the repeater image", cfg.Image)
	case !slices.Equal(cfg.Entrypoint, []string{"sleep"}) || !slices.Equal(cfg.Cmd, []string{"infinity"}):
		return deny("a repeater runs sleep infinity only")
	case cfg.User != repeaterUser:
		return deny("a repeater runs as %s", repeaterUser)
	}
	if len(cfg.Labels) != len(repeaterLabels) {
		return deny("a repeater carries exactly the labels %s", strings.Join(repeaterLabels, ", "))
	}
	for _, key := range repeaterLabels {
		v, ok := cfg.Labels[key]
		if !ok || len(v) > maxLabelBytes {
			return deny("a repeater carries exactly the labels %s", strings.Join(repeaterLabels, ", "))
		}
	}
	if cfg.Labels[labelRepeater] != "1" {
		return deny("label %s must be 1", labelRepeater)
	}

	h := cfg.HostConfig
	switch {
	case !isNetworkRef(h.NetworkMode):
		return deny("network mode %q is not a network's ID or name", h.NetworkMode)
	case !h.ReadonlyRootfs:
		return deny("a repeater has a read-only root file system")
	case !slices.Equal(h.CapDrop, []string{"ALL"}):
		return deny("a repeater drops every capability")
	case !slices.Equal(h.SecurityOpt, []string{"no-new-privileges"}):
		return deny("a repeater's only security option is no-new-privileges")
	case h.Init == nil || !*h.Init:
		return deny("a repeater runs an init")
	case h.PidsLimit == nil || *h.PidsLimit <= 0 || *h.PidsLimit > maxPidsLimit:
		return deny("a repeater's process limit is between 1 and %d", maxPidsLimit)
	case h.Memory <= 0 || h.Memory > maxMemoryBytes:
		return deny("a repeater's memory limit is between 1 and %d bytes", maxMemoryBytes)
	}
	return nil
}

// isNetworkRef reports whether mode names a network, rather than the
// host's network stack, no network or another container's.
func isNetworkRef(mode string) bool {
	return objectRef.MatchString(mode) && mode != "host" && mode != "none"
}

// checkConfined checks that an inspected container is confined like a
// repeater: unprivileged, as nobody, on a network of its own, with every
// capability dropped, no new privileges, a read-only root file system and
// nothing mounted. An exec in such a container gets nothing an exec in a
// repeater would not.
func checkConfined(c docker.Container) error {
	h := c.HostConfig
	switch {
	case h.Privileged:
		return errors.New("it is privileged")
	case !isNetworkRef(h.NetworkMode):
		return fmt.Errorf("network mode %q is not a network's ID or name", h.NetworkMode)
	case c.Config.User != repeaterUser:
		return fmt.Errorf("it does not run as %s", repeaterUser)
	case !slices.Contains(h.CapDrop, "ALL") || len(h.CapAdd) > 0:
		return errors.New("it keeps capabilities")
	case !slices.Contains(h.SecurityOpt, "no-new-privileges"):
		return errors.New("it may gain new privileges")
	case !h.ReadonlyRootfs:
		return errors.New("its root file system is writable")
	case len(h.Binds) > 0 || len(c.Mounts) > 0:
		return errors.New("it has mounts")
	}
	return nil
}

// execConfig is the body of an exec creation, as the companion sends it.
type execConfig struct {
	AttachStdin  bool     `json:"AttachStdin"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Tty          bool     `json:"Tty"`
	Cmd          []string `json:"Cmd"`
}

// execStart is the body of an exec start, as the companion sends it.
type execStart struct {
	Detach bool `json:"Detach"`
	Tty    bool `json:"Tty"`
}

// checkExec checks that cfg runs socat, attached and without a TTY,
// exactly as the companion does.
func checkExec(cfg execConfig) error {
	if !cfg.AttachStdin || !cfg.AttachStdout || !cfg.AttachStderr || cfg.Tty {
		return deny("an exec attaches stdin, stdout and stderr without a TTY")
	}
	if !isSocat(cfg.Cmd) {
		return deny("an exec runs socat -d -d STDIO TCP:<ip>:<port>,connect-timeout=<seconds> only")
	}
	return nil
}

// isSocat reports whether cmd is
// "socat -d -d STDIO TCP:<ip>:<port>,connect-timeout=<seconds>" with a
// literal IP address in canonical form, so that no socat option or
// address type can slip in.
func isSocat(cmd []string) bool {
	if len(cmd) != 5 || !slices.Equal(cmd[:4], []string{"socat", "-d", "-d", "STDIO"}) {
		return false
	}
	rest, ok := strings.CutPrefix(cmd[4], "TCP:")
	if !ok {
		return false
	}
	hostPort, timeout, ok := strings.Cut(rest, ",connect-timeout=")
	if !ok {
		return false
	}
	ap, err := netip.ParseAddrPort(hostPort)
	if err != nil || ap.Port() == 0 || ap.Addr().Zone() != "" || ap.String() != hostPort {
		return false
	}
	n, err := strconv.Atoi(timeout)
	return err == nil && n > 0 && n <= maxConnectTimeout && strconv.Itoa(n) == timeout
}
