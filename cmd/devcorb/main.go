// Command devcorb is the CLI (and, from milestone M2, the agent) of Devcorb.
package main

import (
	"os"

	"github.com/devcorb/devcorb/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
