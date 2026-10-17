package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	agentv2 "github.com/amalychev/devopus/gen/devopus/agent/v2"
	workspacev1 "github.com/amalychev/devopus/gen/devopus/workspace/v1"
	"github.com/amalychev/devopus/gen/devopus/workspace/v1/workspacev1connect"
	"github.com/amalychev/devopus/internal/catalog"
	"github.com/amalychev/devopus/internal/compose"
	"github.com/amalychev/devopus/internal/importer"
	"github.com/amalychev/devopus/internal/spec"
	"github.com/amalychev/devopus/internal/workspace"
)

// Projects, their applications and services live on the workspace server in versions
// (R6). A change reads the current settings, edits one application or service, and
// sends it with the version it read: if someone changed the project meanwhile, it
// reads again and retries.

// inProject is a project of the current workspace with its current settings.
type inProject struct {
	s       *session
	ws      *current
	c       workspacev1connect.WorkspaceServiceClient
	name    string
	cfg     *spec.Project
	version int32
}

func (a *app) remoteProject(ctx context.Context, name string) (*inProject, error) {
	s, cur, c, err := a.inWorkspace(ctx)
	if err != nil {
		return nil, err
	}
	p := &inProject{s: s, ws: cur, c: c, name: name}
	return p, p.reload(ctx)
}

func (p *inProject) reload(ctx context.Context) error {
	res, err := p.c.GetProjectConfig(ctx, req(p.s, &workspacev1.GetProjectConfigRequest{WorkspaceId: p.ws.ID, Project: p.name}))
	if err != nil {
		return rpcErr(err)
	}
	cfg, err := spec.ParseYAML([]byte(res.Msg.Config.Yaml))
	if err != nil {
		return fail(ExitError, "%v", err)
	}
	p.cfg, p.version = cfg, res.Msg.Config.Version
	return nil
}

// edit runs a change made against the settings it read, rereading them when someone
// else changed the project meanwhile.
func (p *inProject) edit(ctx context.Context, change func(base int32) (*workspacev1.ProjectConfig, error)) (*workspacev1.ProjectConfig, error) {
	for try := 0; ; try++ {
		conf, err := change(p.version)
		if connect.CodeOf(err) == connect.CodeAborted && try < 3 {
			if err := p.reload(ctx); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, rpcErr(err)
		}
		return conf, nil
	}
}

func toYAML(v any) string {
	b, err := yaml.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (a *app) changed(conf *workspacev1.ProjectConfig, format string, args ...any) error {
	return a.done(map[string]any{"version": conf.Version, "message": conf.Message},
		format+" (settings version %d)\n", append(args, conf.Version)...)
}

func (a *app) projectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Projects: their settings, versions and rollback",
		Long: "A project is a product of the workspace: its applications (repositories) and services\n" +
			"(php, mysql, redis…) attached to them. Its settings live on the workspace server in\n" +
			"versions: every change is a new version, any version can be made current again.",
	}
	var domain string
	var from []string
	var noPin bool
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a project (admin, teamlead); --from-compose imports docker-compose.yml",
		Example: "  devopus project create shop\n" +
			"  devopus project create shop --from-compose docker-compose.yml --domain shop.localhost",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			config := ""
			var found map[string]string
			if len(from) > 0 {
				p, values, err := a.importCompose(ctx, args[0], from, noPin)
				if err != nil {
					return err
				}
				b, err := p.YAML()
				if err != nil {
					return err
				}
				config, found = string(b), values
			}
			s, cur, c, err := a.inWorkspace(ctx)
			if err != nil {
				return err
			}
			res, err := c.CreateProject(ctx, req(s, &workspacev1.CreateProjectRequest{WorkspaceId: cur.ID, Name: args[0], Domain: domain, ConfigYaml: config}))
			if err != nil {
				return rpcErr(err)
			}
			cfg, _ := spec.ParseYAML([]byte(res.Msg.Config.Yaml))
			if err := a.uploadSecrets(ctx, args[0], found); err != nil {
				fmt.Fprintf(a.stderr, "The project is created, but its secrets are not stored: %v\nSet them with 'devopus secrets set %s <NAME>'.\n", err, args[0])
			}
			return a.done(map[string]any{"id": res.Msg.Project.Id, "name": res.Msg.Project.Name, "domain": cfg.Domain, "version": res.Msg.Config.Version},
				"✓ Project %s created in %s: https://%s, %d applications, %d services.\n", res.Msg.Project.Name, cur.Name, cfg.Domain, len(cfg.Apps), len(cfg.Services))
		},
	}
	create.Flags().StringVar(&domain, "domain", "", "domain of its environments (default: <name>.localhost)")
	create.Flags().StringArrayVar(&from, "from-compose", nil, "compose file to import; repeat to merge overrides in order")
	create.Flags().BoolVar(&noPin, "no-pin", false, "do not resolve floating image tags of the import to digests")

	list := &cobra.Command{
		Use:   "list",
		Short: "Projects you have a role in (the admin sees all)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, cur, c, err := a.inWorkspace(cmd.Context())
			if err != nil {
				return err
			}
			res, err := c.ListProjects(cmd.Context(), req(s, &workspacev1.ListProjectsRequest{WorkspaceId: cur.ID}))
			if err != nil {
				return rpcErr(err)
			}
			if a.json {
				return a.writeProto(res.Msg)
			}
			if len(res.Msg.Projects) == 0 {
				fmt.Fprintln(a.stdout, "No projects yet: devopus project create <name>")
				return nil
			}
			w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "PROJECT\tYOUR ROLE\tVERSION\tCREATED")
			for _, p := range res.Msg.Projects {
				role := p.Role
				if role == "" {
					role = "admin"
				}
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", p.Name, role, p.Version, day(p.CreatedAt.AsTime()))
			}
			return w.Flush()
		},
	}

	show := &cobra.Command{
		Use:   "show <project>",
		Short: "A project: its domain, applications and services",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := a.remoteProject(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if a.json {
				return a.writeJSON(map[string]any{"version": p.version, "project": p.cfg})
			}
			printProject(a.stdout, p.cfg, p.version)
			return nil
		},
	}

	var allowOverrides []string
	set := &cobra.Command{
		Use:   "set <project>",
		Short: "Change a project's settings: --domain, --allow-overrides",
		Long: "--allow-overrides is what developers may change for themselves in devopus.yaml:\n" +
			"versions (of services), env (any variable), env:NAME (one), ports, * (all), or none.\n" +
			"Admins and teamleads may override anything.",
		Example: "  devopus project set shop --domain shop.test\n  devopus project set shop --allow-overrides versions,env:DEBUG",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			policy := ""
			if cmd.Flags().Changed("allow-overrides") {
				allowed := allowOverrides
				if len(allowed) == 1 && allowed[0] == "none" {
					allowed = []string{}
				}
				policy = toYAML(map[string]any{"overrides": map[string]any{"allowed": allowed}})
			}
			if domain == "" && policy == "" {
				return fail(ExitInvalid, "nothing to change: --domain <host> or --allow-overrides <kinds>")
			}
			p, err := a.remoteProject(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			conf, err := p.edit(cmd.Context(), func(base int32) (*workspacev1.ProjectConfig, error) {
				r, err := p.c.UpdateProjectSettings(cmd.Context(), req(p.s, &workspacev1.UpdateProjectSettingsRequest{WorkspaceId: p.ws.ID, Project: p.name, BaseVersion: base, Domain: domain, PolicyYaml: policy}))
				if err != nil {
					return nil, err
				}
				return r.Msg.Config, nil
			})
			if err != nil {
				return err
			}
			return a.changed(conf, "%s: %s", p.name, conf.Message)
		},
	}
	set.Flags().StringVar(&domain, "domain", "", "domain of its environments")
	set.Flags().StringSliceVar(&allowOverrides, "allow-overrides", nil, "what developers may override in devopus.yaml: versions, env, env:NAME, ports, *, none")

	var yes bool
	rm := &cobra.Command{
		Use:   "rm <project>",
		Short: "Delete a project with its settings and roles (its teamleads, the admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, cur, c, err := a.inWorkspace(cmd.Context())
			if err != nil {
				return err
			}
			ok, err := a.confirm(yes, fmt.Sprintf("Delete project %s with all versions of its settings?", args[0]))
			if err != nil || !ok {
				return err
			}
			if _, err := c.DeleteProject(cmd.Context(), req(s, &workspacev1.DeleteProjectRequest{WorkspaceId: cur.ID, Project: args[0], ConfirmName: args[0]})); err != nil {
				return rpcErr(err)
			}
			return a.done(map[string]any{"deleted": args[0]}, "Project %s deleted.\n", args[0])
		},
	}
	rm.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")

	cmd.AddCommand(create, list, show, set, a.projectConfigCmd(), rm)
	return cmd
}

func (a *app) projectConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "A project's settings: show, history, rollback",
	}
	var output string
	var version int32
	var effective bool
	show := &cobra.Command{
		Use:   "show <project>",
		Short: "The settings of a project; -o yaml for all of them, --version for an older one",
		Long: "Shows a project's settings on the workspace server. --effective shows what runs on\n" +
			"this machine instead: the settings with the personal overrides of devopus.yaml.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if effective {
				return a.showEffective(cmd.Context(), args[0], output)
			}
			s, cur, c, err := a.inWorkspace(cmd.Context())
			if err != nil {
				return err
			}
			res, err := c.GetProjectConfig(cmd.Context(), req(s, &workspacev1.GetProjectConfigRequest{WorkspaceId: cur.ID, Project: args[0], Version: version}))
			if err != nil {
				return rpcErr(err)
			}
			conf := res.Msg.Config
			switch {
			case output == "yaml":
				fmt.Fprintf(a.stdout, "# %s, version %d: %s (%s, %s)\n", args[0], conf.Version, conf.Message, conf.Author, conf.CreatedAt.AsTime().Local().Format("2 Jan 2006 15:04"))
				_, err := io.WriteString(a.stdout, conf.Yaml)
				return err
			case a.json || output == "json":
				cfg, err := spec.ParseYAML([]byte(conf.Yaml))
				if err != nil {
					return err
				}
				return a.writeJSON(map[string]any{"version": conf.Version, "current": res.Msg.CurrentVersion, "author": conf.Author, "message": conf.Message, "project": cfg})
			case output != "":
				return fail(ExitInvalid, "-o: yaml or json")
			}
			cfg, err := spec.ParseYAML([]byte(conf.Yaml))
			if err != nil {
				return err
			}
			printProject(a.stdout, cfg, conf.Version)
			if conf.Version != res.Msg.CurrentVersion {
				fmt.Fprintf(a.stdout, "\nThis is version %d; the current one is %d.\n", conf.Version, res.Msg.CurrentVersion)
			}
			return nil
		},
	}
	show.Flags().StringVarP(&output, "output", "o", "", "yaml or json")
	show.Flags().Int32Var(&version, "version", 0, "an older version")
	show.Flags().BoolVar(&effective, "effective", false, "what runs on this machine: with devopus.yaml")

	var limit int32
	history := &cobra.Command{
		Use:   "history <project>",
		Short: "Versions of a project's settings: who changed what",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, cur, c, err := a.inWorkspace(cmd.Context())
			if err != nil {
				return err
			}
			res, err := c.ListProjectVersions(cmd.Context(), req(s, &workspacev1.ListProjectVersionsRequest{WorkspaceId: cur.ID, Project: args[0], Limit: limit}))
			if err != nil {
				return rpcErr(err)
			}
			if a.json {
				return a.writeProto(res.Msg)
			}
			w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "VERSION\tWHEN\tWHO\tWHAT")
			for _, v := range res.Msg.Versions {
				fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", v.Version, v.CreatedAt.AsTime().Local().Format("2 Jan 15:04"), v.Author, v.Message)
			}
			return w.Flush()
		},
	}
	history.Flags().Int32VarP(&limit, "limit", "n", 20, "how many versions")

	rollback := &cobra.Command{
		Use:   "rollback <project> <version>",
		Short: "Make an older version current again; it becomes the newest version",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := strconv.Atoi(args[1])
			if err != nil || v <= 0 {
				return fail(ExitInvalid, "version %q: a number from 'devopus project config history'", args[1])
			}
			s, cur, c, err := a.inWorkspace(cmd.Context())
			if err != nil {
				return err
			}
			res, err := c.RollbackProject(cmd.Context(), req(s, &workspacev1.RollbackProjectRequest{WorkspaceId: cur.ID, Project: args[0], Version: int32(v)}))
			if err != nil {
				return rpcErr(err)
			}
			return a.changed(res.Msg.Config, "%s is back to version %d", args[0], v)
		},
	}
	cmd.AddCommand(show, history, rollback)
	return cmd
}

// showEffective prints a project as the agent on this machine runs it.
func (a *app) showEffective(ctx context.Context, project, output string) error {
	c, err := a.connect(ctx)
	if err != nil {
		return err
	}
	res, err := c.DescribeProject(ctx, connect.NewRequest(&agentv2.DescribeProjectRequest{Project: project}))
	if err != nil {
		return rpcError(err)
	}
	m := res.Msg
	cfg, err := spec.ParseYAML(m.Spec)
	if err != nil {
		return err
	}
	switch {
	case a.json || output == "json":
		return a.writeJSON(map[string]any{"project": cfg, "overrideFile": m.OverrideFile, "applied": m.OverridesApplied, "rejected": m.OverridesRejected})
	case output == "yaml":
		fmt.Fprintf(a.stdout, "# %s as it runs on this machine (with %s)\n", project, dash(m.OverrideFile))
		_, err := a.stdout.Write(m.Spec)
		return err
	case output != "":
		return fail(ExitInvalid, "-o: yaml or json")
	}
	var version int32
	if st, err := c.Status(ctx, connect.NewRequest(&agentv2.StatusRequest{})); err == nil {
		for _, p := range st.Msg.Projects {
			if p.Name == project {
				version = p.ConfigVersion
			}
		}
	}
	printProject(a.stdout, cfg, version)
	if m.OverrideFile != "" {
		fmt.Fprintf(a.stdout, "\nPersonal overrides: %s\n", m.OverrideFile)
	}
	for _, o := range m.OverridesApplied {
		fmt.Fprintf(a.stdout, "  applied   %s\n", o)
	}
	for _, o := range m.OverridesRejected {
		fmt.Fprintf(a.stdout, "  refused   %s\n", o)
	}
	return nil
}

// printProject shows a project as tables.
func printProject(out io.Writer, p *spec.Project, version int32) {
	fmt.Fprintf(out, "Project %s · version %d · https://%s\n\n", p.Name, version, p.Domain)
	if len(p.Apps) == 0 {
		fmt.Fprintln(out, "No applications yet: devopus app add "+p.Name+" <name> --repo <url>")
	} else {
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "APPLICATION\tCODE\tBRANCH\tROUTES\tSERVICES")
		for _, n := range p.AppNames() {
			a := p.Apps[n]
			code := a.Repo
			if code == "" && a.Path != "" {
				code = "./" + a.Path
			}
			if code == "" {
				code = a.Image
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", n, code, a.BaseBranch(), dash(strings.Join(a.Routes, " ")), dash(strings.Join(p.Attached(n), ", ")))
		}
		_ = w.Flush()
	}
	if len(p.Services) > 0 {
		fmt.Fprintln(out)
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "SERVICE\tTYPE\tVERSION\tKIND\tAPPLICATIONS")
		for _, n := range p.ServiceNames() {
			s := p.Services[n]
			v := s.Version
			if v == "" && s.Image != "" {
				v = s.Image
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", n, s.Type, dash(v), s.Kind(), dash(strings.Join(s.Apps, ", ")))
		}
		_ = w.Flush()
	}
}

// importCompose turns compose files into a project the way 'devopus init' reads them:
// compose → workspace.yaml v1 → the model. Values that look like secrets come back
// apart: they are encrypted here before they go to the server.
func (a *app) importCompose(ctx context.Context, name string, files []string, noPin bool) (*spec.Project, map[string]string, error) {
	opt := compose.Options{Project: name}
	if !noPin {
		opt.Pinner = compose.RegistryPinner{}
	}
	res, err := compose.Import(ctx, files, opt)
	if err != nil {
		return nil, nil, fail(ExitInvalid, "cannot load %s: %v", strings.Join(files, ", "), err)
	}
	data, err := marshal(res.Workspace)
	if err != nil {
		return nil, nil, err
	}
	parsed, err := workspace.Parse(data)
	if err != nil {
		return nil, nil, err
	}
	ds := parsed.Diagnostics
	if !a.json {
		res.Report.Write(a.stderr, ds.HasErrors())
		a.printDiagnostics(strings.Join(files, ", "), ds)
	}
	if ds.HasErrors() || parsed.Workspace == nil {
		return nil, nil, fail(ExitInvalid, "the import has errors; fix the compose file or add the project by hand (devopus app add, devopus service add)")
	}
	p, err := importer.FromV1(parsed.Workspace)
	if err != nil {
		return nil, nil, fail(ExitInvalid, "%v", err)
	}
	return p, res.Secrets, nil
}

// --- applications

type appFlags struct {
	repo, path, branch, command string
	routes, env, uses           []string
	port                        int
}

func (f *appFlags) bind(c *cobra.Command) {
	c.Flags().StringVar(&f.repo, "repo", "", "git repository (git@github.com:acme/api.git)")
	c.Flags().StringVar(&f.path, "path", "", "folder in the project's own repository, instead of --repo")
	c.Flags().StringVar(&f.branch, "branch", "", "branch of the base environment (default: main)")
	c.Flags().StringArrayVar(&f.routes, "route", nil, "path it serves (/api); repeat")
	c.Flags().IntVar(&f.port, "port", 0, "port it listens on (default: 8080)")
	c.Flags().StringVar(&f.command, "command", "", "command that starts it")
	c.Flags().StringArrayVar(&f.env, "env", nil, "KEY=value; repeat")
	c.Flags().StringSliceVar(&f.uses, "uses", nil, "other applications it calls")
}

// apply sets the flags given on the command line.
func (f *appFlags) apply(c *cobra.Command, app *spec.App) error {
	ch := c.Flags().Changed
	if ch("repo") {
		app.Repo, app.Path = f.repo, ""
	}
	if ch("path") {
		app.Path, app.Repo = f.path, ""
	}
	if ch("branch") {
		app.Branch = f.branch
	}
	if ch("route") {
		app.Routes = f.routes
	}
	if ch("port") {
		app.Port = f.port
	}
	if ch("command") {
		app.Command = f.command
	}
	if ch("uses") {
		app.Uses = f.uses
	}
	if ch("env") {
		env, err := parseEnv(f.env, app.Env)
		if err != nil {
			return err
		}
		app.Env = env
	}
	return nil
}

// parseEnv sets KEY=value pairs over base; KEY= removes a key.
func parseEnv(pairs []string, base map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fail(ExitInvalid, "--env %q: KEY=value", p)
		}
		if v == "" {
			delete(out, k)
		} else {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func (a *app) appCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "app",
		Short: "Applications of a project: its repositories (admin, teamlead, apps.manage)",
		Long: "An application is a part of the product, usually a repository of its own: frontend,\n" +
			"backend, admin. What it runs on (php 8.3, node 22) is a service attached to it.",
	}
	put := func(create bool) *cobra.Command {
		var f appFlags
		use, short := "add <project> <name>", "Add an application"
		if !create {
			use, short = "set <project> <name>", "Change an application: the flags given replace their values"
		}
		c := &cobra.Command{
			Use:   use,
			Short: short,
			Args:  cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				if create && f.repo == "" && f.path == "" {
					return fail(ExitInvalid, "where is the code? --repo <url> or --path <folder>")
				}
				p, err := a.remoteProject(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				conf, err := p.edit(cmd.Context(), func(base int32) (*workspacev1.ProjectConfig, error) {
					app := &spec.App{}
					if !create {
						if p.cfg.Apps[args[1]] == nil {
							return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s has no application %q", args[0], args[1]))
						}
						app = p.cfg.Apps[args[1]]
					}
					if err := f.apply(cmd, app); err != nil {
						return nil, err
					}
					r, err := p.c.PutApp(cmd.Context(), req(p.s, &workspacev1.PutAppRequest{WorkspaceId: p.ws.ID, Project: p.name, BaseVersion: base, Name: args[1], AppYaml: toYAML(app), Create: create}))
					if err != nil {
						return nil, err
					}
					return r.Msg.Config, nil
				})
				if err != nil {
					return err
				}
				return a.changed(conf, "%s: %s", args[0], conf.Message)
			},
		}
		f.bind(c)
		return c
	}
	rm := &cobra.Command{
		Use:   "rm <project> <name>",
		Short: "Remove an application; services attached to it are detached",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := a.remoteProject(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			conf, err := p.edit(cmd.Context(), func(base int32) (*workspacev1.ProjectConfig, error) {
				r, err := p.c.RemoveApp(cmd.Context(), req(p.s, &workspacev1.RemoveAppRequest{WorkspaceId: p.ws.ID, Project: p.name, BaseVersion: base, Name: args[1]}))
				if err != nil {
					return nil, err
				}
				return r.Msg.Config, nil
			})
			if err != nil {
				return err
			}
			return a.changed(conf, "%s: %s", args[0], conf.Message)
		},
	}
	list := &cobra.Command{
		Use:   "list <project>",
		Short: "Applications of a project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := a.remoteProject(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if a.json {
				return a.writeJSON(p.cfg.Apps)
			}
			printProject(a.stdout, &spec.Project{Name: p.cfg.Name, Domain: p.cfg.Domain, Apps: p.cfg.Apps, Services: nil}, p.version)
			return nil
		},
	}
	cmd.AddCommand(put(true), put(false), rm, list)
	return cmd
}

// --- services

type serviceFlags struct {
	name, image, command, schedule string
	apps, uses, env                []string
	port, replicas                 int
}

func (f *serviceFlags) bind(c *cobra.Command, create bool) {
	if create {
		c.Flags().StringVar(&f.name, "name", "", "service name (default: the type)")
	}
	c.Flags().StringSliceVar(&f.apps, "apps", nil, "applications it is attached to: a,b")
	c.Flags().StringVar(&f.image, "image", "", "image instead of the catalog's (needed for custom)")
	c.Flags().IntVar(&f.port, "port", 0, "port other containers connect to")
	c.Flags().StringArrayVar(&f.env, "env", nil, "KEY=value; repeat; KEY= removes")
	c.Flags().StringVar(&f.command, "command", "", "command (a queue's or cronjob's is required)")
	c.Flags().StringVar(&f.schedule, "schedule", "", "cron schedule of a cronjob (*/5 * * * *)")
	c.Flags().IntVar(&f.replicas, "replicas", 0, "copies of a queue worker")
	c.Flags().StringSliceVar(&f.uses, "uses", nil, "other services it needs")
}

func (f *serviceFlags) apply(c *cobra.Command, s *spec.Service) error {
	ch := c.Flags().Changed
	if ch("apps") {
		s.Apps = f.apps
	}
	if ch("image") {
		s.Image = f.image
	}
	if ch("port") {
		s.Port = f.port
	}
	if ch("command") {
		s.Command = f.command
	}
	if ch("schedule") {
		s.Schedule = f.schedule
	}
	if ch("replicas") {
		s.Replicas = f.replicas
	}
	if ch("uses") {
		s.Uses = f.uses
	}
	if ch("env") {
		env, err := parseEnv(f.env, s.Env)
		if err != nil {
			return err
		}
		s.Env = env
	}
	return nil
}

func (a *app) putService(cmd *cobra.Command, p *inProject, name string, create bool, edit func(*spec.Service) error) error {
	conf, err := p.edit(cmd.Context(), func(base int32) (*workspacev1.ProjectConfig, error) {
		svc := &spec.Service{}
		if !create {
			if p.cfg.Services[name] == nil {
				return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s has no service %q", p.name, name))
			}
			svc = p.cfg.Services[name]
		}
		if err := edit(svc); err != nil {
			return nil, err
		}
		r, err := p.c.PutService(cmd.Context(), req(p.s, &workspacev1.PutServiceRequest{WorkspaceId: p.ws.ID, Project: p.name, BaseVersion: base, Name: name, ServiceYaml: toYAML(svc), Create: create}))
		if err != nil {
			return nil, err
		}
		return r.Msg.Config, nil
	})
	if err != nil {
		return err
	}
	return a.changed(conf, "%s: %s", p.name, conf.Message)
}

func (a *app) serviceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Services of a project, attached to its applications (admin, teamlead, services.manage)",
		Long: "A service lives in the project and is attached to one or more of its applications:\n" +
			"a runtime they run on (php, nodejs, python…), a container (mysql, postgres, redis,\n" +
			"typesense, mailpit…) or a process with their code (queue, cronjob).\n\nTypes: " +
			strings.Join(catalog.Names(), ", "),
	}
	var addF serviceFlags
	add := &cobra.Command{
		Use:     "add <project> <type> [version]",
		Short:   "Add a service of a catalog type",
		Example: "  devopus service add shop php 8.3 --apps api,admin\n  devopus service add shop mysql 8.4 --apps api\n  devopus service add shop queue --apps api --command 'php artisan queue:work'",
		Args:    cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, ok := catalog.Lookup(args[1])
			if !ok {
				return fail(ExitInvalid, "unknown type %q; types: %s", args[1], strings.Join(catalog.Names(), ", "))
			}
			name := addF.name
			if name == "" {
				name = t.Name
			}
			p, err := a.remoteProject(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.putService(cmd, p, name, true, func(s *spec.Service) error {
				s.Type = t.Name
				if len(args) == 3 {
					s.Version = args[2]
				}
				return addF.apply(cmd, s)
			})
		},
	}
	addF.bind(add, true)

	var setF serviceFlags
	set := &cobra.Command{
		Use:     "set <project> <service> [version]",
		Short:   "Change a service: its version, or the flags given",
		Example: "  devopus service set shop php 8.4\n  devopus service set shop db --env MYSQL_DATABASE=shop",
		Args:    cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := a.remoteProject(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.putService(cmd, p, args[1], false, func(s *spec.Service) error {
				if len(args) == 3 {
					s.Version = args[2]
				}
				return setF.apply(cmd, s)
			})
		},
	}
	setF.bind(set, false)

	link := func(attach bool) *cobra.Command {
		use, short := "link <project> <service> <app>…", "Attach a service to applications"
		if !attach {
			use, short = "unlink <project> <service> <app>…", "Detach a service from applications"
		}
		return &cobra.Command{
			Use:   use,
			Short: short,
			Args:  cobra.MinimumNArgs(3),
			RunE: func(cmd *cobra.Command, args []string) error {
				p, err := a.remoteProject(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				return a.putService(cmd, p, args[1], false, func(s *spec.Service) error {
					for _, app := range args[2:] {
						has := false
						for _, x := range s.Apps {
							has = has || x == app
						}
						switch {
						case attach && !has:
							s.Apps = append(s.Apps, app)
						case !attach && has:
							var keep []string
							for _, x := range s.Apps {
								if x != app {
									keep = append(keep, x)
								}
							}
							s.Apps = keep
						}
					}
					return nil
				})
			},
		}
	}

	rm := &cobra.Command{
		Use:   "rm <project> <service>",
		Short: "Remove a service",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := a.remoteProject(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			conf, err := p.edit(cmd.Context(), func(base int32) (*workspacev1.ProjectConfig, error) {
				r, err := p.c.RemoveService(cmd.Context(), req(p.s, &workspacev1.RemoveServiceRequest{WorkspaceId: p.ws.ID, Project: p.name, BaseVersion: base, Name: args[1]}))
				if err != nil {
					return nil, err
				}
				return r.Msg.Config, nil
			})
			if err != nil {
				return err
			}
			return a.changed(conf, "%s: %s", args[0], conf.Message)
		},
	}
	list := &cobra.Command{
		Use:   "list <project>",
		Short: "Services of a project and the applications they are attached to",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := a.remoteProject(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if a.json {
				return a.writeJSON(p.cfg.Services)
			}
			if len(p.cfg.Services) == 0 {
				fmt.Fprintf(a.stdout, "No services yet: devopus service add %s <type> <version> --apps <app>\n", args[0])
				return nil
			}
			w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "SERVICE\tTYPE\tVERSION\tKIND\tAPPLICATIONS")
			for _, n := range p.cfg.ServiceNames() {
				s := p.cfg.Services[n]
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", n, s.Type, dash(s.Version), s.Kind(), dash(strings.Join(s.Apps, ", ")))
			}
			return w.Flush()
		},
	}
	cmd.AddCommand(add, set, link(true), link(false), rm, list)
	return cmd
}
