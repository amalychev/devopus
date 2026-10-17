package cli

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/amalychev/devopus/examples/demo"
	authv1 "github.com/amalychev/devopus/gen/devopus/auth/v1"
	workspacev1 "github.com/amalychev/devopus/gen/devopus/workspace/v1"
	"github.com/amalychev/devopus/gen/devopus/workspace/v1/workspacev1connect"
	"github.com/amalychev/devopus/internal/authz"
	"github.com/amalychev/devopus/internal/credentials"
	"github.com/amalychev/devopus/internal/importer"
)

// The team commands talk to the workspace server (ADR-0016): workspace, project,
// invite, member. The server checks every permission; the CLI only shows its answer.

// DefaultServerURL is the workspace server in development (devopus server run).
const DefaultServerURL = "http://" + DefaultServerListen

// serverURL is the workspace server for new workspaces: $DEVOPUS_SERVER_URL or the default.
func serverURL() string { return strings.TrimRight(envOr("DEVOPUS_SERVER_URL", DefaultServerURL), "/") }

// current is the workspace commands work in, kept per data directory; the agent
// follows it too.
type current = credentials.Workspace

func loadCurrent() (*current, error) { return credentials.CurrentWorkspace() }

func saveCurrent(c *current) error { return credentials.SetCurrentWorkspace(c) }

// session is a signed-in call context: an access token for Auth and workspace servers.
type session struct {
	acc  credentials.Access
	auth interface {
		RegisterWorkspace(context.Context, *connect.Request[authv1.RegisterWorkspaceRequest]) (*connect.Response[authv1.RegisterWorkspaceResponse], error)
		ForgetWorkspace(context.Context, *connect.Request[authv1.ForgetWorkspaceRequest]) (*connect.Response[authv1.ForgetWorkspaceResponse], error)
		ListWorkspaces(context.Context, *connect.Request[authv1.ListWorkspacesRequest]) (*connect.Response[authv1.ListWorkspacesResponse], error)
	}
}

func (a *app) session(ctx context.Context) (*session, error) {
	acc, c, err := a.access(ctx)
	if err != nil {
		return nil, err
	}
	return &session{acc: acc, auth: c}, nil
}

func (s *session) server(url string) workspacev1connect.WorkspaceServiceClient {
	return workspacev1connect.NewWorkspaceServiceClient(&http.Client{Timeout: 30 * time.Second}, url)
}

func req[T any](s *session, msg *T) *connect.Request[T] { return bearer(msg, s.acc) }

// register keeps a workspace in the user's registry in Auth: where its server is.
func (s *session) register(ctx context.Context, w *workspacev1.Workspace, server string) {
	_, _ = s.auth.RegisterWorkspace(ctx, req(s, &authv1.RegisterWorkspaceRequest{Id: w.Id, Name: w.Name, ServerUrl: server}))
}

// entry is a workspace the user is in, on its server.
type entry struct {
	ws     *workspacev1.Workspace
	server string
}

// workspaces asks every server of the user's registry (and the default one) which
// workspaces the user is in; the registry follows: entries the servers deny are
// forgotten, memberships it lacks are added.
func (a *app) workspaces(ctx context.Context, s *session) ([]entry, error) {
	reg, err := s.auth.ListWorkspaces(ctx, req(s, &authv1.ListWorkspacesRequest{}))
	if err != nil {
		return nil, rpcErr(err)
	}
	servers := map[string]bool{serverURL(): true}
	known := map[string]*authv1.Workspace{}
	for _, w := range reg.Msg.Workspaces {
		servers[w.ServerUrl] = true
		known[w.Id] = w
	}
	var out []entry
	reached := map[string]bool{}
	for _, url := range sortedKeys(servers) {
		res, err := s.server(url).ListWorkspaces(ctx, req(s, &workspacev1.ListWorkspacesRequest{}))
		if err != nil {
			if url != serverURL() || len(servers) == 1 {
				fmt.Fprintf(a.stderr, "Workspace server %s did not answer: %v\n", url, connect.CodeOf(err))
			}
			continue
		}
		reached[url] = true
		for _, w := range res.Msg.Workspaces {
			out = append(out, entry{ws: w, server: url})
			if k := known[w.Id]; k == nil || k.Name != w.Name || k.ServerUrl != url {
				s.register(ctx, w, url)
			}
			delete(known, w.Id)
		}
	}
	for id, w := range known {
		if reached[w.ServerUrl] {
			_, _ = s.auth.ForgetWorkspace(ctx, req(s, &authv1.ForgetWorkspaceRequest{Id: id}))
		}
	}
	if len(reached) == 0 {
		return nil, fail(ExitError, "no workspace server answered; is 'devopus server run' up, and DEVOPUS_SERVER_URL right?")
	}
	// The current workspace is gone or the user left it: choose again.
	if cur, _ := loadCurrent(); cur != nil && reached[cur.ServerURL] && !slices.ContainsFunc(out, func(e entry) bool { return e.ws.Id == cur.ID }) {
		_ = saveCurrent(nil)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ws.Name < out[j].ws.Name })
	return out, nil
}

// inWorkspace is the current workspace with a client of its server. With none chosen
// and exactly one to choose from, it takes that one.
func (a *app) inWorkspace(ctx context.Context) (*session, *current, workspacev1connect.WorkspaceServiceClient, error) {
	s, err := a.session(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	cur, err := loadCurrent()
	if err != nil {
		return nil, nil, nil, fail(ExitError, "%v", err)
	}
	if cur == nil {
		list, err := a.workspaces(ctx, s)
		if err != nil {
			return nil, nil, nil, err
		}
		switch len(list) {
		case 0:
			return nil, nil, nil, fail(ExitInvalid, "you are in no workspace: create one with 'devopus workspace create <name>' or accept an invite")
		case 1:
			cur = &current{ID: list[0].ws.Id, Name: list[0].ws.Name, ServerURL: list[0].server}
			if err := saveCurrent(cur); err != nil {
				return nil, nil, nil, fail(ExitError, "%v", err)
			}
		default:
			return nil, nil, nil, fail(ExitInvalid, "choose a workspace: devopus workspace use <name> ('devopus workspace list' shows them)")
		}
	}
	return s, cur, s.server(cur.ServerURL), nil
}

func (a *app) workspaceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "workspace",
		Aliases: []string{"ws"},
		Short:   "Your workspaces: create, list, switch, rename, hand over, delete",
		Long: "A workspace is your company's place in Devopus: its members with their roles, and its\n" +
			"projects. Whoever creates one is its admin; there is one admin per workspace.",
	}
	var server string
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a workspace; you are its admin, and it becomes the current one",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			s, err := a.session(ctx)
			if err != nil {
				return err
			}
			url := strings.TrimRight(server, "/")
			res, err := s.server(url).CreateWorkspace(ctx, req(s, &workspacev1.CreateWorkspaceRequest{Name: args[0]}))
			if err != nil {
				return rpcErr(err)
			}
			w := res.Msg.Workspace
			s.register(ctx, w, url)
			if err := saveCurrent(&current{ID: w.Id, Name: w.Name, ServerURL: url}); err != nil {
				return fail(ExitError, "%v", err)
			}
			return a.done(map[string]any{"id": w.Id, "name": w.Name, "server": url},
				"✓ Workspace %s created; you are its admin. Next: devopus project create <name>, devopus invite create <email> --role teamlead\n", w.Name)
		},
	}
	create.Flags().StringVar(&server, "server", serverURL(), "the workspace server ($DEVOPUS_SERVER_URL)")

	list := &cobra.Command{
		Use:   "list",
		Short: "List your workspaces and your role in each",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := a.session(cmd.Context())
			if err != nil {
				return err
			}
			list, err := a.workspaces(cmd.Context(), s)
			if err != nil {
				return err
			}
			cur, _ := loadCurrent()
			if a.json {
				var out []map[string]any
				for _, e := range list {
					out = append(out, map[string]any{"id": e.ws.Id, "name": e.ws.Name, "role": e.ws.Role, "admin": e.ws.AdminEmail,
						"server": e.server, "current": cur != nil && cur.ID == e.ws.Id})
				}
				return a.writeJSON(out)
			}
			if len(list) == 0 {
				fmt.Fprintln(a.stdout, "No workspaces. Create one: devopus workspace create <name>")
				return nil
			}
			w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "\tNAME\tROLE\tADMIN\tSERVER")
			for _, e := range list {
				mark := ""
				if cur != nil && cur.ID == e.ws.Id {
					mark = "*"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", mark, e.ws.Name, e.ws.Role, e.ws.AdminEmail, e.server)
			}
			return w.Flush()
		},
	}

	use := &cobra.Command{
		Use:   "use <name>",
		Short: "Make a workspace the current one: the commands below work in it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.session(cmd.Context())
			if err != nil {
				return err
			}
			list, err := a.workspaces(cmd.Context(), s)
			if err != nil {
				return err
			}
			var found []entry
			for _, e := range list {
				if e.ws.Id == args[0] || strings.EqualFold(e.ws.Name, args[0]) {
					found = append(found, e)
				}
			}
			switch len(found) {
			case 0:
				return fail(ExitInvalid, "you are in no workspace %q; 'devopus workspace list' shows yours", args[0])
			case 1:
			default:
				return fail(ExitInvalid, "several workspaces are named %q: use the id from 'devopus workspace list --json'", args[0])
			}
			e := found[0]
			if err := saveCurrent(&current{ID: e.ws.Id, Name: e.ws.Name, ServerURL: e.server}); err != nil {
				return fail(ExitError, "%v", err)
			}
			return a.done(map[string]any{"id": e.ws.Id, "name": e.ws.Name}, "Now in workspace %s (%s).\n", e.ws.Name, e.ws.Role)
		},
	}

	show := &cobra.Command{
		Use:   "show",
		Short: "The current workspace and what you may do in it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, cur, c, err := a.inWorkspace(cmd.Context())
			if err != nil {
				return err
			}
			res, err := c.GetWorkspace(cmd.Context(), req(s, &workspacev1.GetWorkspaceRequest{WorkspaceId: cur.ID}))
			if err != nil {
				return rpcErr(err)
			}
			if a.json {
				return a.writeProto(res.Msg)
			}
			m := res.Msg
			w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "Workspace\t%s (%s)\n", m.Workspace.Name, m.Workspace.Id)
			fmt.Fprintf(w, "Admin\t%s\n", m.Workspace.AdminEmail)
			fmt.Fprintf(w, "Server\t%s\n", cur.ServerURL)
			fmt.Fprintf(w, "Your role\t%s\n", m.Workspace.Role)
			fmt.Fprintf(w, "You may\t%s\n", strings.Join(m.Permissions, ", "))
			for _, p := range m.Projects {
				fmt.Fprintf(w, "In %s\t%s\n", p.Project, strings.Join(p.Permissions, ", "))
			}
			return w.Flush()
		},
	}

	rename := &cobra.Command{
		Use:   "rename <new-name>",
		Short: "Rename the current workspace (admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, cur, c, err := a.inWorkspace(cmd.Context())
			if err != nil {
				return err
			}
			res, err := c.RenameWorkspace(cmd.Context(), req(s, &workspacev1.RenameWorkspaceRequest{WorkspaceId: cur.ID, Name: args[0]}))
			if err != nil {
				return rpcErr(err)
			}
			s.register(cmd.Context(), res.Msg.Workspace, cur.ServerURL)
			cur.Name = res.Msg.Workspace.Name
			_ = saveCurrent(cur)
			return a.done(map[string]any{"id": cur.ID, "name": cur.Name}, "Renamed to %s.\n", cur.Name)
		},
	}

	transfer := &cobra.Command{
		Use:   "transfer <email>",
		Short: "Hand the current workspace over to another member; you stay as a teamlead (admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, cur, c, err := a.inWorkspace(cmd.Context())
			if err != nil {
				return err
			}
			if _, err := c.TransferWorkspace(cmd.Context(), req(s, &workspacev1.TransferWorkspaceRequest{WorkspaceId: cur.ID, Email: args[0]})); err != nil {
				return rpcErr(err)
			}
			return a.done(map[string]any{"admin": args[0]}, "%s is the admin of %s now; you are a teamlead.\n", args[0], cur.Name)
		},
	}

	var yes bool
	del := &cobra.Command{
		Use:   "delete",
		Short: "Delete the current workspace with its projects, members and invites (admin)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, cur, c, err := a.inWorkspace(cmd.Context())
			if err != nil {
				return err
			}
			ok, err := a.confirm(yes, fmt.Sprintf("Delete workspace %s with its projects and members? It cannot be undone.", cur.Name))
			if err != nil || !ok {
				return err
			}
			if _, err := c.DeleteWorkspace(cmd.Context(), req(s, &workspacev1.DeleteWorkspaceRequest{WorkspaceId: cur.ID, ConfirmName: cur.Name})); err != nil {
				return rpcErr(err)
			}
			_, _ = s.auth.ForgetWorkspace(cmd.Context(), req(s, &authv1.ForgetWorkspaceRequest{Id: cur.ID}))
			_ = saveCurrent(nil)
			return a.done(map[string]any{"deleted": cur.ID}, "Workspace %s deleted.\n", cur.Name)
		},
	}
	del.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")

	var limit int
	audit := &cobra.Command{
		Use:   "audit",
		Short: "Who changed members, roles and permissions, and when (admin)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, cur, c, err := a.inWorkspace(cmd.Context())
			if err != nil {
				return err
			}
			res, err := c.ListAudit(cmd.Context(), req(s, &workspacev1.ListAuditRequest{WorkspaceId: cur.ID, Limit: int32(limit)}))
			if err != nil {
				return rpcErr(err)
			}
			if a.json {
				return a.writeProto(res.Msg)
			}
			w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "WHEN\tWHO\tWHAT\tWHOM\tDETAIL")
			for _, e := range res.Msg.Events {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.At.AsTime().Local().Format("2 Jan 15:04"), e.Actor, e.Action, e.Target, e.Detail)
			}
			return w.Flush()
		},
	}
	audit.Flags().IntVarP(&limit, "limit", "n", 50, "how many events")

	demoCmd := &cobra.Command{
		Use:   "demo [name]",
		Short: "Create a demo workspace with the demo project, to try Devopus or debug it",
		Long: "Creates a workspace (named demo unless given) with the project of examples/demo:\n" +
			"three applications on Node, Python and PHP, Postgres and a mail catcher.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := "demo"
			if len(args) == 1 {
				name = args[0]
			}
			project, _, err := importer.Parse(demo.Config)
			if err != nil {
				return fail(ExitError, "the demo project: %v", err)
			}
			config, err := project.YAML()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			s, err := a.session(ctx)
			if err != nil {
				return err
			}
			url := strings.TrimRight(server, "/")
			c := s.server(url)
			res, err := c.CreateWorkspace(ctx, req(s, &workspacev1.CreateWorkspaceRequest{Name: name}))
			if err != nil {
				return rpcErr(err)
			}
			w := res.Msg.Workspace
			s.register(ctx, w, url)
			if err := saveCurrent(&current{ID: w.Id, Name: w.Name, ServerURL: url}); err != nil {
				return fail(ExitError, "%v", err)
			}
			pr, err := c.CreateProject(ctx, req(s, &workspacev1.CreateProjectRequest{WorkspaceId: w.Id, Name: project.Name, Domain: project.Domain, ConfigYaml: string(config)}))
			if err != nil {
				return rpcErr(err)
			}
			// The demo's database password, as a secret of the workspace: made here,
			// encrypted here, never seen by the server.
			if err := a.uploadSecrets(ctx, project.Name, map[string]string{"DB_PASSWORD": random(18)}); err != nil {
				return err
			}
			return a.done(map[string]any{"workspace": w.Id, "name": w.Name, "project": pr.Msg.Project.Name, "version": pr.Msg.Config.Version},
				"✓ Workspace %s with project %s: %d applications, %d services. Its code is in examples/demo of the Devopus repository:\n  devopus up %s --dir examples/demo\n",
				w.Name, project.Name, len(project.Apps), len(project.Services), project.Name)
		},
	}
	demoCmd.Flags().StringVar(&server, "server", serverURL(), "the workspace server ($DEVOPUS_SERVER_URL)")

	cmd.AddCommand(create, demoCmd, list, use, show, rename, transfer, del, audit)
	return cmd
}

func (a *app) inviteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "invite",
		Short: "Invite people into the workspace with a role; accept invites",
		Long: "The admin invites teamleads and developers; a teamlead invites developers into the\n" +
			"projects they lead. The invite goes to the email and works for 7 days; the invitee\n" +
			"signs in with that email and runs 'devopus invite accept <code>'.",
	}
	var role string
	var projects []string
	create := &cobra.Command{
		Use:   "create <email>",
		Short: "Invite an email as a teamlead or developer of projects",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if role == "" {
				return fail(ExitInvalid, "choose the role: --role teamlead or --role developer")
			}
			s, cur, c, err := a.inWorkspace(cmd.Context())
			if err != nil {
				return err
			}
			res, err := c.CreateInvite(cmd.Context(), req(s, &workspacev1.CreateInviteRequest{WorkspaceId: cur.ID, Email: args[0], Role: role, Projects: projects}))
			if err != nil {
				return rpcErr(err)
			}
			inv := res.Msg.Invite
			where := ""
			if len(inv.Projects) > 0 {
				where = " in " + strings.Join(inv.Projects, ", ")
			}
			return a.done(map[string]any{"id": inv.Id, "email": inv.Email, "role": inv.Role, "projects": inv.Projects, "code": res.Msg.Code},
				"✓ Invited %s as %s%s; the invite is emailed and works until %s.\nTo join, they sign in with that email and run:\n  devopus invite accept %s\n",
				inv.Email, inv.Role, where, day(inv.ExpiresAt.AsTime()), res.Msg.Code)
		},
	}
	create.Flags().StringVar(&role, "role", "", "teamlead or developer")
	create.Flags().StringSliceVar(&projects, "project", nil, "projects the role is in (repeat or comma-separate)")

	list := &cobra.Command{
		Use:   "list",
		Short: "Open invites (a teamlead sees their own)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, cur, c, err := a.inWorkspace(cmd.Context())
			if err != nil {
				return err
			}
			res, err := c.ListInvites(cmd.Context(), req(s, &workspacev1.ListInvitesRequest{WorkspaceId: cur.ID}))
			if err != nil {
				return rpcErr(err)
			}
			if a.json {
				return a.writeProto(res.Msg)
			}
			if len(res.Msg.Invites) == 0 {
				fmt.Fprintln(a.stdout, "No open invites.")
				return nil
			}
			w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tEMAIL\tROLE\tPROJECTS\tBY\tUNTIL")
			for _, i := range res.Msg.Invites {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", i.Id, i.Email, i.Role, strings.Join(i.Projects, ","), i.InvitedBy, day(i.ExpiresAt.AsTime()))
			}
			return w.Flush()
		},
	}
	revoke := &cobra.Command{
		Use:   "revoke <invite-id>",
		Short: "Revoke an invite before it is accepted",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, cur, c, err := a.inWorkspace(cmd.Context())
			if err != nil {
				return err
			}
			if _, err := c.RevokeInvite(cmd.Context(), req(s, &workspacev1.RevokeInviteRequest{WorkspaceId: cur.ID, InviteId: args[0]})); err != nil {
				return rpcErr(err)
			}
			return a.done(map[string]any{"revoked": args[0]}, "Invite %s revoked.\n", args[0])
		},
	}
	var server string
	accept := &cobra.Command{
		Use:   "accept <code>",
		Short: "Join the workspace of an invite to your email; it becomes the current one",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.session(cmd.Context())
			if err != nil {
				return err
			}
			url := strings.TrimRight(server, "/")
			res, err := s.server(url).AcceptInvite(cmd.Context(), req(s, &workspacev1.AcceptInviteRequest{Code: args[0]}))
			if err != nil {
				return rpcErr(err)
			}
			w := res.Msg.Workspace
			s.register(cmd.Context(), w, url)
			if err := saveCurrent(&current{ID: w.Id, Name: w.Name, ServerURL: url}); err != nil {
				return fail(ExitError, "%v", err)
			}
			return a.done(map[string]any{"id": w.Id, "name": w.Name, "role": w.Role}, "✓ You joined %s as %s.\n", w.Name, w.Role)
		},
	}
	accept.Flags().StringVar(&server, "server", serverURL(), "the workspace server of the invite ($DEVOPUS_SERVER_URL)")
	cmd.AddCommand(create, list, revoke, accept)
	return cmd
}

func (a *app) memberCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "member",
		Short: "Members of the workspace: roles, permissions, removal",
		Long: "Permissions on top of a developer's role in a project: " + strings.Join(permissionNames(authz.Grantable), ", ") + ".\n" +
			"The admin and the project's teamleads give and take them.",
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "Members with their roles in the workspace and its projects",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, cur, c, err := a.inWorkspace(cmd.Context())
			if err != nil {
				return err
			}
			res, err := c.ListMembers(cmd.Context(), req(s, &workspacev1.ListMembersRequest{WorkspaceId: cur.ID}))
			if err != nil {
				return rpcErr(err)
			}
			if a.json {
				return a.writeProto(res.Msg)
			}
			w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "EMAIL\tROLE\tPROJECTS")
			for _, m := range res.Msg.Members {
				var ps []string
				for _, p := range m.Projects {
					s := p.Project + " (" + p.Role
					if len(p.Grants) > 0 {
						s += " + " + strings.Join(p.Grants, ", ")
					}
					ps = append(ps, s+")")
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", m.Email, m.Role, strings.Join(ps, "; "))
			}
			return w.Flush()
		},
	}
	var project string
	grant := func(give bool) *cobra.Command {
		use, short := "grant <email> <permission>", "Give a developer a permission in a project"
		if !give {
			use, short = "revoke <email> <permission>", "Take a permission back"
		}
		c := &cobra.Command{
			Use:   use,
			Short: short,
			Args:  cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				if project == "" {
					return fail(ExitInvalid, "name the project: --project <name>")
				}
				s, cur, c, err := a.inWorkspace(cmd.Context())
				if err != nil {
					return err
				}
				if give {
					_, err = c.GrantPermission(cmd.Context(), req(s, &workspacev1.GrantPermissionRequest{WorkspaceId: cur.ID, Project: project, Email: args[0], Permission: args[1]}))
				} else {
					_, err = c.RevokePermission(cmd.Context(), req(s, &workspacev1.RevokePermissionRequest{WorkspaceId: cur.ID, Project: project, Email: args[0], Permission: args[1]}))
				}
				if err != nil {
					return rpcErr(err)
				}
				what := "has"
				if !give {
					what = "no longer has"
				}
				return a.done(map[string]any{"email": args[0], "permission": args[1], "project": project, "granted": give},
					"%s %s %s in %s.\n", args[0], what, args[1], project)
			},
		}
		c.Flags().StringVar(&project, "project", "", "the project")
		return c
	}
	remove := &cobra.Command{
		Use:   "remove <email>",
		Short: "Remove a member from a project (--project), or from the workspace (admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, cur, c, err := a.inWorkspace(cmd.Context())
			if err != nil {
				return err
			}
			if project != "" {
				if _, err := c.RemoveFromProject(cmd.Context(), req(s, &workspacev1.RemoveFromProjectRequest{WorkspaceId: cur.ID, Project: project, Email: args[0]})); err != nil {
					return rpcErr(err)
				}
				return a.done(map[string]any{"removed": args[0], "project": project}, "%s removed from %s.\n", args[0], project)
			}
			if _, err := c.RemoveMember(cmd.Context(), req(s, &workspacev1.RemoveMemberRequest{WorkspaceId: cur.ID, Email: args[0]})); err != nil {
				return rpcErr(err)
			}
			return a.done(map[string]any{"removed": args[0]}, "%s removed from %s; their access ended.\n", args[0], cur.Name)
		},
	}
	remove.Flags().StringVar(&project, "project", "", "remove from this project only")
	cmd.AddCommand(list, grant(true), grant(false), remove)
	return cmd
}

func permissionNames(ps []authz.Permission) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = string(p)
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
