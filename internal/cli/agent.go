package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	agentv2 "github.com/amalychev/devopus/gen/devopus/agent/v2"
	"github.com/amalychev/devopus/gen/devopus/agent/v2/agentv2connect"
	"github.com/amalychev/devopus/internal/agent"
	"github.com/amalychev/devopus/internal/agent/client"
	"github.com/amalychev/devopus/internal/agent/paths"
	"github.com/amalychev/devopus/internal/agent/pki"
	"github.com/amalychev/devopus/internal/credentials"
	"github.com/amalychev/devopus/internal/service"
	"github.com/amalychev/devopus/internal/trust"
)

func (a *app) agentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Install, start, stop and inspect the agent on this machine",
		Long: "The agent runs the environments on this machine: it keeps Docker in the state the\n" +
			"projects describe. Commands that need it start it on their own; 'devopus agent install'\n" +
			"makes it a service that starts at login.",
	}
	run := &cobra.Command{
		Use:   "run",
		Short: "Run the agent in the foreground (what the service runs)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			log := slog.New(slog.NewTextHandler(a.stderr, nil))
			err := agent.Run(ctx, agent.Options{Version: Version, Log: log})
			if errors.Is(err, agent.ErrAlreadyRunning) {
				return fail(ExitError, "another agent is running for %s: %v", paths.Home(), err)
			}
			return err
		},
	}
	cmd.AddCommand(
		a.agentInstallCmd(),
		&cobra.Command{
			Use:   "start",
			Short: "Start the agent: its service if installed, otherwise in the background",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if _, err := a.connect(ctx); err != nil {
					return err
				}
				return a.done(map[string]any{"running": true, "pid": client.Pid()}, "The agent is running (pid %d).\n", client.Pid())
			},
		},
		&cobra.Command{
			Use:   "stop",
			Short: "Stop the agent; environments keep running, the next command starts it again",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := client.Stop(ctx); err != nil {
					return fail(ExitError, "%v", err)
				}
				return a.done(map[string]any{"running": false}, "The agent is stopped. Environments keep running; any devopus command starts it again.\n")
			},
		},
		&cobra.Command{
			Use:   "restart",
			Short: "Restart the agent (after rebuilding devopus, for example)",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := client.Stop(ctx); err != nil {
					return fail(ExitError, "%v", err)
				}
				if err := client.Start(); err != nil {
					return fail(ExitError, "start the agent: %v", err)
				}
				if err := client.Wait(ctx, 15*time.Second); err != nil {
					return fail(ExitError, "%v", err)
				}
				return a.done(map[string]any{"running": true, "pid": client.Pid()}, "The agent is running again (pid %d).\n", client.Pid())
			},
		},
		&cobra.Command{
			Use:   "status",
			Short: "Whether the agent runs, its version, service and files",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				version, err := client.Version(ctx, client.New(paths.Socket()))
				return a.componentStatus(service.Agent, err == nil, version, client.Pid(), paths.Log(),
					map[string]string{"data": paths.Home(), "socket": paths.Socket()})
			},
		},
		a.logFileCmd("Show the agent's log", paths.Log),
		a.agentUninstallCmd(),
		run,
	)
	return cmd
}

// componentStatus prints how a component (the agent, the panel) runs on this machine.
func (a *app) componentStatus(component string, running bool, version string, pid int, logFile string, extra map[string]string) error {
	installed := service.Installed(component)
	file := ""
	if s, err := service.For(component); err == nil {
		file, _ = s.File()
	}
	if a.json {
		v := map[string]any{"running": running, "version": version, "pid": pid, "service": installed, "log": logFile}
		if installed {
			v["service_file"] = file
		}
		for k, x := range extra {
			v[k] = x
		}
		return a.writeJSON(v)
	}
	state := "not running"
	if running {
		state = fmt.Sprintf("running, version %s, pid %d", version, pid)
	}
	how := "not installed as a service: 'devopus " + component + " install'"
	if installed {
		how = "service " + file
	}
	w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "State\t%s\n", state)
	fmt.Fprintf(w, "Runs as\t%s\n", how)
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s\t%s\n", strings.ToUpper(k[:1])+k[1:], extra[k])
	}
	fmt.Fprintf(w, "Log\t%s\n", logFile)
	return w.Flush()
}

// logFileCmd prints the end of a component's log; --follow keeps printing.
func (a *app) logFileCmd(short string, file func() string) *cobra.Command {
	var follow bool
	var lines int
	cmd := &cobra.Command{
		Use:   "logs",
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if err := tailFile(ctx, a.stdout, file(), lines, follow); err != nil {
				return fail(ExitError, "%v", err)
			}
			return nil
		},
	}
	// -f is --file everywhere, so only --follow, as in devopus logs.
	cmd.Flags().BoolVar(&follow, "follow", false, "keep printing new lines")
	cmd.Flags().IntVarP(&lines, "lines", "n", 100, "how many lines from the end")
	return cmd
}

// tailFile prints the last n lines of file and, with follow, what is appended.
func tailFile(ctx context.Context, w io.Writer, file string, n int, follow bool) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	all := strings.SplitAfter(string(data), "\n")
	if len(all) > 0 && all[len(all)-1] == "" {
		all = all[:len(all)-1]
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	for _, l := range all {
		io.WriteString(w, l)
	}
	for follow {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(300 * time.Millisecond):
		}
		if _, err := io.Copy(w, f); err != nil {
			return err
		}
	}
	return nil
}

// connect returns a client to the agent, starting it if needed.
func (a *app) connect(ctx context.Context) (agentv2connect.AgentServiceClient, error) {
	c, started, err := client.Ensure(ctx, Version)
	if err != nil {
		return nil, fail(ExitError, "%v", err)
	}
	if started && !a.json && !service.Installed(service.Agent) {
		fmt.Fprintf(a.stderr, "Started the agent in the background (log: %s); 'devopus agent install' runs it as a service\n", paths.Log())
	}
	return c, nil
}

func (a *app) statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the agent, the container runtime and the environments",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := context.Background()
			c := client.New(paths.Socket())
			res, err := c.Status(ctx, connect.NewRequest(&agentv2.StatusRequest{}))
			if err != nil {
				if connect.CodeOf(err) == connect.CodeUnavailable {
					if a.json {
						return a.writeJSON(map[string]any{"agent": "not running"})
					}
					fmt.Fprintln(a.stdout, "Agent: not running. 'devopus up' starts it.")
					return nil
				}
				return fail(ExitError, "%v", err)
			}
			if a.json {
				return a.writeProto(res.Msg)
			}
			a.printStatus(res.Msg)
			return nil
		},
	}
}

func (a *app) printStatus(s *agentv2.StatusResponse) {
	fmt.Fprintf(a.stdout, "Agent    %s · desired revision %d\n", s.AgentVersion, s.DesiredRevision)
	if s.Account != nil {
		fmt.Fprintf(a.stdout, "Account  %s\n", s.Account.Email)
	} else {
		fmt.Fprintln(a.stdout, "Account  the agent has no session yet ('devopus agent logs' tells why)")
	}
	if l := s.Link; l != nil && l.TrustOffered != "" {
		fmt.Fprintf(a.stdout, "Trust    ! the workspace server offers another root of trust (%s) than the one this agent keeps (%s).\n"+
			"         Signatures count against the kept one. Ask the workspace's admin: 'devopus trust show' on their machine shows theirs.\n", l.TrustOffered, l.TrustRoot)
	} else if l != nil && l.TrustRoot != "" {
		fmt.Fprintf(a.stdout, "Trust    root %s\n", l.TrustRoot)
	}
	switch l := s.Link; {
	case l.GetConnected():
		fmt.Fprintf(a.stdout, "Link     workspace %s at %s · connected\n", l.Workspace, l.Server)
	case l.GetError() != "":
		fmt.Fprintf(a.stdout, "Link     %s\n", l.Error)
	}
	rt := s.Runtime
	if rt.Reachable {
		fmt.Fprintf(a.stdout, "Runtime  %s %s (%s)\n", rt.Name, rt.ServerVersion, rt.Endpoint)
	} else {
		msg := rt.Error
		if msg == "" {
			msg = "not checked yet"
		}
		fmt.Fprintf(a.stdout, "Runtime  %s — %s\n", rt.Name, msg)
	}
	a.printProxy(s.Proxy)
	a.printCA(s.Ca)
	fmt.Fprintf(a.stdout, "Panel    %s\n", panelLine(context.Background()))
	if len(s.Projects) == 0 {
		fmt.Fprintln(a.stdout, "\nNo projects. Run 'devopus up' in a folder with workspace.yaml.")
		return
	}
	fmt.Fprintln(a.stdout)
	w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROJECT\tENV\tKIND\tPHASE\tURL\tNETWORK")
	byProject := map[string][]*agentv2.EnvironmentStatus{}
	for _, e := range s.Environments {
		byProject[e.Project] = append(byProject[e.Project], e)
	}
	for _, p := range s.Projects {
		envs := byProject[p.Name]
		if len(envs) == 0 {
			fmt.Fprintf(w, "%s\t-\t-\tpending\t-\t-\n", p.Name)
		}
		for _, e := range envs {
			net := "-"
			if e.Network != nil && e.Network.Name != "" {
				net = e.Network.Name + " " + e.Network.Subnet
			}
			phase := phaseName(e.Phase)
			if e.Error != "" {
				phase += ": " + e.Error
			}
			url := e.Url
			if url == "" {
				url = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", p.Name, e.Id, kindName(e.Kind), phase, url, net)
		}
	}
	w.Flush()
	for _, p := range s.Projects {
		if p.Pending != nil {
			fmt.Fprintf(a.stdout, "\n%s: settings v%d wait for you: they run new code on this machine. 'devopus changes' shows them.\n", p.Name, p.Pending.Version)
		}
	}
	for _, e := range s.Environments {
		a.printUnits(e)
	}
}

// printUnits lists the services, applications and workers of an environment.
func (a *app) printUnits(e *agentv2.EnvironmentStatus) {
	if len(e.Units) == 0 {
		return
	}
	fmt.Fprintf(a.stdout, "\n%s · %s\n", e.Project, e.Id)
	w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	for _, u := range e.Units {
		addr := u.Address
		if addr == "" {
			addr = "-"
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n", u.Name, u.Role, u.State, addr, strings.Join(u.Routes, " "))
	}
	w.Flush()
	for _, u := range e.Units {
		if u.Error != "" {
			fmt.Fprintf(a.stdout, "  %s: %s\n", u.Name, strings.ReplaceAll(u.Error, "\n", "\n    "))
		}
	}
}

// printCA shows where the local CA is and whether this machine trusts it.
func (a *app) printCA(ca *agentv2.CAInfo) {
	if ca.GetRootPath() == "" {
		return
	}
	state := "trusted"
	if loaded, err := pki.LoadCA(filepath.Dir(ca.RootPath)); err != nil {
		state = err.Error()
	} else if ok, err := trust.Trusted(loaded); err != nil || !ok {
		state = "not trusted: 'devopus doctor --fix'"
	}
	fmt.Fprintf(a.stdout, "CA       %s · %s\n", ca.RootPath, state)
}

// trustHint tells people why the browser warns, once per command.
func (a *app) trustHint() {
	ca, err := pki.LoadCA(paths.CA())
	if err != nil {
		return
	}
	if ok, err := trust.Trusted(ca); err == nil && !ok {
		fmt.Fprintln(a.stderr, "Browsers will warn about the certificate until you trust the local CA: devopus doctor --fix")
	}
}

func (a *app) upCmd() *cobra.Command {
	var detach bool
	var timeout time.Duration
	var dir string
	cmd := &cobra.Command{
		Use:   "up [project…]",
		Short: "Bring up the projects of the current workspace on this machine",
		Long: "Brings up the base environments of the current workspace's projects (all, or those\n" +
			"named) on this machine; the agent gets them from the workspace server, and brings new\n" +
			"ones up on its own. 'devopus down' keeps a project stopped here until 'devopus up'.\n\n" +
			"--dir is the folder of a project's path applications on this machine (the demo:\n" +
			"devopus up demo --dir examples/demo); the agent remembers it.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.upServer(args, dir, detach, timeout)
		},
	}
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "return without waiting for the environment")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "how long to wait")
	cmd.Flags().StringVar(&dir, "dir", "", "folder of the project's path applications on this machine")
	return cmd
}

// workspaceProjects are the projects of the current workspace the agent has. Right
// after a sign-in or a switch the agent may still be fetching them: it waits a while.
func (a *app) workspaceProjects(ctx context.Context, c agentv2connect.AgentServiceClient, want []string) (*credentials.Workspace, []*agentv2.ProjectSpec, error) {
	cur, err := credentials.CurrentWorkspace()
	if err != nil {
		return nil, nil, fail(ExitError, "%v", err)
	}
	if cur == nil {
		return nil, nil, fail(ExitInvalid, "no workspace.yaml here, and no workspace chosen on this machine: devopus workspace use <name>")
	}
	told := false
	deadline := time.Now().Add(time.Minute)
	for {
		st, err := c.Status(ctx, connect.NewRequest(&agentv2.StatusRequest{}))
		if err != nil {
			return nil, nil, rpcError(err)
		}
		var have []*agentv2.ProjectSpec
		names := map[string]bool{}
		for _, p := range st.Msg.Projects {
			if p.WorkspaceId == cur.ID && !p.Orphaned {
				have = append(have, p)
				names[p.Name] = true
			}
		}
		missing := ""
		for _, w := range want {
			if !names[w] {
				missing = w
			}
		}
		connected := st.Msg.Link.GetConnected()
		switch {
		case len(have) > 0 && missing == "":
			return cur, have, nil
		case connected && missing != "":
			return nil, nil, fail(ExitInvalid, "%s has no project %q for you; 'devopus project list' shows yours", cur.Name, missing)
		case connected:
			return nil, nil, fail(ExitInvalid, "%s has no projects for you yet; 'devopus project list' shows yours", cur.Name)
		case time.Now().After(deadline):
			why := st.Msg.Link.GetError()
			if why == "" {
				why = "no answer"
			}
			return nil, nil, fail(ExitError, "the agent did not get the projects of %s from %s: %s", cur.Name, cur.ServerURL, why)
		}
		if !told && !a.json {
			fmt.Fprintf(a.stderr, "Waiting for the agent to get the projects of %s from the workspace server…\n", cur.Name)
			told = true
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// upServer brings up projects of the current workspace.
func (a *app) upServer(names []string, dir string, detach bool, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c, err := a.connect(ctx)
	if err != nil {
		return err
	}
	_, projects, err := a.workspaceProjects(ctx, c, names)
	if err != nil {
		return err
	}
	if len(names) > 0 {
		var chosen []*agentv2.ProjectSpec
		for _, p := range projects {
			if slices.Contains(names, p.Name) {
				chosen = append(chosen, p)
			}
		}
		projects = chosen
	}
	if dir != "" {
		if len(projects) != 1 {
			return fail(ExitInvalid, "--dir is the folder of one project: name it")
		}
		if dir, err = filepath.Abs(dir); err != nil {
			return err
		}
	}
	w, err := watch(ctx, c)
	if err != nil {
		return err
	}
	defer w.Close()
	type started struct {
		name, env string
		rev       uint64
	}
	var ups []started
	for _, p := range projects {
		res, err := c.Up(ctx, connect.NewRequest(&agentv2.UpRequest{Project: p.Name, Dir: dir}))
		if err != nil {
			return rpcError(err)
		}
		ups = append(ups, started{p.Name, res.Msg.EnvironmentId, res.Msg.Revision})
	}
	if detach {
		var out []map[string]any
		for _, u := range ups {
			out = append(out, map[string]any{"project": u.name, "environment_id": u.env, "revision": u.rev})
		}
		return a.done(map[string]any{"projects": out}, "%d projects are coming up; 'devopus status' shows progress.\n", len(ups))
	}
	for _, u := range ups {
		if err := a.awaitRunning(ctx, c, w, u.env, u.rev, u.name); err != nil {
			return err
		}
	}
	return nil
}

// awaitRunning follows an environment until it runs, then prints its URL and routes.
func (a *app) awaitRunning(ctx context.Context, c agentv2connect.AgentServiceClient, w *connect.ServerStreamForClient[agentv2.WatchResponse], envID string, rev uint64, what string) error {
	final, err := a.follow(w, []string{envID}, rev, true, func(s *agentv2.EnvironmentStatus) bool {
		return s.Phase == agentv2.Phase_PHASE_RUNNING
	})
	if err != nil {
		return err
	}
	if a.json {
		return a.writeProto(final[envID])
	}
	fmt.Fprintf(a.stdout, "%s is up: %s (environment %s)\n", what, final[envID].Url, envID)
	if routes, err := c.Routes(ctx, connect.NewRequest(&agentv2.RoutesRequest{EnvironmentId: envID})); err == nil {
		w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
		for _, r := range routes.Msg.Routes {
			fmt.Fprintf(w, "  %s\t%s\n", r.Url, r.Target)
		}
		w.Flush()
	}
	for _, n := range final[envID].Notes {
		fmt.Fprintln(a.stdout, "  note: "+n)
	}
	a.trustHint()
	return nil
}

func (a *app) downCmd() *cobra.Command {
	var volumes bool
	var project string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "down [project]",
		Short: "Stop the environments of a project on this machine; --volumes removes them with their data",
		Long: "Stops the environments of a project on this machine: the one named, or the current\n" +
			"workspace's only one. It stays stopped here until 'devopus up', whatever changes on\n" +
			"the server.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				project = args[0]
			}
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
			st, err := c.Status(ctx, connect.NewRequest(&agentv2.StatusRequest{}))
			if err != nil {
				return rpcError(err)
			}
			var envs []string
			for _, e := range st.Msg.Environments {
				if e.Project == project {
					envs = append(envs, e.Id)
				}
			}
			w, err := watch(ctx, c)
			if err != nil {
				return err
			}
			defer w.Close()
			res, err := c.Down(ctx, connect.NewRequest(&agentv2.DownRequest{Project: project, Purge: volumes}))
			if err != nil {
				return rpcError(err)
			}
			if _, err := a.follow(w, envs, res.Msg.Revision, false, func(s *agentv2.EnvironmentStatus) bool {
				if volumes {
					return s.Removed
				}
				return s.Phase == agentv2.Phase_PHASE_STOPPED
			}); err != nil {
				return err
			}
			verb := "stopped"
			if volumes {
				verb = "removed with its data"
			}
			return a.done(map[string]any{"project": project, "revision": res.Msg.Revision}, "%s is %s.\n", project, verb)
		},
	}
	cmd.Flags().BoolVar(&volumes, "volumes", false, "remove the environments and their data")
	cmd.Flags().StringVar(&project, "project", "", "project (default: the current workspace's only one)")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "how long to wait")
	return cmd
}

// watch opens a status stream and returns once the agent delivers every change.
func watch(ctx context.Context, c agentv2connect.AgentServiceClient) (*connect.ServerStreamForClient[agentv2.WatchResponse], error) {
	w, err := c.Watch(ctx, connect.NewRequest(&agentv2.WatchRequest{}))
	if err != nil {
		return nil, rpcError(err)
	}
	for w.Receive() {
		if w.Msg().Synced {
			return w, nil
		}
	}
	w.Close()
	if err := w.Err(); err != nil {
		return nil, rpcError(err)
	}
	return nil, fail(ExitError, "the agent closed the connection")
}

// follow waits until done holds for every environment in ids at the given revision, or
// one of them fails, printing the steps if asked. It returns the final statuses by ID.
func (a *app) follow(w *connect.ServerStreamForClient[agentv2.WatchResponse], ids []string, rev uint64, steps bool, done func(*agentv2.EnvironmentStatus) bool) (map[string]*agentv2.EnvironmentStatus, error) {
	final := map[string]*agentv2.EnvironmentStatus{}
	pending := map[string]bool{}
	printed := map[string]map[string]string{}
	for _, id := range ids {
		pending[id] = true
		printed[id] = map[string]string{}
	}
	for len(pending) > 0 && w.Receive() {
		s := w.Msg().Status
		if s == nil || !pending[s.Id] {
			continue
		}
		if steps && !a.json {
			a.printSteps(s, printed[s.Id])
		}
		if s.AppliedRevision < rev {
			continue
		}
		if done(s) {
			final[s.Id] = s
			delete(pending, s.Id)
			continue
		}
		if s.Phase == agentv2.Phase_PHASE_ERROR {
			return final, fail(ExitError, "environment %s failed: %s", s.Id, s.Error)
		}
	}
	if len(pending) == 0 {
		return final, nil
	}
	if err := w.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || connect.CodeOf(err) == connect.CodeDeadlineExceeded {
			return nil, fail(ExitError, "timed out; 'devopus status' shows where it stopped")
		}
		return nil, rpcError(err)
	}
	return nil, fail(ExitError, "the agent closed the connection")
}

// printSteps prints each change of a step: its state, and what a running step is doing.
func (a *app) printSteps(s *agentv2.EnvironmentStatus, printed map[string]string) {
	for _, st := range s.Steps {
		key := st.State.String() + "\x00" + st.Message
		if printed[st.Name] == key {
			continue
		}
		printed[st.Name] = key
		switch st.State {
		case agentv2.StepState_STEP_STATE_RUNNING:
			fmt.Fprintln(a.stdout, "  "+strings.TrimSpace("… "+st.Name+"  "+st.Message))
		case agentv2.StepState_STEP_STATE_DONE:
			fmt.Fprintf(a.stdout, "  ✓ %s  %s\n", st.Name, st.Message)
		case agentv2.StepState_STEP_STATE_FAILED:
			fmt.Fprintf(a.stdout, "  ✗ %s  %s\n", st.Name, st.Message)
		}
	}
}

// done prints a result for people or for --json.
func (a *app) done(v map[string]any, format string, args ...any) error {
	if a.json {
		return a.writeJSON(v)
	}
	fmt.Fprintf(a.stdout, format, args...)
	return nil
}

func (a *app) writeProto(m proto.Message) error {
	data, err := protojson.MarshalOptions{Multiline: true, Indent: "  ", EmitUnpopulated: false}.Marshal(m)
	if err != nil {
		return err
	}
	_, err = io.WriteString(a.stdout, string(data)+"\n")
	return err
}

// rpcError maps agent errors to exit codes: a rejected config is exit 2.
func rpcError(err error) error {
	msg := err.Error()
	var ce *connect.Error
	if errors.As(err, &ce) {
		msg = ce.Message()
	}
	switch connect.CodeOf(err) {
	case connect.CodeInvalidArgument, connect.CodeAlreadyExists, connect.CodeNotFound:
		return fail(ExitInvalid, "%s", msg)
	case connect.CodeUnavailable:
		return fail(ExitError, "%v", client.ErrNotRunning)
	}
	return fail(ExitError, "%s", msg)
}

func phaseName(p agentv2.Phase) string {
	return strings.ToLower(strings.TrimPrefix(p.String(), "PHASE_"))
}

func kindName(k agentv2.EnvironmentKind) string {
	return strings.ToLower(strings.TrimPrefix(k.String(), "ENVIRONMENT_KIND_"))
}
