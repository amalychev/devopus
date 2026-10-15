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

	"github.com/devopus/devopus/internal/workspace"
)

// Exit codes of specs/cli.md.
const (
	ExitOK           = 0
	ExitError        = 1
	ExitInvalid      = 2 // bad arguments or configuration
	ExitConfirm      = 3 // confirmation needed in a non-interactive run
	ExitPreflightErr = 4
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
	stdout, stderr io.Writer
	json           bool
	file           string
}

// Run executes the command line and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	a := &app{stdout: stdout, stderr: stderr}
	root := &cobra.Command{
		Use:           "devopus",
		Short:         "Local environments for every feature branch",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.PersistentFlags().BoolVar(&a.json, "json", false, "machine-readable output")
	root.PersistentFlags().StringVarP(&a.file, "file", "f", "", "workspace file (default: workspace.yaml in this or a parent directory)")
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return fail(ExitInvalid, "%v\nRun '%s --help' for usage.", err, c.CommandPath())
	})

	root.AddCommand(
		a.initCmd(),
		a.validateCmd(),
		a.exportCmd(),
		a.schemaCmd(),
		a.versionCmd(),
		a.agentCmd(),
		a.statusCmd(),
		a.upCmd(),
		a.downCmd(),
		a.doctorCmd(),
		a.routesCmd(),
		a.openCmd(),
		a.logsCmd(),
		a.execCmd(),
		a.proxyCmd(),
	)

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
