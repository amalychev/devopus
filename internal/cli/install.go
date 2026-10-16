package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	agentv2 "github.com/amalychev/devopus/gen/devopus/agent/v2"
	"github.com/amalychev/devopus/internal/agent/client"
	"github.com/amalychev/devopus/internal/agent/paths"
	"github.com/amalychev/devopus/internal/agent/pki"
	"github.com/amalychev/devopus/internal/agent/runtime"
	"github.com/amalychev/devopus/internal/agent/store"
	"github.com/amalychev/devopus/internal/panel"
	"github.com/amalychev/devopus/internal/service"
	"github.com/amalychev/devopus/internal/trust"
)

func (a *app) agentInstallCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Run the agent as a service on this machine and set the machine up",
		Long: "Registers the agent as a per-user service (a launchd agent on macOS, a systemd user unit\n" +
			"on Linux) that starts at login, then checks the machine and offers to trust the local CA.\n" +
			"Every change asks first. Run it again after moving the binary. The web panel is separate:\n" +
			"'devopus panel install'.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := service.For(service.Agent)
			if err != nil {
				return fail(ExitError, "%v", err)
			}
			file, err := s.File()
			if err != nil {
				return fail(ExitError, "%v", err)
			}
			if temporary(s.Exe) {
				return fail(ExitInvalid, "this devopus is a temporary build (%s); install it first (brew install amalychev/tap/devopus, or the install script) and run 'devopus agent install' from there", s.Exe)
			}
			ok, err := a.confirm(yes, fmt.Sprintf("Run the Devopus agent as a service?\n  %s\n  It runs %s at login and restarts it after a crash.", file, s.Exe))
			if err != nil {
				return err
			}
			if !ok {
				return fail(ExitError, "nothing changed")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			// An agent started in the background, or by an older service, gives way.
			if err := client.Stop(ctx); err != nil {
				return fail(ExitError, "stop the running agent: %v", err)
			}
			if err := s.Install(); err != nil {
				return fail(ExitError, "%v", err)
			}
			if err := client.Wait(ctx, 15*time.Second); err != nil {
				return fail(ExitError, "%v", err)
			}
			if !a.json {
				fmt.Fprintf(a.stdout, "✓ agent    running as a service (%s)\n", file)
			}
			checks := a.preflight(ctx)
			if checks, err = a.applyFixes(ctx, checks, yes); err != nil {
				return err
			}
			failed := false
			for _, c := range checks {
				failed = failed || c.Status == "fail"
			}
			if a.json {
				if err := a.writeJSON(map[string]any{"service": file, "checks": checks, "ok": !failed}); err != nil {
					return err
				}
			} else {
				a.printChecks(checks)
				fmt.Fprintln(a.stdout, "\nNext: in a project, 'devopus init' (imports docker-compose.yml) and 'devopus up'.\n"+
					"Web panel: 'devopus panel install' runs it as a service, 'devopus panel open' opens it.")
			}
			if failed {
				return silent(ExitPreflightErr)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	return cmd
}

// temporary tells a `go run` or test binary from an installed one: a service must
// not point at a file the Go build cache deletes.
func temporary(exe string) bool {
	return strings.Contains(exe, string(filepath.Separator)+"go-build") || strings.HasPrefix(exe, os.TempDir())
}

func (a *app) agentUninstallCmd() *cobra.Command {
	var purge, yes bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop the environments, remove the agent's service and the trust in the local CA",
		Long: "Stops every environment (their data stays), stops the agent, removes its service and\n" +
			"takes the local CA out of the keychain. --purge also removes the environments with\n" +
			"their data and the agent's directory, the local panel included; worktrees with work that\n" +
			"exists nowhere else are kept.\n" +
			"The devopus binary itself stays: remove it with the tool that installed it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := service.For(service.Agent)
			if err != nil {
				return fail(ExitError, "%v", err)
			}
			file, _ := s.File()
			plan := []string{"stop every environment; their data stays"}
			if purge {
				plan[0] = "remove every environment with its data (containers, volumes, worktrees without unsaved work)"
			}
			plan = append(plan, "stop the agent and remove the service "+file, "take the local CA out of the login keychain")
			if purge {
				plan = append(plan, "stop the local panel and remove its service, if any", "delete "+paths.Home())
			}
			ok, err := a.confirm(yes, "Uninstall the Devopus agent from this machine?\n  - "+strings.Join(plan, "\n  - "))
			if err != nil {
				return err
			}
			if !ok {
				return fail(ExitError, "nothing changed")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			report := map[string]any{}

			if _, err := os.Stat(paths.State()); err == nil {
				n, err := a.stopEnvironments(ctx, purge)
				if err != nil {
					return err
				}
				report["environments"] = n
			}
			// The service first, so nothing restarts the agent; then an agent started
			// in the background, if that is what runs.
			had := service.Installed(service.Agent)
			if err := s.Uninstall(); err != nil {
				return fail(ExitError, "remove the service: %v", err)
			}
			if err := client.Stop(ctx); err != nil {
				return fail(ExitError, "stop the agent: %v", err)
			}
			if had {
				report["service"] = "removed"
				a.progress("✓ agent    stopped, service removed")
			} else {
				a.progress("✓ agent    stopped")
			}
			if n, err := stopContainers(ctx, purge); err != nil {
				a.progress("! proxy    %v", err)
			} else if n > 0 {
				verb := "stopped"
				if purge {
					verb = "removed"
				}
				a.progress("✓ proxy    %d containers %s", n, verb)
			}

			if ca, err := pki.LoadCA(paths.CA()); err == nil {
				switch removed, err := trust.Remove(ca.RootPath()); {
				case err != nil:
					a.progress("! ca       %v", err)
					report["ca"] = err.Error()
				case removed:
					a.progress("✓ ca       taken out of the keychain")
					report["ca"] = "removed"
				default:
					report["ca"] = "not trusted"
				}
			}

			if purge {
				// The panel's token and port live in the directory that goes away.
				if err := a.removePanel(ctx); err != nil {
					a.progress("! panel    %v", err)
				}
				kept, err := purgeHome()
				if err != nil {
					return fail(ExitError, "%v", err)
				}
				report["kept"] = kept
				if kept != "" {
					a.progress("! data     kept %s: worktrees with unsaved work; delete it yourself when you are done", kept)
				} else {
					a.progress("✓ data     %s deleted", paths.Home())
				}
			}
			if a.json {
				return a.writeJSON(report)
			}
			if !purge && (service.Installed(service.Panel) || panel.Pid() != 0) {
				fmt.Fprintln(a.stdout, "\nThe local panel stays: 'devopus panel uninstall' removes it.")
			}
			fmt.Fprintln(a.stdout, "\nThe agent is uninstalled. Remove the binary with the tool that installed it (brew uninstall devopus, or delete "+s.Exe+").")
			return nil
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "also remove the environments with their data and the agent's directory")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	return cmd
}

func (a *app) progress(format string, args ...any) {
	if !a.json {
		fmt.Fprintf(a.stdout, format+"\n", args...)
	}
}

// stopEnvironments stops (or, with purge, removes) the environments of every project
// and waits until the agent is done. It returns how many there were.
func (a *app) stopEnvironments(ctx context.Context, purge bool) (int, error) {
	c, err := a.connect(ctx)
	if err != nil {
		return 0, err
	}
	// The desired state, not the agent's statuses: an agent that has just started
	// has not reported any environment yet.
	desired, err := store.Open(paths.State())
	if err != nil {
		return 0, fail(ExitError, "%v", err)
	}
	st := desired.Snapshot()
	if len(st.Projects) == 0 {
		return 0, nil
	}
	var ids []string
	for _, e := range st.Environments {
		ids = append(ids, e.Id)
	}
	w, err := watch(ctx, c)
	if err != nil {
		return 0, err
	}
	defer w.Close()
	var rev uint64
	for _, p := range st.Projects {
		res, err := c.Down(ctx, connect.NewRequest(&agentv2.DownRequest{Project: p.Name, Purge: purge}))
		if err != nil {
			return 0, rpcError(err)
		}
		rev = res.Msg.Revision
	}
	if _, err := a.follow(w, ids, rev, false, func(s *agentv2.EnvironmentStatus) bool {
		if purge {
			return s.Removed
		}
		return s.Phase == agentv2.Phase_PHASE_STOPPED || s.Phase == agentv2.Phase_PHASE_ERROR
	}); err != nil {
		return 0, err
	}
	verb := "stopped"
	if purge {
		verb = "removed"
	}
	a.progress("✓ envs     %d %s", len(ids), verb)
	return len(ids), nil
}

// stopContainers stops what the agent leaves running without environments: the proxy
// on 80/443 and its DNS. With purge it removes every container of this agent.
func stopContainers(ctx context.Context, purge bool) (int, error) {
	rt, err := runtime.New(runtime.Detect())
	if err != nil {
		return 0, err
	}
	defer rt.Close()
	ctrs, err := rt.Containers(ctx, paths.Instance())
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range ctrs {
		switch {
		case purge:
			err = rt.RemoveContainer(ctx, c.ID)
		case c.Running:
			err = rt.StopContainer(ctx, c.ID)
		default:
			continue
		}
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// purgeHome deletes the agent's directory. Checkouts the agent kept because they hold
// work that exists nowhere else stay, and so does src/ around them; kept names it.
func purgeHome() (kept string, err error) {
	home := paths.Home()
	entries, err := os.ReadDir(home)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		p := filepath.Join(home, e.Name())
		if p == paths.Sources() {
			if empty(p) {
				continue
			}
			kept = p
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			return kept, err
		}
	}
	if kept == "" {
		return "", os.RemoveAll(home)
	}
	return kept, nil
}

// empty reports whether dir has no files, ignoring empty directories.
func empty(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return !found
}
