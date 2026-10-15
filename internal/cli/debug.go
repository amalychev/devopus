package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	agentv1 "github.com/devcorb/devcorb/gen/devcorb/agent/v1"
	"github.com/devcorb/devcorb/internal/agent/client"
	"github.com/devcorb/devcorb/internal/agent/paths"
)

// agentStatus asks the running agent for its status; it does not start one.
func (a *app) agentStatus() (*agentv1.StatusResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := client.New(paths.Socket()).Status(ctx, connect.NewRequest(&agentv1.StatusRequest{}))
	if err != nil {
		return nil, rpcError(err)
	}
	return res.Msg, nil
}

// findEnv resolves an environment by ID or slug; empty means the base environment of
// the project in the current folder.
func (a *app) findEnv(st *agentv1.StatusResponse, arg string) (*agentv1.EnvironmentStatus, error) {
	if arg != "" {
		for _, e := range st.Environments {
			if e.Id == arg || e.Slug == arg {
				return e, nil
			}
		}
		return nil, fail(ExitInvalid, "no environment %q; 'devcorb status' lists them", arg)
	}
	res, _, err := a.load()
	if err != nil {
		return nil, err
	}
	if res.Workspace == nil {
		return nil, fail(ExitInvalid, "cannot read the project name from workspace.yaml")
	}
	for _, e := range st.Environments {
		if e.Project == res.Workspace.Project && e.Kind == agentv1.EnvironmentKind_ENVIRONMENT_KIND_BASE {
			return e, nil
		}
	}
	return nil, fail(ExitError, "%s is not up; run 'devcorb up'", res.Workspace.Project)
}

// findUnit resolves [env] <unit> arguments: one argument is a unit of the base
// environment of the current project, two are an environment and its unit.
func (a *app) findUnit(args []string) (*agentv1.StatusResponse, *agentv1.UnitStatus, error) {
	st, err := a.agentStatus()
	if err != nil {
		return nil, nil, err
	}
	envArg, unitArg := "", args[0]
	if len(args) == 2 {
		envArg, unitArg = args[0], args[1]
	}
	env, err := a.findEnv(st, envArg)
	if err != nil {
		return nil, nil, err
	}
	var names []string
	for _, u := range env.Units {
		if u.Name == unitArg {
			return st, u, nil
		}
		names = append(names, u.Name)
	}
	if len(names) == 0 {
		return nil, nil, fail(ExitError, "environment %s has no modules or services running yet", env.Id)
	}
	return nil, nil, fail(ExitInvalid, "%s has no %q; it has %s", env.Id, unitArg, strings.Join(names, ", "))
}

// docker runs the docker CLI against the agent's runtime with the terminal attached.
func (a *app) docker(st *agentv1.StatusResponse, args ...string) error {
	cmd := exec.Command("docker", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, a.stdout, a.stderr
	env := os.Environ()[:0:0]
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "DOCKER_HOST=") && !strings.HasPrefix(kv, "DOCKER_CONTEXT=") {
			env = append(env, kv)
		}
	}
	if ep := st.GetRuntime().GetEndpoint(); ep != "" {
		env = append(env, "DOCKER_HOST="+ep)
	}
	cmd.Env = env
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return silent(ee.ExitCode())
	}
	if err != nil {
		return fail(ExitError, "docker: %v", err)
	}
	return nil
}

func (a *app) logsCmd() *cobra.Command {
	var follow bool
	var tail int
	cmd := &cobra.Command{
		Use:   "logs [env] <module|service>",
		Short: "Show the output of a module, worker or service",
		Long: "Shows the output of a module, worker or service. With one argument it is a unit of\n" +
			"the base environment of the project in this folder.\n" +
			"Hooks and builds log to ~/.devcorb/envs/<env>/logs.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, u, err := a.findUnit(args)
			if err != nil {
				return err
			}
			dargs := []string{"logs"}
			if follow {
				dargs = append(dargs, "--follow")
			}
			if tail >= 0 {
				dargs = append(dargs, "--tail", strconv.Itoa(tail))
			}
			return a.docker(st, append(dargs, u.Container)...)
		},
	}
	// No -f: it is --file everywhere.
	cmd.Flags().BoolVar(&follow, "follow", false, "keep printing new output")
	cmd.Flags().IntVarP(&tail, "tail", "n", -1, "only the last lines")
	return cmd
}

func (a *app) execCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "exec [env] <module|service> [-- command…]",
		Short: "Run a command in a module or service container (default: a shell)",
		Long: "Runs a command in the container of a module, worker or service. With one argument\n" +
			"before -- it is a unit of the base environment of the project in this folder.\n\n" +
			"  devcorb exec api -- php artisan tinker",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, command := args, []string{"sh"}
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				target, command = args[:dash], args[dash:]
				if len(command) == 0 {
					command = []string{"sh"}
				}
			}
			if len(target) < 1 || len(target) > 2 {
				return fail(ExitInvalid, "usage: devcorb exec [env] <module|service> [-- command…]")
			}
			st, u, err := a.findUnit(target)
			if err != nil {
				return err
			}
			if u.State != "running" && u.State != "starting" {
				return fail(ExitError, "%s is %s", u.Name, u.State)
			}
			dargs := []string{"exec", "-i"}
			if isTerminal(os.Stdin) && isTerminal(os.Stdout) {
				dargs = append(dargs, "-t")
			}
			return a.docker(st, append(append(dargs, u.Container), command...)...)
		},
	}
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
