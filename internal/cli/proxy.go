package cli

import (
	"context"
	"fmt"
	"os/exec"
	goruntime "runtime"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	agentv1 "github.com/devcorb/devcorb/gen/devcorb/agent/v1"
	"github.com/devcorb/devcorb/internal/agent/client"
	"github.com/devcorb/devcorb/internal/agent/paths"
)

func (a *app) routesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "routes [env]",
		Short: "Show the proxy's routing table",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			c := client.New(paths.Socket())
			req := &agentv1.RoutesRequest{}
			if len(args) == 1 {
				req.EnvironmentId = args[0]
			}
			res, err := c.Routes(ctx, connect.NewRequest(req))
			if err != nil {
				return rpcError(err)
			}
			if a.json {
				return a.writeProto(res.Msg)
			}
			a.printProxy(res.Msg.Proxy)
			if len(res.Msg.Routes) == 0 {
				fmt.Fprintln(a.stdout, "\nNo routes.")
				return nil
			}
			fmt.Fprintln(a.stdout)
			w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "URL\tPATH\tENV\tPROJECT\tTARGET")
			for _, r := range res.Msg.Routes {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Url, r.Path, r.EnvironmentId, r.Project, r.Target)
			}
			return w.Flush()
		},
	}
}

func (a *app) openCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "open [env]",
		Short: "Open an environment in the browser (default: the project's base environment)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			c := client.New(paths.Socket())
			st, err := c.Status(ctx, connect.NewRequest(&agentv1.StatusRequest{}))
			if err != nil {
				return rpcError(err)
			}
			var env *agentv1.EnvironmentStatus
			if len(args) == 1 {
				for _, e := range st.Msg.Environments {
					if e.Id == args[0] || e.Slug == args[0] {
						env = e
					}
				}
				if env == nil {
					return fail(ExitInvalid, "no environment %q; 'devcorb status' lists them", args[0])
				}
			} else {
				res, _, err := a.load()
				if err != nil {
					return err
				}
				for _, e := range st.Msg.Environments {
					if e.Project == res.Workspace.Project && e.Kind == agentv1.EnvironmentKind_ENVIRONMENT_KIND_BASE {
						env = e
					}
				}
				if env == nil {
					return fail(ExitError, "%s is not up; run 'devcorb up'", res.Workspace.Project)
				}
			}
			if env.Url == "" {
				return fail(ExitError, "environment %s has no URL yet (%s)", env.Id, phaseName(env.Phase))
			}
			if a.json {
				return a.writeJSON(map[string]string{"url": env.Url})
			}
			fmt.Fprintln(a.stdout, env.Url)
			opener := "xdg-open"
			if goruntime.GOOS == "darwin" {
				opener = "open"
			}
			if err := exec.Command(opener, env.Url).Run(); err != nil {
				return fail(ExitError, "%s %s: %v", opener, env.Url, err)
			}
			return nil
		},
	}
}

func (a *app) proxyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "proxy",
		Short: "Manage the local proxy (ports 80/443)",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "restart",
		Short: "Recreate the proxy: it probes 80/443 again (URLs change if the ports do)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := a.currentProxy()
			if err != nil {
				return err
			}
			return a.setProxy(&agentv1.SetProxyRequest{CoexistHttpPort: st.CoexistHttpPort, CoexistHttpsPort: st.CoexistHttpsPort, Restart: true})
		},
	})
	var httpPort, httpsPort uint32
	var off bool
	coexist := &cobra.Command{
		Use:   "coexist --http PORT [--https PORT] | --off",
		Short: "Share 80/443 with a dev server (Herd, Valet, nginx) moved to other ports",
		Long: "Move your dev server off 80/443 first (to 8080/8443, say), then run\n" +
			"  devcorb proxy coexist --http 8080 --https 8443\n" +
			"Devcorb takes 80/443 and passes every host it does not serve to the dev server:\n" +
			"HTTP as is, HTTPS without decrypting it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if off == (httpPort != 0) {
				return fail(ExitInvalid, "pass either --http (and optionally --https) or --off")
			}
			return a.setProxy(&agentv1.SetProxyRequest{CoexistHttpPort: httpPort, CoexistHttpsPort: httpsPort})
		},
	}
	coexist.Flags().Uint32Var(&httpPort, "http", 0, "host port the dev server now serves HTTP on")
	coexist.Flags().Uint32Var(&httpsPort, "https", 0, "host port the dev server now serves HTTPS on")
	coexist.Flags().BoolVar(&off, "off", false, "stop passing foreign hosts to a dev server")
	cmd.AddCommand(coexist)
	return cmd
}

func (a *app) currentProxy() (*agentv1.ProxyStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := client.New(paths.Socket()).Status(ctx, connect.NewRequest(&agentv1.StatusRequest{}))
	if err != nil {
		return nil, rpcError(err)
	}
	return res.Msg.Proxy, nil
}

// setProxy changes the settings and waits for the agent to apply them.
func (a *app) setProxy(req *agentv1.SetProxyRequest) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := client.New(paths.Socket())
	st, err := c.Status(ctx, connect.NewRequest(&agentv1.StatusRequest{}))
	if err != nil {
		return rpcError(err)
	}
	var ids []string
	for _, e := range st.Msg.Environments {
		ids = append(ids, e.Id)
	}
	w, err := watch(ctx, c)
	if err != nil {
		return err
	}
	defer w.Close()
	res, err := c.SetProxy(ctx, connect.NewRequest(req))
	if err != nil {
		return rpcError(err)
	}
	if len(ids) > 0 {
		if _, err := a.follow(w, ids, res.Msg.Revision, func(s *agentv1.EnvironmentStatus) bool {
			return s.Phase == agentv1.Phase_PHASE_RUNNING || s.Phase == agentv1.Phase_PHASE_STOPPED
		}); err != nil {
			return err
		}
	}
	p, err := a.currentProxy()
	if err != nil {
		return err
	}
	if a.json {
		return a.writeProto(p)
	}
	if len(ids) == 0 {
		fmt.Fprintln(a.stdout, "Saved; the proxy starts with the first environment.")
		return nil
	}
	a.printProxy(p)
	return nil
}

// printProxy is the one-line proxy summary of status and routes.
func (a *app) printProxy(p *agentv1.ProxyStatus) {
	switch p.GetState() {
	case "", "absent":
		fmt.Fprintln(a.stdout, "Proxy    not running (no environments)")
		return
	case "error":
		fmt.Fprintf(a.stdout, "Proxy    error: %s\n", p.Error)
	default:
		link := "xDS connected"
		if !p.Connected {
			link = "xDS not connected"
		}
		if p.Rejected != "" {
			link = "configuration rejected: " + p.Rejected
		}
		fmt.Fprintf(a.stdout, "Proxy    %s · %s mode, ports %d/%d · %s\n", p.State, p.Mode, p.HttpPort, p.HttpsPort, link)
		if p.Mode == "coexist" {
			fmt.Fprintf(a.stdout, "         other hosts → dev server on %d/%d\n", p.CoexistHttpPort, p.CoexistHttpsPort)
		}
		for _, r := range p.Relays {
			fmt.Fprintf(a.stdout, "         relay %s (the runtime publishes IPv4 only)\n", r)
		}
	}
	for _, c := range p.Conflicts {
		fmt.Fprintf(a.stdout, "         %s\n", conflictLine(c.Address, c.Owner, c.Pid))
	}
	if len(p.Conflicts) > 0 {
		fmt.Fprintf(a.stdout, "         → %s\n", p.Conflicts[0].Hint)
	}
}
