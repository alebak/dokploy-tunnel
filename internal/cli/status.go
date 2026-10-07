package cli

import (
	"cmp"
	"flag"
	"fmt"
	"net/netip"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
	"github.com/alebak/dokploy-tunnel/internal/output"
	"github.com/alebak/dokploy-tunnel/internal/runstate"
)

// statusJSON is the stable JSON form of "status".
type statusJSON struct {
	Forwards []statusForwardJSON `json:"forwards"`
	Warnings []string            `json:"warnings"`
}

// statusForwardJSON is one active forward: the process that serves it and
// the forward as that process recorded it.
type statusForwardJSON struct {
	PID          int       `json:"pid"`
	StartedAt    time.Time `json:"started_at"`
	Context      string    `json:"context"`
	CompanionURL string    `json:"companion_url"`
	runstate.Forward
}

func newStatusCommand() *Command {
	return &Command{
		Name:    "status",
		Summary: "Show active forwards",
		Description: "Lists the forwards of every running 'doktunnel forward' process, of all contexts, or only of " +
			"the context named by an explicit --context. It reads the state files those processes keep in the " +
			"doktunnel state directory and needs no network access. A file whose process is no longer running " +
			"is stale and removed; a file of a running process that cannot be read, such as one written by " +
			"another doktunnel version, is left in place and reported as a warning. A killed forward process " +
			"whose PID was reused by another program is still listed until that program exits. " +
			"Human output: a table with CONTEXT, HOSTNAME, ADDRESS, TARGET, PID and SINCE, sorted by context, " +
			"hostname and port, or 'No active forwards.' on stderr. JSON output: " +
			"`{\"forwards\":[...],\"warnings\":[...]}`, both always arrays, where each forward has `pid`, " +
			"`started_at` (RFC 3339), `context`, `companion_url`, `target` (`type`, `id`, `name`), `hostname`, " +
			"`ip` and `port`, and each warning is a string.",
		// No flags of its own, but declaring them makes usage show that
		// status takes no arguments.
		Flags: func(*flag.FlagSet) {},
		Run: func(env *Env, args []string) error {
			if len(args) > 0 {
				return clierr.Newf(clierr.InvalidArgument, "status takes no arguments, got %q", args[0]).
					WithHint("run 'doktunnel status --help' for usage")
			}
			return runStatus(env)
		},
	}
}

func runStatus(env *Env) error {
	out, err := env.activeForwards()
	if err != nil {
		return err
	}
	if env.JSON {
		return output.WriteJSON(env.Stdout, out)
	}
	for _, w := range out.Warnings {
		env.warn(w)
	}
	if len(out.Forwards) == 0 {
		_, err := fmt.Fprintln(env.Stderr, "No active forwards.")
		return err
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CONTEXT\tHOSTNAME\tADDRESS\tTARGET\tPID\tSINCE")
	for _, f := range out.Forwards {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", f.Context, f.Hostname,
			netip.AddrPortFrom(f.IP, uint16(f.Port)), f.Target.Name, f.PID,
			f.StartedAt.Local().Format(time.DateTime))
	}
	return tw.Flush()
}

// activeForwards reads the state files of forward processes, removes those
// of processes that are no longer running, and returns the forwards of the
// running ones, filtered by an explicit --context.
func (e *Env) activeForwards() (statusJSON, error) {
	out := statusJSON{Forwards: []statusForwardJSON{}, Warnings: []string{}}
	dir, err := e.forwardStateDir()
	if err != nil {
		return out, err
	}
	entries, err := runstate.List(dir)
	if err != nil {
		return out, clierr.New(clierr.Internal, err.Error())
	}
	for _, entry := range entries {
		if !e.ProcessAlive(entry.PID) {
			// Stale whatever its content: its process can no longer use it.
			if err := runstate.Remove(dir, entry.PID); err != nil {
				out.Warnings = append(out.Warnings, err.Error())
			}
			continue
		}
		if entry.Err != nil {
			out.Warnings = append(out.Warnings, "ignored: "+entry.Err.Error())
			continue
		}
		p := entry.Process
		if e.Context != "" && p.Context != e.Context {
			continue
		}
		for _, f := range p.Forwards {
			out.Forwards = append(out.Forwards, statusForwardJSON{
				PID: p.PID, StartedAt: p.StartedAt, Context: p.Context, CompanionURL: p.CompanionURL, Forward: f,
			})
		}
	}
	slices.SortFunc(out.Forwards, func(a, b statusForwardJSON) int {
		return cmp.Or(
			cmp.Compare(a.Context, b.Context),
			cmp.Compare(a.Hostname, b.Hostname),
			cmp.Compare(a.Port, b.Port),
			cmp.Compare(a.PID, b.PID),
		)
	})
	return out, nil
}
