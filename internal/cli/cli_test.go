package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"testing"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
	"github.com/alebak/dokploy-tunnel/internal/prompt"
	"github.com/alebak/dokploy-tunnel/internal/version"
)

type result struct {
	exit   int
	stdout string
	stderr string
}

func run(t *testing.T, root *Command, stdin string, terminal bool, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	app := &App{
		Root:            root,
		Stdin:           strings.NewReader(stdin),
		Stdout:          &stdout,
		Stderr:          &stderr,
		StdinIsTerminal: terminal,
	}
	exit := app.Run(args)
	return result{exit: exit, stdout: stdout.String(), stderr: stderr.String()}
}

func decodeError(t *testing.T, stdout string) clierr.Error {
	t.Helper()
	var e clierr.Error
	if err := json.Unmarshal([]byte(stdout), &e); err != nil {
		t.Fatalf("stdout is not a JSON error: %v (%q)", err, stdout)
	}
	return e
}

func TestRun_HelpListsCommandGroups(t *testing.T) {
	groups := []string{"context", "services", "forward", "status", "hosts"}
	for _, args := range [][]string{{"--help"}, {"-h"}, {}} {
		t.Run(fmt.Sprintf("args %q", args), func(t *testing.T) {
			root := NewRoot()
			r := run(t, root, "", true, args...)
			if r.exit != 0 {
				t.Fatalf("exit = %d, want 0 (stderr %q)", r.exit, r.stderr)
			}
			for _, name := range groups {
				cmd := root.Find(name)
				if cmd == nil {
					t.Fatalf("command %q is missing from the tree", name)
				}
				if cmd.Summary == "" {
					t.Errorf("command %q has no summary", name)
				}
				if !strings.Contains(r.stdout, name) || !strings.Contains(r.stdout, cmd.Summary) {
					t.Errorf("help does not list %q with its summary:\n%s", name, r.stdout)
				}
			}
			for _, flag := range []string{"--json", "--no-input", "--context", "--version"} {
				if !strings.Contains(r.stdout, flag) {
					t.Errorf("help does not list %s:\n%s", flag, r.stdout)
				}
			}
		})
	}
}

func TestRun_Version(t *testing.T) {
	r := run(t, NewRoot(), "", true, "--version")
	if r.exit != 0 {
		t.Fatalf("exit = %d, want 0", r.exit)
	}
	if got, want := strings.TrimSpace(r.stdout), version.String("doktunnel"); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestRun_CommandHelp(t *testing.T) {
	root := NewRoot()
	r := run(t, root, "", true, "services", "--help")
	if r.exit != 0 {
		t.Fatalf("exit = %d, want 0", r.exit)
	}
	if !strings.Contains(r.stdout, root.Find("services").Summary) || !strings.Contains(r.stdout, "--json") {
		t.Errorf("services help is incomplete:\n%s", r.stdout)
	}
}

func TestRun_UnimplementedCommandsReturnNotImplemented(t *testing.T) {
	for _, name := range []string{"context", "services", "forward", "status", "hosts"} {
		variants := [][]string{
			{name, "--json", "--no-input"},
			{"--json", name, "--context", "prod"},
			{name, "some-arg", "--json"},
		}
		for _, args := range variants {
			t.Run(strings.Join(args, " "), func(t *testing.T) {
				r := run(t, NewRoot(), "", false, args...)
				if want := clierr.NotImplemented.ExitCode(); r.exit != want {
					t.Errorf("exit = %d, want %d", r.exit, want)
				}
				if r.stderr != "" {
					t.Errorf("stderr = %q, want empty in JSON mode", r.stderr)
				}
				if e := decodeError(t, r.stdout); e.Code != clierr.NotImplemented || e.Message == "" {
					t.Errorf("error = %+v, want code %q with a message", e, clierr.NotImplemented)
				}
			})
		}
	}
}

func TestRun_HumanErrorGoesToStderr(t *testing.T) {
	r := run(t, NewRoot(), "", true, "status")
	if want := clierr.NotImplemented.ExitCode(); r.exit != want {
		t.Errorf("exit = %d, want %d", r.exit, want)
	}
	if r.stdout != "" {
		t.Errorf("stdout = %q, want empty", r.stdout)
	}
	if !strings.Contains(r.stderr, string(clierr.NotImplemented)) {
		t.Errorf("stderr %q does not show the error code", r.stderr)
	}
}

func TestRun_InvalidUsage(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"unknown command", []string{"--json", "nope"}},
		{"unknown flag on root", []string{"--json", "--bogus"}},
		{"unknown flag before --json", []string{"--bogus", "--json"}},
		{"unknown flag on command", []string{"services", "--bogus", "--json"}},
		{"bad bool value", []string{"--json", "--no-input=maybe"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := run(t, NewRoot(), "", true, tt.args...)
			if want := clierr.InvalidArgument.ExitCode(); r.exit != want {
				t.Errorf("exit = %d, want %d", r.exit, want)
			}
			if e := decodeError(t, r.stdout); e.Code != clierr.InvalidArgument {
				t.Errorf("code = %q, want %q", e.Code, clierr.InvalidArgument)
			}
		})
	}
}

// askRoot builds a tree whose only command asks for --context through the
// input policy and prints the answer.
func askRoot(seen *Env) *Command {
	return &Command{
		Name: "doktunnel",
		Subcommands: []*Command{{
			Name:    "ask",
			Summary: "Ask for a context",
			Run: func(env *Env, args []string) error {
				*seen = *env
				v, err := env.Input.Ask(prompt.Request{Flag: "context", Label: "Context"})
				if err != nil {
					return err
				}
				fmt.Fprintln(env.Stdout, v)
				return nil
			},
		}},
	}
}

func TestRun_InputPolicy(t *testing.T) {
	tests := []struct {
		name        string
		terminal    bool
		args        []string
		wantExit    int
		wantStdout  string
		wantNoInput bool
	}{
		{"terminal asks", true, []string{"ask"}, 0, "prod\n", false},
		{"--no-input refuses", true, []string{"ask", "--no-input"}, clierr.MissingInput.ExitCode(), "", true},
		{"non-terminal stdin implies --no-input", false, []string{"ask"}, clierr.MissingInput.ExitCode(), "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var env Env
			r := run(t, askRoot(&env), "prod\n", tt.terminal, tt.args...)
			if r.exit != tt.wantExit {
				t.Errorf("exit = %d, want %d (stderr %q)", r.exit, tt.wantExit, r.stderr)
			}
			if r.stdout != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", r.stdout, tt.wantStdout)
			}
			if env.NoInput != tt.wantNoInput {
				t.Errorf("env.NoInput = %v, want %v", env.NoInput, tt.wantNoInput)
			}
			if tt.wantExit != 0 && !strings.Contains(r.stderr, "--context") {
				t.Errorf("stderr %q does not name the --context flag", r.stderr)
			}
		})
	}
}

func TestRun_GlobalFlagsReachCommandFromAnyLevel(t *testing.T) {
	var env Env
	r := run(t, askRoot(&env), "answer\n", true, "--context", "prod", "ask", "--json")
	if r.exit != 0 {
		t.Fatalf("exit = %d, want 0 (stdout %q)", r.exit, r.stdout)
	}
	if !env.JSON || env.Context != "prod" {
		t.Errorf("globals = %+v, want JSON=true Context=prod", env.Globals)
	}
}

func TestRun_FlagsAfterDoubleDashArePositional(t *testing.T) {
	var gotArgs []string
	root := &Command{Name: "doktunnel", Subcommands: []*Command{{
		Name: "echo", Summary: "Echo args",
		Run: func(env *Env, args []string) error {
			gotArgs = args
			if env.JSON {
				return fmt.Errorf("--json after -- must not be parsed")
			}
			return nil
		},
	}}}
	r := run(t, root, "", true, "echo", "a", "--", "--json", "b")
	if r.exit != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", r.exit, r.stderr)
	}
	if want := []string{"a", "--json", "b"}; strings.Join(gotArgs, ",") != strings.Join(want, ",") {
		t.Errorf("args = %q, want %q", gotArgs, want)
	}
}

// flagRoot builds a tree with a leaf that declares its own flags.
func flagRoot(gotURL *string, gotForce *bool, gotArgs *[]string) *Command {
	var url string
	var force bool
	return &Command{Name: "doktunnel", Subcommands: []*Command{
		{
			Name: "add", Summary: "Add a thing", Args: "<name>",
			Flags: func(fs *flag.FlagSet) {
				fs.StringVar(&url, "url", "", "panel `URL`")
				fs.BoolVar(&force, "force", false, "overwrite")
			},
			Run: func(env *Env, args []string) error {
				*gotURL, *gotForce, *gotArgs = url, force, args
				return nil
			},
		},
		{Name: "other", Summary: "Another thing", Run: func(*Env, []string) error { return nil }},
	}}
}

func TestRun_CommandFlags(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantURL   string
		wantForce bool
		wantArgs  []string
	}{
		{"flags after args", []string{"add", "x", "--url", "http://h", "--force"}, "http://h", true, []string{"x"}},
		{"flags before args", []string{"add", "--url=http://h", "x", "--json"}, "http://h", false, []string{"x"}},
		{"defaults", []string{"add", "x"}, "", false, []string{"x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var url string
			var force bool
			var args []string
			r := run(t, flagRoot(&url, &force, &args), "", false, tt.args...)
			if r.exit != 0 {
				t.Fatalf("exit = %d, want 0 (stdout %q stderr %q)", r.exit, r.stdout, r.stderr)
			}
			if url != tt.wantURL || force != tt.wantForce || strings.Join(args, ",") != strings.Join(tt.wantArgs, ",") {
				t.Errorf("got url=%q force=%v args=%q, want url=%q force=%v args=%q",
					url, force, args, tt.wantURL, tt.wantForce, tt.wantArgs)
			}
		})
	}
}

func TestRun_CommandFlagsAreLocal(t *testing.T) {
	var url string
	var force bool
	var args []string
	r := run(t, flagRoot(&url, &force, &args), "", false, "other", "--url", "x", "--json")
	if e := decodeError(t, r.stdout); e.Code != clierr.InvalidArgument {
		t.Errorf("code = %q, want %q: a flag of one command must not be accepted by another", e.Code, clierr.InvalidArgument)
	}
}

func TestRun_CommandHelpListsFlagsAndArgs(t *testing.T) {
	var url string
	var force bool
	var args []string
	r := run(t, flagRoot(&url, &force, &args), "", true, "add", "--help")
	if r.exit != 0 {
		t.Fatalf("exit = %d, want 0", r.exit)
	}
	for _, want := range []string{"doktunnel add [flags] <name>", "--url <URL>", "--force", "--json"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("help does not contain %q:\n%s", want, r.stdout)
		}
	}
}
