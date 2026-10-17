// Package cli implements the devopus command line (specs/cli.md).
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/amalychev/devopus/internal/workspace"
)

// Exit codes of specs/cli.md.
const (
	ExitOK           = 0
	ExitError        = 1
	ExitInvalid      = 2 // bad arguments or configuration
	ExitConfirm      = 3 // confirmation needed in a non-interactive run
	ExitPreflightErr = 4
	ExitAuth         = 5 // not signed in, or not allowed
)

// Version is set by the release build.
var Version = "dev"

// exitError carries an exit code; its message, if any, was already printed or is printed by Run.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func fail(code int, format string, a ...any) error {
	return &exitError{code: code, msg: fmt.Sprintf(format, a...)}
}

// silent exits with a code after the command printed its own output.
func silent(code int) error { return &exitError{code: code} }

type app struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	json           bool
	// openURL opens a link in the browser.
	openURL func(string) error
}

// Run executes the command line and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	return execute(&app{stdin: os.Stdin, stdout: stdout, stderr: stderr, openURL: openBrowser}, args)
}

func execute(a *app, args []string) int {
	stdout, stderr := a.stdout, a.stderr
	root := &cobra.Command{
		Use:   "devopus",
		Short: "Local environments for every feature branch",
		Long: `Devopus gives every feature branch its own environment on this machine: its own
containers, data and URL (https://pay-123.shop.localhost), next to the main one.

The devopus command controls all of it: your account, the team's workspace with its
members and projects, the agent and the web panel on this machine, environments and
their features.

Get started:
  devopus register                       create an account (or 'devopus login')
  devopus workspace create acme          a workspace for the team; you are its admin
  devopus project create shop --from-compose docker-compose.yml
  devopus invite create <email> --role developer --project shop
  devopus agent install                  run the agent as a service; it brings the projects up
  devopus feature create PAY-1 --apps api   an environment of its own for a branch
  devopus panel open                     the web panel

Try it alone: devopus workspace demo, then devopus up demo --dir examples/demo.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return requireSignIn(cmd)
		},
	}
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.PersistentFlags().BoolVar(&a.json, "json", false, "machine-readable output")
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return fail(ExitInvalid, "%v\nRun '%s --help' for usage.", err, c.CommandPath())
	})

	// Commands keep the order below: the most used first.
	cobra.EnableCommandSorting = false
	root.AddGroup(
		&cobra.Group{ID: "account", Title: "Your account:"},
		&cobra.Group{ID: "team", Title: "Workspaces, projects and members:"},
		&cobra.Group{ID: "machine", Title: "This machine: the agent, the panel and what they need:"},
		&cobra.Group{ID: "project", Title: "Projects and their main environment:"},
		&cobra.Group{ID: "features", Title: "Features and data:"},
	)
	add := func(group string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = group
			root.AddCommand(c)
		}
	}
	add("account", a.registerCmd(), a.loginCmd(), a.logoutCmd(), a.whoamiCmd(), a.authCmd())
	add("team", a.workspaceCmd(), a.projectCmd(), a.appCmd(), a.serviceCmd(), a.secretsCmd(), a.driftCmd(), a.changesCmd(), a.inviteCmd(), a.memberCmd())
	add("machine", a.agentCmd(), a.panelCmd(), a.serverCmd(), a.statusCmd(), a.doctorCmd(), a.proxyCmd())
	add("project", a.upCmd(), a.downCmd(), a.openCmd(), a.logsCmd(), a.execCmd(), a.routesCmd())
	add("features", a.featureCmd(), a.dbCmd())
	root.AddCommand(a.versionCmd())

	err := root.Execute()
	if err == nil {
		return ExitOK
	}
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.msg != "" {
			fmt.Fprintln(stderr, "devopus: "+ee.msg)
		}
		return ee.code
	}
	fmt.Fprintln(stderr, "devopus: "+err.Error())
	// cobra reports unknown commands and arguments as plain errors.
	return ExitInvalid
}

// display shortens a path for messages: relative to the working directory when it is
// below it or close by.
func display(file string) string {
	wd, err := os.Getwd()
	if err != nil {
		return file
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return file
	}
	r, err := filepath.Rel(wd, abs)
	if err != nil || strings.HasPrefix(r, "../..") {
		return abs
	}
	return r
}

func (a *app) printDiagnostics(file string, ds workspace.Diagnostics) {
	rel := display(file)
	for _, d := range ds {
		loc := rel
		if d.Line > 0 {
			loc = fmt.Sprintf("%s:%d", rel, d.Line)
		}
		path := ""
		if d.Path != "" {
			path = d.Path + ": "
		}
		fmt.Fprintf(a.stderr, "%s: %s: %s%s\n", loc, d.Severity, path, d.Message)
	}
}

func (a *app) writeJSON(v any) error {
	enc := json.NewEncoder(a.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
