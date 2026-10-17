package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	workspacev1 "github.com/amalychev/devopus/gen/devopus/workspace/v1"
	"github.com/amalychev/devopus/gen/devopus/workspace/v1/workspacev1connect"
	"github.com/amalychev/devopus/internal/agent/link"
	"github.com/amalychev/devopus/internal/credentials"
	"github.com/amalychev/devopus/internal/selfhost"
)

// Servers of the admin (R13, ADR-0019): machines with an agent that run projects of
// the workspace — staging, production.
func (a *app) serversCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "servers",
		Short: "Servers of the admin that run projects: staging, production (servers.manage)",
		Long: "A server is a machine of yours with the Devopus agent: it runs the projects you give\n" +
			"it, from the same settings as everyone's machines. It joins with a one-time code and\n" +
			"runs only versions signed by people the admin certified ('devopus trust').",
	}
	var projects []string
	add := &cobra.Command{
		Use:   "add <name> --project <project>…",
		Short: "Add a server; prints the command that joins it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			s, cur, c, err := a.inWorkspace(ctx)
			if err != nil {
				return err
			}
			res, err := c.CreateServer(ctx, req(s, &workspacev1.CreateServerRequest{WorkspaceId: cur.ID, Name: args[0], Projects: projects}))
			if err != nil {
				return rpcErr(err)
			}
			join := fmt.Sprintf("sudo devopus agent install --server --url %s --join %s", cur.ServerURL, res.Msg.JoinCode)
			return a.done(map[string]any{"name": args[0], "projects": res.Msg.Server.Projects, "join_code": res.Msg.JoinCode, "join": join,
				"expires_at": res.Msg.ExpiresAt.AsTime()},
				"✓ Server %s added to %s for %s.\nOn the server (Linux with Docker; the code works once, for an hour):\n\n  %s\n\n"+
					"It runs only versions signed by people the admin certified: 'devopus trust show'.\n",
				args[0], cur.Name, dash(strings.Join(res.Msg.Server.Projects, ", ")), join)
		},
	}
	add.Flags().StringSliceVar(&projects, "project", nil, "projects it runs")
	list := &cobra.Command{
		Use:   "list",
		Short: "Servers and whether their agents are connected",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			s, cur, c, err := a.inWorkspace(ctx)
			if err != nil {
				return err
			}
			res, err := c.ListServers(ctx, req(s, &workspacev1.ListServersRequest{WorkspaceId: cur.ID}))
			if err != nil {
				return rpcErr(err)
			}
			if a.json {
				return a.writeJSON(res.Msg.Servers)
			}
			if len(res.Msg.Servers) == 0 {
				fmt.Fprintln(a.stdout, "No servers. 'devopus servers add <name> --project <project>' adds one.")
				return nil
			}
			w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tPROJECTS\tAGENT\tLAST SEEN")
			for _, sv := range res.Msg.Servers {
				state, seen := "not joined yet", "-"
				if sv.Joined {
					state = "joined"
				}
				if sv.LastSeen != nil {
					seen = sv.LastSeen.AsTime().Local().Format("2 Jan 15:04")
					state = dash(sv.AgentVersion)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", sv.Name, dash(strings.Join(sv.Projects, ", ")), state, seen)
			}
			return w.Flush()
		},
	}
	var setProjects []string
	set := &cobra.Command{
		Use:   "set <name> --project <project>…",
		Short: "Set which projects a server runs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			s, cur, c, err := a.inWorkspace(ctx)
			if err != nil {
				return err
			}
			res, err := c.SetServerProjects(ctx, req(s, &workspacev1.SetServerProjectsRequest{WorkspaceId: cur.ID, Name: args[0], Projects: setProjects}))
			if err != nil {
				return rpcErr(err)
			}
			return a.done(map[string]any{"name": args[0], "projects": res.Msg.Server.Projects},
				"✓ %s runs %s.\n", args[0], dash(strings.Join(res.Msg.Server.Projects, ", ")))
		},
	}
	set.Flags().StringSliceVar(&setProjects, "project", nil, "projects it runs (all of them: the list replaces the old one)")
	rm := &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a server: its token stops working, its agent stops the projects",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			s, cur, c, err := a.inWorkspace(ctx)
			if err != nil {
				return err
			}
			if _, err := c.RemoveServer(ctx, req(s, &workspacev1.RemoveServerRequest{WorkspaceId: cur.ID, Name: args[0]})); err != nil {
				return rpcErr(err)
			}
			return a.done(map[string]any{"removed": args[0]}, "✓ Server %s removed from %s. On it, 'sudo devopus agent uninstall --server' removes the agent.\n", args[0], cur.Name)
		},
	}
	cmd.AddCommand(add, list, set, rm)
	return cmd
}

// installServerAgent makes this machine a server of the workspace: the join code
// traded for the device token, the agent a system service.
func (a *app) installServerAgent(ctx context.Context, url, code string, dryRun bool) error {
	if runtime.GOOS != "linux" {
		return fail(ExitInvalid, "a server's agent runs on Linux (systemd and Docker); on a laptop, 'devopus agent install'")
	}
	if url == "" || code == "" {
		return fail(ExitInvalid, "--url and --join come from 'devopus servers add' on the admin's machine")
	}
	exe, err := os.Executable()
	if err != nil {
		return fail(ExitError, "%v", err)
	}
	var h selfhost.Host = selfhost.System{}
	cfg := &link.ServerConfig{URL: strings.TrimRight(url, "/")}
	if dryRun {
		h = selfhost.DryRun{Out: a.stdout}
		cfg.Token, cfg.Name = "…", "(from the join code)"
		fmt.Fprintln(a.stdout, "Would join the workspace with the code, then:")
	} else {
		if os.Geteuid() != 0 {
			return fail(ExitInvalid, "run it as root: sudo devopus agent install --server …")
		}
		jctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		c := workspacev1connect.NewWorkspaceServiceClient(&http.Client{Timeout: 30 * time.Second}, cfg.URL)
		res, err := c.JoinServer(jctx, connect.NewRequest(&workspacev1.JoinServerRequest{Code: strings.TrimSpace(code),
			DeviceId: credentials.DeviceID(), DeviceName: credentials.DeviceName()}))
		if err != nil {
			return rpcErr(err)
		}
		m := res.Msg
		cfg.WorkspaceID, cfg.WorkspaceName, cfg.ServerID, cfg.Name, cfg.Token = m.WorkspaceId, m.WorkspaceName, m.ServerId, m.Name, m.Token
	}
	if err := selfhost.InstallAgent(ctx, h, selfhost.AgentOptions{Binary: exe, Config: cfg, Log: a.stderr}); err != nil {
		return fail(ExitError, "%v", err)
	}
	if dryRun {
		return nil
	}
	return a.done(map[string]any{"server": cfg.Name, "workspace": cfg.WorkspaceName},
		"✓ This machine is server %s of %s; its agent runs as the service %s.\n"+
			"It runs versions signed by people the admin certified. 'sudo devopus status' shows what runs;\n"+
			"'sudo devopus changes' shows versions that wait.\n", cfg.Name, cfg.WorkspaceName, selfhost.AgentService)
}

// uninstallServerAgent stops a server's environments and removes its agent's
// service; purge removes their data and the agent's directory too.
func (a *app) uninstallServerAgent(purge, yes bool) error {
	if os.Geteuid() != 0 {
		return fail(ExitInvalid, "run it as root: sudo devopus agent uninstall --server")
	}
	what := "stop its environments (their data stays) and remove the service " + selfhost.AgentService
	if purge {
		what = "remove its environments with their data, the service " + selfhost.AgentService + " and " + selfhost.AgentDataDir
	}
	ok, err := a.confirm(yes, "Uninstall the agent of this server? It will "+what+".")
	if err != nil {
		return err
	}
	if !ok {
		return fail(ExitError, "nothing changed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if n, err := a.stopEnvironments(ctx, purge); err != nil {
		a.progress("! environments: %v", err)
	} else {
		a.progress("✓ %d environments stopped", n)
	}
	if err := selfhost.UninstallAgent(ctx, selfhost.System{}, purge); err != nil {
		return fail(ExitError, "%v", err)
	}
	return a.done(map[string]any{"removed": true}, "✓ The agent of this server is removed. 'devopus servers rm <name>' removes the server from the workspace.\n")
}
