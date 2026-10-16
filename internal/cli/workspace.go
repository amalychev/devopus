package cli

import (
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/devopus/devopus/internal/compose"
	"github.com/devopus/devopus/internal/workspace"
	"github.com/devopus/devopus/schema"
)

func (a *app) validateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Check workspace.yaml against the schema, the rules and known anti-patterns",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, file, err := a.load()
			if err != nil {
				return err
			}
			ds := res.Diagnostics
			missing, err := missingSecrets(file)
			if err != nil {
				return fail(ExitError, "%v", err)
			}
			if a.json {
				if err := a.writeJSON(map[string]any{
					"file":            file,
					"valid":           !ds.HasErrors(),
					"diagnostics":     nonNil(ds),
					"missing_secrets": nonNilStrings(missing),
				}); err != nil {
					return err
				}
			} else {
				a.printDiagnostics(file, ds)
				if len(missing) > 0 {
					fmt.Fprintf(a.stderr, "%s: warning: %s; 'devopus up' fails without them\n", display(file), secretsHint(file, missing))
				}
				if !ds.HasErrors() {
					fmt.Fprintf(a.stdout, "%s is valid", display(file))
					if n := len(ds.Warnings()); n > 0 {
						fmt.Fprintf(a.stdout, " (%d warnings)", n)
					}
					fmt.Fprintln(a.stdout)
				}
			}
			if ds.HasErrors() {
				return silent(ExitInvalid)
			}
			return nil
		},
	}
}

func (a *app) exportCmd() *cobra.Command {
	export := &cobra.Command{
		Use:   "export",
		Short: "Export workspace.yaml to other formats",
	}
	var out string
	composeCmd := &cobra.Command{
		Use:   "compose",
		Short: "Export the base environment as docker-compose.yml",
		Long: "Writes a docker-compose file for the base environment: services, modules with build,\n" +
			"depends_on and env with values. Features, routing and database clones stay with Devopus.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, file, err := a.load()
			if err != nil {
				return err
			}
			if res.Diagnostics.HasErrors() {
				a.printDiagnostics(file, res.Diagnostics)
				return fail(ExitInvalid, "fix the errors above before exporting")
			}
			data, err := compose.Export(res.Workspace)
			if err != nil {
				return fail(ExitError, "%v", err)
			}
			if out == "" || out == "-" {
				_, err = a.stdout.Write(data)
				return err
			}
			if err := os.WriteFile(out, data, 0o644); err != nil {
				return err
			}
			fmt.Fprintf(a.stderr, "Wrote %s\n", out)
			return nil
		},
	}
	composeCmd.Flags().StringVarP(&out, "output", "o", "", "output file (default: stdout)")
	export.AddCommand(composeCmd)
	return export
}

func (a *app) schemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Print the JSON Schema of workspace.yaml",
		Long: "Prints the JSON Schema of workspace.yaml v1. Editors with yaml-language-server pick it up\n" +
			"from the first line of workspace.yaml that 'devopus init' writes:\n  # yaml-language-server: $schema=" + schema.URL,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := a.stdout.Write(schema.WorkspaceV1)
			return err
		},
	}
}

func (a *app) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.json {
				return a.writeJSON(map[string]any{
					"version":        Version,
					"go":             runtime.Version(),
					"platform":       runtime.GOOS + "/" + runtime.GOARCH,
					"format_version": workspace.Version,
				})
			}
			fmt.Fprintf(a.stdout, "devopus %s (%s, %s/%s)\n", Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
			return nil
		},
	}
}

func nonNil(ds workspace.Diagnostics) workspace.Diagnostics {
	if ds == nil {
		return workspace.Diagnostics{}
	}
	return ds
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
