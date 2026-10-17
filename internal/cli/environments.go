package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	workspacev1 "github.com/amalychev/devopus/gen/devopus/workspace/v1"
	"github.com/amalychev/devopus/internal/credentials"
	"github.com/amalychev/devopus/internal/signing"
)

// Environments of projects on servers of the admin (R14, ADR-0020).
func (a *app) envCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Staging and production of projects on servers of the admin, at public domains",
		Long: "A server runs a project as staging (production: R14b) at a public domain; its agent\n" +
			"gets the certificate from Let's Encrypt, so the domain's DNS must point at the server\n" +
			"and its port 80 must be open. The environment is signed by your device: the server\n" +
			"takes no domain on the workspace server's word.",
	}
	var project, server, domain string
	create := &cobra.Command{
		Use:   "create staging --project <project> --server <server> --domain <host>",
		Short: "Run a project on a server at a public domain (servers.manage)",
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
			fmt.Fprintln(w, "PROJECT\tKIND\tSERVER\tURL\tCREATED BY")
			for _, e := range res.Msg.Environments {
				fmt.Fprintf(w, "%s\t%s\t%s\thttps://%s\t%s\n", e.Project, e.Kind, e.Server, e.Domain, e.CreatedBy)
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
	cmd.AddCommand(create, list, rm)
	return cmd
}
