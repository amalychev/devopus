package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	agentv1 "github.com/devopus/devopus/gen/devopus/agent/v1"
	"github.com/devopus/devopus/internal/agent/client"
	"github.com/devopus/devopus/internal/agent/paths"
	"github.com/devopus/devopus/internal/agent/stack"
	"github.com/devopus/devopus/internal/workspace"
)

func (a *app) featureCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "feature",
		Short: "Environments for feature branches: create, add modules, list, remove",
	}
	cmd.AddCommand(a.featureCreateCmd(), a.featureAddCmd(), a.featureListCmd(), a.featureRmCmd(), a.featurePathCmd())
	return cmd
}

// loadValid reads workspace.yaml for the agent: errors stop the command.
func (a *app) loadValid(again string) (*workspace.Workspace, string, []byte, error) {
	res, file, err := a.load()
	if err != nil {
		return nil, "", nil, err
	}
	a.printDiagnostics(file, res.Diagnostics.Warnings())
	if res.Diagnostics.HasErrors() {
		a.printDiagnostics(file, res.Diagnostics.Errors())
		return nil, "", nil, fail(ExitInvalid, "fix the errors above, then run '%s' again", again)
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, "", nil, err
	}
	config, err := os.ReadFile(abs)
	if err != nil {
		return nil, "", nil, err
	}
	return res.Workspace, abs, config, nil
}

func splitList(list []string) []string {
	var out []string
	for _, s := range list {
		for _, p := range strings.Split(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func (a *app) featureCreateCmd() *cobra.Command {
	var modules []string
	var branch string
	var detach bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "create <name> --modules a,b",
		Short: "Create a feature that forks some modules on its own branch and URL",
		Long: "Creates a feature environment: the forked modules get worktrees on the feature branch\n" +
			"(features.branch_prefix + name, or --branch), the modules that use them run in the\n" +
			"feature too, and everything else comes from the base environment.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			modules = splitList(modules)
			if len(modules) == 0 {
				return fail(ExitInvalid, "name the modules to fork: --modules a,b")
			}
			ws, abs, config, err := a.loadValid("devopus feature create")
			if err != nil {
				return err
			}
			iso, err := stack.Feature(ws, modules)
			if err != nil {
				return fail(ExitInvalid, "%v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			c, err := a.connect(ctx)
			if err != nil {
				return err
			}
			w, err := watch(ctx, c)
			if err != nil {
				return err
			}
			defer w.Close()
			res, err := c.CreateFeature(ctx, connect.NewRequest(&agentv1.CreateFeatureRequest{
				WorkspacePath: abs, Config: config, Name: name, Modules: modules, Branch: branch,
			}))
			if err != nil {
				return rpcError(err)
			}
			r := res.Msg
			if !a.json {
				a.printIsolation(ws, iso, name, r.Branch)
			}
			if detach {
				return a.done(map[string]any{"environment_id": r.EnvironmentId, "slug": r.Slug, "branch": r.Branch, "revision": r.Revision},
					"Feature %s is coming up; 'devopus feature list' shows it.\n", name)
			}
			return a.awaitRunning(ctx, c, w, r.EnvironmentId, r.Revision, "Feature "+name)
		},
	}
	cmd.Flags().StringSliceVarP(&modules, "modules", "m", nil, "modules to fork, comma-separated")
	cmd.Flags().StringVar(&branch, "branch", "", "branch of the forked modules (default: features.branch_prefix + name)")
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "return without waiting for the feature")
	cmd.Flags().DurationVar(&timeout, "timeout", 15*time.Minute, "how long to wait")
	return cmd
}

// printIsolation tells what the feature has to itself and what it shares with the
// base environment, with a hint where shared data may surprise (US-4).
func (a *app) printIsolation(ws *workspace.Workspace, iso *stack.Isolation, name, branch string) {
	w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "Feature %s of %s\n", name, ws.Project)
	fmt.Fprintf(w, "  forks\t%s (branch %s)\n", strings.Join(iso.Forked, ", "), branch)
	if d := iso.Dependents(); len(d) > 0 {
		fmt.Fprintf(w, "  also runs\t%s: they use forked modules, on the base branch\n", strings.Join(d, ", "))
	}
	var copies, fresh []string
	for _, svc := range iso.Services {
		if stack.Engine(ws.Services[svc].Image) != "" {
			copies = append(copies, svc)
		} else {
			fresh = append(fresh, svc)
		}
	}
	if len(copies) > 0 {
		fmt.Fprintf(w, "  copies\t%s: main's data, the feature's migrations run on the copy\n", strings.Join(copies, ", "))
	}
	if len(fresh) > 0 {
		fmt.Fprintf(w, "  own\t%s: empty, its own instance\n", strings.Join(fresh, ", "))
	}
	if shared := append(append([]string{}, iso.SharedModules...), iso.SharedServices...); len(shared) > 0 {
		fmt.Fprintf(w, "  shared\t%s from the base environment\n", strings.Join(shared, ", "))
	}
	w.Flush()
	for _, m := range iso.NoMigrate {
		fmt.Fprintf(a.stdout, "  ! %s writes to data of the base environment; its migrations are skipped (features.database: shared)\n", m)
	}
	for _, m := range iso.SharedModules {
		var data []string
		for _, u := range ws.Modules[m].Uses {
			if ws.Services[u] != nil {
				data = append(data, u)
			}
		}
		if len(data) > 0 {
			fmt.Fprintf(a.stdout, "  ! the feature calls %s of the base environment, which writes to %s there; fork it to keep the feature's data apart: devopus feature add %s %s\n",
				m, strings.Join(data, ", "), name, m)
		}
	}
}

func (a *app) featureAddCmd() *cobra.Command {
	var detach bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "add <feature> <module>...",
		Short: "Fork more modules in a feature",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, abs, config, err := a.loadValid("devopus feature add")
			if err != nil {
				return err
			}
			modules := splitList(args[1:])
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			c, err := a.connect(ctx)
			if err != nil {
				return err
			}
			w, err := watch(ctx, c)
			if err != nil {
				return err
			}
			defer w.Close()
			res, err := c.AddFeatureModules(ctx, connect.NewRequest(&agentv1.AddFeatureModulesRequest{
				WorkspacePath: abs, Config: config, Feature: args[0], Modules: modules,
			}))
			if err != nil {
				return rpcError(err)
			}
			if detach {
				return a.done(map[string]any{"environment_id": res.Msg.EnvironmentId, "revision": res.Msg.Revision},
					"Feature %s of %s is changing; 'devopus feature list' shows it.\n", args[0], ws.Project)
			}
			return a.awaitRunning(ctx, c, w, res.Msg.EnvironmentId, res.Msg.Revision, "Feature "+args[0])
		},
	}
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "return without waiting for the feature")
	cmd.Flags().DurationVar(&timeout, "timeout", 15*time.Minute, "how long to wait")
	return cmd
}

// project is the project of the current folder, or the --project flag.
func (a *app) project(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	res, _, err := a.load()
	if err != nil {
		return "", err
	}
	if res.Workspace == nil || res.Workspace.Project == "" {
		return "", fail(ExitInvalid, "cannot read the project name from workspace.yaml; pass --project")
	}
	return res.Workspace.Project, nil
}

func features(st *agentv1.StatusResponse, project string) []*agentv1.EnvironmentStatus {
	var out []*agentv1.EnvironmentStatus
	for _, e := range st.Environments {
		if e.Kind == agentv1.EnvironmentKind_ENVIRONMENT_KIND_FEATURE && (project == "" || e.Project == project) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		return out[i].Slug < out[j].Slug
	})
	return out
}

// findFeature resolves a feature by name, slug or ID within a project.
func findFeature(st *agentv1.StatusResponse, project, key string) (*agentv1.EnvironmentStatus, error) {
	list := features(st, project)
	var names []string
	for _, e := range list {
		if strings.EqualFold(e.Name, key) || e.Slug == key || e.Id == key {
			return e, nil
		}
		names = append(names, e.Name)
	}
	if len(names) == 0 {
		return nil, fail(ExitInvalid, "%s has no features; 'devopus feature create' makes one", project)
	}
	return nil, fail(ExitInvalid, "%s has no feature %q; it has %s", project, key, strings.Join(names, ", "))
}

func (a *app) featureListCmd() *cobra.Command {
	var project string
	var all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the features of the project",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !all {
				var err error
				if project, err = a.project(project); err != nil {
					return err
				}
			}
			st, err := a.agentStatus()
			if err != nil {
				return err
			}
			list := features(st, project)
			if a.json {
				out := &agentv1.StatusResponse{Environments: list}
				return a.writeProto(out)
			}
			if len(list) == 0 {
				fmt.Fprintln(a.stdout, "No features. 'devopus feature create <name> --modules a,b' makes one.")
				return nil
			}
			w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "PROJECT\tFEATURE\tPHASE\tURL\tFORKED\tBRANCH\tSHARED")
			for _, e := range list {
				phase := phaseName(e.Phase)
				if e.Error != "" {
					phase += ": " + firstLine(e.Error)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.Project, e.Name, phase, dash(e.Url),
					dash(strings.Join(e.ForkedModules, ",")), dash(e.Branch), dash(strings.Join(e.Shared, ",")))
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name (default: from workspace.yaml)")
	cmd.Flags().BoolVar(&all, "all", false, "features of every project")
	return cmd
}

func (a *app) featureRmCmd() *cobra.Command {
	var project string
	var force bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "rm <feature>",
		Short: "Remove a feature with its containers, data and worktrees",
		Long: "Removes a feature. Without --force it refuses while a worktree has uncommitted changes\n" +
			"or commits that are on no remote and not in the base branch. The branch stays either way.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, err := a.project(project)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			c := client.New(paths.Socket())
			if err := client.Ping(ctx, c); err != nil {
				return fail(ExitError, "%v", err)
			}
			w, err := watch(ctx, c)
			if err != nil {
				return err
			}
			defer w.Close()
			res, err := c.RemoveFeature(ctx, connect.NewRequest(&agentv1.RemoveFeatureRequest{Project: project, Feature: args[0], Force: force}))
			if err != nil {
				var ce *connect.Error
				if errors.As(err, &ce) && ce.Code() == connect.CodeFailedPrecondition {
					return fail(ExitError, "%s", ce.Message())
				}
				return rpcError(err)
			}
			id := res.Msg.EnvironmentId
			if _, err := a.follow(w, []string{id}, res.Msg.Revision, false, func(s *agentv1.EnvironmentStatus) bool { return s.Removed }); err != nil {
				return err
			}
			return a.done(map[string]any{"environment_id": id, "revision": res.Msg.Revision}, "Feature %s of %s is removed.\n", args[0], project)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name (default: from workspace.yaml)")
	cmd.Flags().BoolVar(&force, "force", false, "remove even with uncommitted or unpushed work")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "how long to wait")
	return cmd
}

func (a *app) featurePathCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "path <feature> [module]",
		Short: "Print the folder of a feature's code, for IDEs and AI agents",
		Long: "Prints the feature's folder of the project in its worktree, or the folder of one module.\n" +
			"For example: code $(devopus feature path PAY-123)",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, err := a.project(project)
			if err != nil {
				return err
			}
			st, err := a.agentStatus()
			if err != nil {
				return err
			}
			e, err := findFeature(st, project, args[0])
			if err != nil {
				return err
			}
			if len(e.Sources) == 0 {
				return fail(ExitError, "feature %s has no worktrees yet; 'devopus feature list' shows where it is", e.Name)
			}
			var path string
			switch {
			case len(args) == 2:
				var names []string
				for _, s := range e.Sources {
					if s.Module == args[1] {
						path = s.Path
					}
					names = append(names, s.Module)
				}
				if path == "" {
					return fail(ExitInvalid, "feature %s does not run %q; it runs %s", e.Name, args[1], strings.Join(names, ", "))
				}
			case e.CodeRoot != "":
				path = e.CodeRoot
			case len(e.Sources) == 1:
				path = e.Sources[0].Path
			default:
				var names []string
				for _, s := range e.Sources {
					names = append(names, s.Module)
				}
				return fail(ExitInvalid, "the modules of %s are in separate repositories; name one: %s", e.Name, strings.Join(names, ", "))
			}
			return a.done(map[string]any{"path": path}, "%s\n", path)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name (default: from workspace.yaml)")
	return cmd
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
