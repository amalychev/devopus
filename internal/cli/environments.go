package cli

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	workspacev1 "github.com/amalychev/devopus/gen/devopus/workspace/v1"
	"github.com/amalychev/devopus/gen/devopus/workspace/v1/workspacev1connect"
	"github.com/amalychev/devopus/internal/credentials"
	"github.com/amalychev/devopus/internal/signing"
	"github.com/amalychev/devopus/internal/spec"
)

// Environments of projects on servers of the admin (R14, ADR-0020).
func (a *app) envCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Staging and production of projects on servers of the admin, at public domains",
		Long: "A server runs a project as staging or production at a public domain; its agent gets\n" +
			"the certificate from Let's Encrypt, so the domain's DNS must point at the server and\n" +
			"its port 80 must be open. Staging follows the project's settings; production runs\n" +
			"the version someone with production.deploy deployed ('devopus env deploy'). Every\n" +
			"environment and deploy is signed by your device: servers take nothing on the\n" +
			"workspace server's word.",
	}
	var project, server, domain string
	create := &cobra.Command{
		Use:   "create staging|production --project <project> --server <server> --domain <host>",
		Short: "Run a project on a server at a public domain (servers.manage; production.deploy for production)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if project == "" || server == "" || domain == "" {
				return fail(ExitInvalid, "--project, --server and --domain are needed")
			}
			s, cur, c, err := a.inWorkspace(ctx)
			if err != nil {
				return err
			}
			servers, err := c.ListServers(ctx, req(s, &workspacev1.ListServersRequest{WorkspaceId: cur.ID}))
			if err != nil {
				return rpcErr(err)
			}
			serverID := ""
			for _, sv := range servers.Msg.Servers {
				if sv.Name == server {
					serverID = sv.Id
				}
			}
			if serverID == "" {
				return fail(ExitInvalid, "no server %q: 'devopus servers list'", server)
			}
			key, certified, err := a.signingKey(ctx, s, cur, c)
			if err != nil {
				return fail(ExitError, "%v", err)
			}
			host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
			sig := key.Sign(signing.EnvironmentMessage(cur.ID, serverID, project, args[0], host))
			res, err := c.CreateEnvironment(ctx, req(s, &workspacev1.CreateEnvironmentRequest{WorkspaceId: cur.ID, Project: project, Server: server,
				Kind: args[0], Domain: host, DeviceId: credentials.DeviceID(), Signature: sig}))
			if err != nil {
				return rpcErr(err)
			}
			e := res.Msg.Environment
			if !certified {
				fmt.Fprintln(a.stderr, "Signed, but the workspace's admin has not certified this device yet: the server takes the domain once they do ('devopus trust approve').")
			}
			return a.done(map[string]any{"project": e.Project, "server": e.Server, "kind": e.Kind, "domain": e.Domain},
				"✓ %s runs %s on %s at https://%s.\n"+
					"Point the DNS of %s at the server (an A or AAAA record) and keep its port 80 open: Let's Encrypt checks it.\n"+
					"The server runs versions signed by people the admin certified.\n", e.Server, e.Project, e.Kind, e.Domain, e.Domain)
		},
	}
	create.Flags().StringVar(&project, "project", "", "the project")
	create.Flags().StringVar(&server, "server", "", "the server ('devopus servers list')")
	create.Flags().StringVar(&domain, "domain", "", "its public domain: staging.shop.example.com")
	list := &cobra.Command{
		Use:   "list",
		Short: "Environments of projects on servers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			s, cur, c, err := a.inWorkspace(ctx)
			if err != nil {
				return err
			}
			res, err := c.ListEnvironments(ctx, req(s, &workspacev1.ListEnvironmentsRequest{WorkspaceId: cur.ID}))
			if err != nil {
				return rpcErr(err)
			}
			if a.json {
				return a.writeJSON(res.Msg.Environments)
			}
			if len(res.Msg.Environments) == 0 {
				fmt.Fprintln(a.stdout, "No environments on servers. 'devopus env create staging --project … --server … --domain …' makes one.")
				return nil
			}
			w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "PROJECT\tKIND\tSERVER\tURL\tRUNS\tCREATED BY")
			for _, e := range res.Msg.Environments {
				runs := "newest signed version"
				if e.Kind == "production" {
					runs = "nothing deployed yet"
					if e.DeployedVersion != 0 {
						runs = fmt.Sprintf("v%d, deployed by %s", e.DeployedVersion, e.DeployedBy)
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\thttps://%s\t%s\t%s\n", e.Project, e.Kind, e.Server, e.Domain, runs, e.CreatedBy)
			}
			return w.Flush()
		},
	}
	var rmProject, rmServer string
	rm := &cobra.Command{
		Use:   "rm --project <project> --server <server>",
		Short: "Stop running a project on a server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			s, cur, c, err := a.inWorkspace(ctx)
			if err != nil {
				return err
			}
			if _, err := c.RemoveEnvironment(ctx, req(s, &workspacev1.RemoveEnvironmentRequest{WorkspaceId: cur.ID, Project: rmProject, Server: rmServer})); err != nil {
				return rpcErr(err)
			}
			return a.done(map[string]any{"project": rmProject, "server": rmServer}, "✓ %s no longer runs %s; its data stays on the server.\n", rmServer, rmProject)
		},
	}
	rm.Flags().StringVar(&rmProject, "project", "", "the project")
	rm.Flags().StringVar(&rmServer, "server", "", "the server")
	cmd.AddCommand(create, list, a.envDeployCmd(), rm)
	return cmd
}

// envDeployCmd makes a production environment run a version (R14b).
func (a *app) envDeployCmd() *cobra.Command {
	var project, server string
	var version int32
	var yes bool
	cmd := &cobra.Command{
		Use:   "deploy production --project <project> --server <server> [--version N]",
		Short: "Make production run a version of the settings; an older one rolls back (production.deploy)",
		Long: "Production does not follow the settings: it runs the version deployed here, the newest\n" +
			"by default. Shows what changes against what runs, asks, and signs the deploy with your\n" +
			"device. A rollback is a deploy of an older version ('devopus project config history').",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "production" {
				return fail(ExitInvalid, "only production is deployed: staging follows the settings")
			}
			if project == "" || server == "" {
				return fail(ExitInvalid, "--project and --server are needed")
			}
			ctx := cmd.Context()
			s, cur, c, err := a.inWorkspace(ctx)
			if err != nil {
				return err
			}
			envs, err := c.ListEnvironments(ctx, req(s, &workspacev1.ListEnvironmentsRequest{WorkspaceId: cur.ID}))
			if err != nil {
				return rpcErr(err)
			}
			var env *workspacev1.Environment
			for _, e := range envs.Msg.Environments {
				if e.Project == project && e.Server == server && e.Kind == "production" {
					env = e
				}
			}
			if env == nil {
				return fail(ExitInvalid, "%s has no production environment on %s: 'devopus env list'", project, server)
			}
			servers, err := c.ListServers(ctx, req(s, &workspacev1.ListServersRequest{WorkspaceId: cur.ID}))
			if err != nil {
				return rpcErr(err)
			}
			serverID := ""
			for _, sv := range servers.Msg.Servers {
				if sv.Name == server {
					serverID = sv.Id
				}
			}
			target, err := c.GetProjectConfig(ctx, req(s, &workspacev1.GetProjectConfigRequest{WorkspaceId: cur.ID, Project: project, Version: version}))
			if err != nil {
				return rpcErr(err)
			}
			conf := target.Msg.Config
			if len(conf.Signature) == 0 {
				return fail(ExitInvalid, "version %d of %s is not signed: servers run signed versions only. Sign it by changing the settings with devopus, or deploy a signed one", conf.Version, project)
			}
			if conf.Version == env.DeployedVersion {
				return a.done(map[string]any{"project": project, "version": conf.Version}, "%s production runs v%d already.\n", project, conf.Version)
			}
			question := fmt.Sprintf("Deploy %s v%d to production on %s (https://%s)?", project, conf.Version, server, env.Domain)
			if env.DeployedVersion != 0 {
				question += a.deployChanges(ctx, s, cur, c, project, env.DeployedVersion, conf)
			}
			ok, err := a.confirm(yes, question)
			if err != nil {
				return err
			}
			if !ok {
				return fail(ExitError, "nothing deployed")
			}
			key, certified, err := a.signingKey(ctx, s, cur, c)
			if err != nil {
				return fail(ExitError, "%v", err)
			}
			sig := key.Sign(signing.DeployMessage(cur.ID, serverID, project, conf.Version, []byte(conf.Yaml)))
			res, err := c.DeployProduction(ctx, req(s, &workspacev1.DeployProductionRequest{WorkspaceId: cur.ID, Project: project, Server: server,
				Version: conf.Version, DeviceId: credentials.DeviceID(), Signature: sig}))
			if err != nil {
				return rpcErr(err)
			}
			if !certified {
				fmt.Fprintln(a.stderr, "Signed, but the workspace's admin has not certified this device yet: the server runs the deploy once they do ('devopus trust approve').")
			}
			e := res.Msg.Environment
			return a.done(map[string]any{"project": project, "server": server, "version": e.DeployedVersion},
				"✓ %s production on %s runs v%d. 'devopus drift' shows when the server has it.\n", project, server, e.DeployedVersion)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "the project")
	cmd.Flags().StringVar(&server, "server", "", "the server")
	cmd.Flags().Int32Var(&version, "version", 0, "the version to run (default: the newest)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	return cmd
}

// deployChanges is what a deploy changes against what production runs, for the
// question: new code first.
func (a *app) deployChanges(ctx context.Context, s *session, cur *current, c workspacev1connect.WorkspaceServiceClient, project string, running int32, next *workspacev1.ProjectConfig) string {
	old, err := c.GetProjectConfig(ctx, req(s, &workspacev1.GetProjectConfigRequest{WorkspaceId: cur.ID, Project: project, Version: running}))
	if err != nil {
		return ""
	}
	a1, err1 := spec.ParseYAML([]byte(old.Msg.Config.Yaml))
	a2, err2 := spec.ParseYAML([]byte(next.Yaml))
	if err1 != nil || err2 != nil {
		return ""
	}
	changes := spec.Diff(a1, a2)
	if len(changes) == 0 {
		return fmt.Sprintf("\n  It runs v%d now; the settings are the same.", running)
	}
	out := fmt.Sprintf("\n  It runs v%d now. Changes:", running)
	for _, ch := range changes {
		mark := " "
		if ch.Risky {
			mark = "!"
		}
		out += fmt.Sprintf("\n  %s %s", mark, ch.What)
	}
	return out + "\n  (! — new code on the server)"
}
