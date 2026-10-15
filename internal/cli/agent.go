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
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/devopus/devopus/gen/devopus/agent/v1"
	"github.com/devopus/devopus/gen/devopus/agent/v1/agentv1connect"
	"github.com/devopus/devopus/internal/agent"
	"github.com/devopus/devopus/internal/agent/client"
	"github.com/devopus/devopus/internal/agent/paths"
	"github.com/devopus/devopus/internal/agent/pki"
	"github.com/devopus/devopus/internal/trust"
)

func (a *app) agentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run the Devopus agent",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "run",
		Short: "Run the agent in the foreground",
		Long: "Runs the agent in the foreground until interrupted. Commands that need the agent\n" +
			"start it in the background on their own; 'devopus install' will register it as a service.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			log := slog.New(slog.NewTextHandler(a.stderr, nil))
			err := agent.Run(ctx, agent.Options{Version: Version, Log: log})
			if errors.Is(err, agent.ErrAlreadyRunning) {
				return fail(ExitError, "%v", err)
			}
			return err
		},
	})
	return cmd
}

// connect returns a client to the agent, starting it if needed.
func (a *app) connect(ctx context.Context) (agentv1connect.AgentServiceClient, error) {
	c, started, err := client.Ensure(ctx)
	if err != nil {
		return nil, fail(ExitError, "%v", err)
	}
	if started && !a.json {
		fmt.Fprintf(a.stderr, "Started the agent in the background (log: %s)\n", paths.Log())
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
			res, err := c.Status(ctx, connect.NewRequest(&agentv1.StatusRequest{}))
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

func (a *app) printStatus(s *agentv1.StatusResponse) {
	fmt.Fprintf(a.stdout, "Agent    %s · desired revision %d\n", s.AgentVersion, s.DesiredRevision)
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
	if len(s.Projects) == 0 {
		fmt.Fprintln(a.stdout, "\nNo projects. Run 'devopus up' in a folder with workspace.yaml.")
		return
	}
	fmt.Fprintln(a.stdout)
	w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROJECT\tENV\tKIND\tPHASE\tURL\tNETWORK")
	byProject := map[string][]*agentv1.EnvironmentStatus{}
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
	for _, e := range s.Environments {
		a.printUnits(e)
	}
}

// printUnits lists the services, modules and workers of an environment.
func (a *app) printUnits(e *agentv1.EnvironmentStatus) {
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
func (a *app) printCA(ca *agentv1.CAInfo) {
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
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Bring up the base environment of the project",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, file, err := a.load()
			if err != nil {
				return err
			}
			a.printDiagnostics(file, res.Diagnostics.Warnings())
			if res.Diagnostics.HasErrors() {
				a.printDiagnostics(file, res.Diagnostics.Errors())
				return fail(ExitInvalid, "fix the errors above, then run 'devopus up' again")
			}
			abs, err := filepath.Abs(file)
			if err != nil {
				return err
			}
			config, err := os.ReadFile(abs)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			c, err := a.connect(ctx)
			if err != nil {
				return err
			}
			// Watch first, so no progress is lost between Up and the subscription.
			w, err := watch(ctx, c)
			if err != nil {
				return err
			}
			defer w.Close()
			up, err := c.Up(ctx, connect.NewRequest(&agentv1.UpRequest{WorkspacePath: abs, Config: config}))
			if err != nil {
				return rpcError(err)
			}
			envID, rev := up.Msg.EnvironmentId, up.Msg.Revision
			if detach {
				return a.done(map[string]any{"environment_id": envID, "revision": rev}, "Environment %s of %s is coming up; 'devopus status' shows progress.\n", envID, res.Workspace.Project)
			}
			return a.awaitRunning(ctx, c, w, envID, rev, res.Workspace.Project)
		},
	}
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "return without waiting for the environment")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "how long to wait")
	return cmd
}

// awaitRunning follows an environment until it runs, then prints its URL and routes.
func (a *app) awaitRunning(ctx context.Context, c agentv1connect.AgentServiceClient, w *connect.ServerStreamForClient[agentv1.WatchResponse], envID string, rev uint64, what string) error {
	final, err := a.follow(w, []string{envID}, rev, true, func(s *agentv1.EnvironmentStatus) bool {
		return s.Phase == agentv1.Phase_PHASE_RUNNING
	})
	if err != nil {
		return err
	}
	if a.json {
		return a.writeProto(final[envID])
	}
	fmt.Fprintf(a.stdout, "%s is up: %s (environment %s)\n", what, final[envID].Url, envID)
	if routes, err := c.Routes(ctx, connect.NewRequest(&agentv1.RoutesRequest{EnvironmentId: envID})); err == nil {
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
		Use:   "down",
		Short: "Stop the environments of the project; --volumes removes them with their data",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if project == "" {
				res, _, err := a.load()
				if err != nil {
					return err
				}
				if res.Workspace == nil {
					return fail(ExitInvalid, "cannot read the project name; pass --project")
				}
				project = res.Workspace.Project
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			c := client.New(paths.Socket())
			if err := client.Ping(ctx, c); err != nil {
				return fail(ExitError, "%v", err)
			}
			st, err := c.Status(ctx, connect.NewRequest(&agentv1.StatusRequest{}))
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
			res, err := c.Down(ctx, connect.NewRequest(&agentv1.DownRequest{Project: project, Purge: volumes}))
			if err != nil {
				return rpcError(err)
			}
			if _, err := a.follow(w, envs, res.Msg.Revision, false, func(s *agentv1.EnvironmentStatus) bool {
				if volumes {
					return s.Removed
				}
				return s.Phase == agentv1.Phase_PHASE_STOPPED
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
	cmd.Flags().StringVar(&project, "project", "", "project name (default: from workspace.yaml)")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "how long to wait")
	return cmd
}

// watch opens a status stream and returns once the agent delivers every change.
func watch(ctx context.Context, c agentv1connect.AgentServiceClient) (*connect.ServerStreamForClient[agentv1.WatchResponse], error) {
	w, err := c.Watch(ctx, connect.NewRequest(&agentv1.WatchRequest{}))
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
func (a *app) follow(w *connect.ServerStreamForClient[agentv1.WatchResponse], ids []string, rev uint64, steps bool, done func(*agentv1.EnvironmentStatus) bool) (map[string]*agentv1.EnvironmentStatus, error) {
	final := map[string]*agentv1.EnvironmentStatus{}
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
		if s.Phase == agentv1.Phase_PHASE_ERROR {
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
func (a *app) printSteps(s *agentv1.EnvironmentStatus, printed map[string]string) {
	for _, st := range s.Steps {
		key := st.State.String() + "\x00" + st.Message
		if printed[st.Name] == key {
			continue
		}
		printed[st.Name] = key
		switch st.State {
		case agentv1.StepState_STEP_STATE_RUNNING:
			fmt.Fprintln(a.stdout, "  "+strings.TrimSpace("… "+st.Name+"  "+st.Message))
		case agentv1.StepState_STEP_STATE_DONE:
			fmt.Fprintf(a.stdout, "  ✓ %s  %s\n", st.Name, st.Message)
		case agentv1.StepState_STEP_STATE_FAILED:
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

func phaseName(p agentv1.Phase) string {
	return strings.ToLower(strings.TrimPrefix(p.String(), "PHASE_"))
}

func kindName(k agentv1.EnvironmentKind) string {
	return strings.ToLower(strings.TrimPrefix(k.String(), "ENVIRONMENT_KIND_"))
}
