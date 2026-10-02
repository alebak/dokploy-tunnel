package cli

import "github.com/alebak/dokploy-tunnel/internal/clierr"

// NewRoot returns the doktunnel command tree.
func NewRoot() *Command {
	return &Command{
		Name:    "doktunnel",
		Summary: "doktunnel forwards Dokploy services to stable local hostnames and ports.",
		Subcommands: []*Command{
			newContextCommand(),
			{
				Name:    "services",
				Summary: "List Dokploy services that can be forwarded",
				Run:     notImplemented("services"),
			},
			{
				Name:    "forward",
				Summary: "Forward a Dokploy service to a stable local hostname and port",
				Run:     notImplemented("forward"),
			},
			{
				Name:    "status",
				Summary: "Show active forwards",
				Run:     notImplemented("status"),
			},
			{
				Name:    "hosts",
				Summary: "Manage local hostname entries for forwarded services",
				Run:     notImplemented("hosts"),
			},
		},
	}
}

// notImplemented returns a Run function for a command that exists in the tree
// but has no behavior yet.
func notImplemented(name string) func(*Env, []string) error {
	return func(*Env, []string) error {
		return clierr.Newf(clierr.NotImplemented, "%s is not implemented yet", name)
	}
}
