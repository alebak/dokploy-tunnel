package cli

// NewRoot returns the doktunnel command tree.
func NewRoot() *Command {
	return &Command{
		Name:    "doktunnel",
		Summary: "doktunnel forwards Dokploy services to stable local hostnames and ports.",
		Subcommands: []*Command{
			newContextCommand(),
			newServicesCommand(),
			newForwardCommand(),
			newStatusCommand(),
			newHostsCommand(),
		},
	}
}
