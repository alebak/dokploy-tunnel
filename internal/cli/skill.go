package cli

import (
	"flag"
	"io"
	"strings"

	"github.com/alebak/dokploy-tunnel/internal/skill"
)

// rootFlags holds the flags accepted only by the root command.
type rootFlags struct {
	version bool
	skill   bool
}

// bindRootFlags defines the root-only flags on fs, storing values in r.
func bindRootFlags(fs *flag.FlagSet, r *rootFlags) {
	fs.BoolVar(&r.version, "version", false, "print the version and exit")
	fs.BoolVar(&r.skill, "skill", false, "print a SKILL.md that teaches AI agents to use this CLI, and exit")
}

// SkillSpec describes root, its command tree and its flags for the agent
// skill generator.
func SkillSpec(root *Command) skill.Spec {
	global := flag.NewFlagSet(root.Name, flag.ContinueOnError)
	BindGlobalFlags(global, &Globals{})
	rootOnly := flag.NewFlagSet(root.Name, flag.ContinueOnError)
	bindRootFlags(rootOnly, &rootFlags{})

	return skill.Spec{
		Name:        root.Name,
		Summary:     root.Summary,
		Commands:    skillCommands(nil, []string{root.Name}, root.Subcommands),
		GlobalFlags: skillFlags(global),
		RootFlags:   skillFlags(rootOnly),
	}
}

// skillCommands appends cmds and their descendants to dst, depth first.
func skillCommands(dst []skill.Command, path []string, cmds []*Command) []skill.Command {
	for _, c := range cmds {
		if c.Hidden {
			continue
		}
		p := append(path[:len(path):len(path)], c.Name)
		var flags []skill.Flag
		if c.Flags != nil {
			fs := flag.NewFlagSet(c.Name, flag.ContinueOnError)
			c.Flags(fs)
			flags = skillFlags(fs)
		}
		dst = append(dst, skill.Command{
			Path:        strings.Join(p, " "),
			Summary:     c.Summary,
			Description: c.Description,
			Args:        c.Args,
			Flags:       flags,
		})
		dst = skillCommands(dst, p, c.Subcommands)
	}
	return dst
}

// skillFlags lists the flags defined on fs, sorted by name.
func skillFlags(fs *flag.FlagSet) []skill.Flag {
	var flags []skill.Flag
	fs.VisitAll(func(f *flag.Flag) {
		// UnquoteUsage returns an empty value name for boolean flags.
		arg, usage := flag.UnquoteUsage(f)
		flags = append(flags, skill.Flag{Name: f.Name, Arg: arg, Usage: usage})
	})
	return flags
}

// writeSkill prints the agent skill for the tree rooted at root.
func writeSkill(w io.Writer, root *Command) error {
	return skill.Write(w, SkillSpec(root))
}
