// Command devopus is the CLI (and, from milestone M2, the agent) of Devopus.
package main

import (
	"os"

	"github.com/devopus/devopus/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
