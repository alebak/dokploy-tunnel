package skill

import (
	"fmt"
	"strings"
	"testing"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
)

func sampleSpec() Spec {
	return Spec{
		Name:    "doktunnel",
		Summary: "doktunnel forwards things.",
		Commands: []Command{
			{Path: "doktunnel status", Summary: "Show active forwards"},
			{Path: "doktunnel widget spin", Summary: "Spin a widget", Description: "Spins until stopped."},
		},
		GlobalFlags: []Flag{
			{Name: "json", Usage: "print JSON"},
			{Name: "color", Arg: "mode", Usage: "use auto|always|never colors"},
		},
		RootFlags: []Flag{{Name: "skill", Usage: "print the skill"}},
	}
}

func render(t *testing.T, s Spec) string {
	t.Helper()
	var b strings.Builder
	if err := Write(&b, s); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return b.String()
}

func TestWrite_Frontmatter(t *testing.T) {
	got := render(t, sampleSpec())
	lines := strings.Split(got, "\n")
	if len(lines) < 4 || lines[0] != "---" || lines[1] != "name: doktunnel" || lines[3] != "---" {
		t.Fatalf("frontmatter must be ---, name, description, ---; got:\n%s", strings.Join(lines[:min(len(lines), 5)], "\n"))
	}
	desc, ok := strings.CutPrefix(lines[2], "description: ")
	if !ok {
		t.Fatalf("second frontmatter field is not description: %q", lines[2])
	}
	// The description contains ": ", so it must be quoted to be valid YAML.
	if !strings.HasPrefix(desc, `"`) || !strings.HasSuffix(desc, `"`) {
		t.Errorf("description is not a quoted YAML scalar: %s", desc)
	}
	if !strings.Contains(desc, "Trigger:") {
		t.Errorf("description does not say when to use the skill: %s", desc)
	}
}

func TestWrite_DescribesSpec(t *testing.T) {
	got := render(t, sampleSpec())
	for _, want := range []string{
		"doktunnel forwards things.",
		"| `doktunnel status` | Show active forwards |",
		"| `doktunnel widget spin` | Spin a widget |",
		"Spins until stopped.",
		"| `--json` | print JSON |",
		`| ` + "`--color <mode>`" + ` | use auto\|always\|never colors |`,
		"| `--skill` | print the skill |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not contain %q:\n%s", want, got)
		}
	}
}

func TestWrite_ListsEveryErrorCode(t *testing.T) {
	got := render(t, sampleSpec())
	for _, code := range clierr.Codes() {
		row := fmt.Sprintf("| %d | `%s` | %s |", code.ExitCode(), code, meanings[code])
		if !strings.Contains(got, row) {
			t.Errorf("error table is missing row %q", row)
		}
	}
}

func TestMeanings_CoverEveryCode(t *testing.T) {
	for _, code := range clierr.Codes() {
		if meanings[code] == "" {
			t.Errorf("error code %q has no meaning for the skill; add it to meanings", code)
		}
	}
}

func TestWrite_CommandUsageAndFlags(t *testing.T) {
	s := sampleSpec()
	s.Commands = append(s.Commands, Command{
		Path:    "doktunnel widget paint",
		Summary: "Paint a widget",
		Args:    "<name>",
		Flags:   []Flag{{Name: "color", Arg: "name", Usage: "paint color"}, {Name: "dry-run", Usage: "only print"}},
	})
	got := render(t, s)
	for _, want := range []string{
		"### `doktunnel widget paint`\n\nUsage: `doktunnel widget paint [flags] <name>`\n\n",
		"| `--color <name>` | paint color |\n| `--dry-run` | only print |\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not contain %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "### `doktunnel status`") {
		t.Errorf("a command without description, args or flags must not get a section:\n%s", got)
	}
}
