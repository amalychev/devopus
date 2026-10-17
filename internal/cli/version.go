package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

func (a *app) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.json {
				return a.writeJSON(map[string]any{
					"version":  Version,
					"go":       runtime.Version(),
					"platform": runtime.GOOS + "/" + runtime.GOARCH,
				})
			}
			fmt.Fprintf(a.stdout, "devopus %s (%s, %s/%s)\n", Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
			return nil
		},
	}
}

// plural is a count with a word: 1 application, 3 applications.
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
