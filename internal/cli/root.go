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
	file           string
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
  devopus register                create an account (or 'devopus login')
  devopus workspace create acme   a workspace for the team; you are its admin
  devopus invite create <email> --role teamlead --project shop
  devopus agent install           run the agent as a service and check this machine
  devopus init                    create workspace.yaml from docker-compose.yml
  devopus up                      start the project's main environment
  devopus feature create PAY-1    an environment of its own for a branch
  devopus panel open              the web panel`,
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
	root.PersistentFlags().StringVarP(&a.file, "file", "f", "", "workspace file (default: workspace.yaml in this or a parent directory)")
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
		&cobra.Group{ID: "config", Title: "workspace.yaml:"},
	)
	add := func(group string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = group
			root.AddCommand(c)
		}
	}
	add("account", a.registerCmd(), a.loginCmd(), a.logoutCmd(), a.whoamiCmd(), a.authCmd())
	add("team", a.workspaceCmd(), a.projectCmd(), a.appCmd(), a.serviceCmd(), a.inviteCmd(), a.memberCmd())
	add("machine", a.agentCmd(), a.panelCmd(), a.serverCmd(), a.statusCmd(), a.doctorCmd(), a.proxyCmd())
	add("project", a.upCmd(), a.downCmd(), a.openCmd(), a.logsCmd(), a.execCmd(), a.routesCmd())
	add("features", a.featureCmd(), a.dbCmd())
	add("config", a.initCmd(), a.validateCmd(), a.exportCmd(), a.schemaCmd())
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

// workspaceFile finds the workspace file: the --file flag, or workspace.yaml in the
// current directory or the nearest parent.
func (a *app) workspaceFile() (string, error) {
	if a.file != "" {
		return a.file, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		p := filepath.Join(dir, workspace.FileName)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fail(ExitInvalid, "no %s here or in a parent directory; run 'devopus init' or pass --file", workspace.FileName)
		}
		dir = parent
	}
}

// load reads the workspace file and prints its diagnostics; it fails on errors.
func (a *app) load() (*workspace.Result, string, error) {
	file, err := a.workspaceFile()
	if err != nil {
		return nil, "", err
	}
	res, err := workspace.Load(file)
	if err != nil {
		return nil, file, fail(ExitInvalid, "%v", err)
	}
	return res, file, nil
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
