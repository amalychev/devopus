// Command docklet is the CLI (and, from milestone M2, the agent) of Docklet.
package main

import (
	"os"

	"github.com/docklet/docklet/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
