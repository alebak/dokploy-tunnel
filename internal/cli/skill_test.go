package cli

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "update golden files")

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden file (run go test -update to create it): %v", err)
	}
	if got != string(want) {
		t.Errorf("output does not match %s; run go test ./internal/cli -update and review the diff\ngot:\n%s", path, got)
	}
}

func TestRun_SkillGolden(t *testing.T) {
	r := run(t, NewRoot(), "", false, "--skill")
	if r.exit != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", r.exit, r.stderr)
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q, want empty", r.stderr)
	}
	assertGolden(t, "skill.golden", r.stdout)
}

func TestRun_SkillIgnoresOutputFlags(t *testing.T) {
	want := run(t, NewRoot(), "", false, "--skill").stdout
	for _, args := range [][]string{
		{"--skill", "--json"},
		{"--json", "--skill"},
		{"--skill", "--no-input", "--context", "missing"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			r := run(t, NewRoot(), "", true, args...)
			if r.exit != 0 {
				t.Fatalf("exit = %d, want 0 (stderr %q)", r.exit, r.stderr)
			}
			if r.stdout != want {
				t.Errorf("output differs from plain --skill:\n%s", r.stdout)
			}
		})
	}
}

func noopRun(*Env, []string) error { return nil }

func TestRun_SkillFollowsCommandTree(t *testing.T) {
	root := NewRoot()
	root.Subcommands = append(root.Subcommands, &Command{
		Name:    "widget",
		Summary: "Manage widgets",
		Subcommands: []*Command{
			{Name: "spin", Summary: "Spin a widget", Description: "Spins until stopped.", Run: noopRun},
			{
				Name: "paint", Summary: "Paint a widget", Args: "<name>",
				Flags: func(fs *flag.FlagSet) { fs.String("color", "", "paint `color`") },
				Run:   noopRun,
			},
		},
	})
	r := run(t, root, "", false, "--skill")
	if r.exit != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", r.exit, r.stderr)
	}
	for _, want := range []string{
		"| `doktunnel widget` | Manage widgets |",
		"| `doktunnel widget spin` | Spin a widget |",
		"Spins until stopped.",
		"Usage: `doktunnel widget paint [flags] <name>`",
		"| `--color <color>` | paint color |",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("skill does not contain %q:\n%s", want, r.stdout)
		}
	}
}

func TestSkillSpec_IncludesEveryGlobalFlag(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	BindGlobalFlags(fs, &Globals{})
	spec := SkillSpec(NewRoot())
	fs.VisitAll(func(f *flag.Flag) {
		for _, got := range spec.GlobalFlags {
			if got.Name == f.Name {
				if got.Usage == "" {
					t.Errorf("flag --%s has no usage", f.Name)
				}
				return
			}
		}
		t.Errorf("global flag --%s is missing from the skill spec", f.Name)
	})
	if len(spec.RootFlags) == 0 {
		t.Error("skill spec has no root flags; want --version and --skill")
	}
}
