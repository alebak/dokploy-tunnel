// Command doktunnel-companion is a placeholder entrypoint; only --version is implemented.
package main

import (
	"fmt"
	"os"

	"github.com/alebak/dokploy-tunnel/internal/version"
)

const binaryName = "doktunnel-companion"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(version.String(binaryName))
		return
	}
	fmt.Fprintf(os.Stderr, "%s: not implemented yet (only --version is available)\n", binaryName)
	os.Exit(1)
}
