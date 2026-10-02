// Command doktunnel is the doktunnel CLI; the command tree lives in internal/cli.
package main

import (
	"os"

	"github.com/alebak/dokploy-tunnel/internal/cli"
	"github.com/alebak/dokploy-tunnel/internal/keyring"
	"github.com/alebak/dokploy-tunnel/internal/prompt"
)

func main() {
	app := &cli.App{
		Root:            cli.NewRoot(),
		Stdin:           os.Stdin,
		Stdout:          os.Stdout,
		Stderr:          os.Stderr,
		StdinIsTerminal: prompt.IsTerminal(os.Stdin),
		Keyring:         keyring.System(),
		ReadSecret:      cli.TerminalSecret(os.Stdin),
		Getenv:          os.Getenv,
	}
	os.Exit(app.Run(os.Args[1:]))
}
