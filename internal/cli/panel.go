package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	goruntime "runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/devopus/devopus/internal/agent/client"
	"github.com/devopus/devopus/internal/agent/paths"
	"github.com/devopus/devopus/internal/panel"
	"github.com/devopus/devopus/internal/service"
)

func (a *app) panelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "panel",
		Short: "Install, start, stop and open the local web panel",
		Long: "The local panel is a web app for this machine's environments: progress, logs, data,\n" +
			"features. It is a process of its own on 127.0.0.1 that talks to the agent; the agent\n" +
			"works without it. A remote panel for a team comes with the server (stage 2).",
	}
	var port int
	run := &cobra.Command{
		Use:   "run",
		Short: "Run the panel in the foreground (what the service runs)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			log := slog.New(slog.NewTextHandler(a.stderr, nil))
			err := panel.Run(ctx, panel.Options{Version: Version, Log: log, Port: port})
			if errors.Is(err, panel.ErrAlreadyRunning) {
				return fail(ExitError, "another panel is running for %s: %v", paths.Home(), err)
			}
			return err
		},
	}
	run.Flags().IntVar(&port, "port", 0, "port on 127.0.0.1 (default: the last one, or a free one)")

	cmd.AddCommand(
		a.panelOpenCmd(),
		a.panelInstallCmd(),
		&cobra.Command{
			Use:   "start",
			Short: "Start the panel: its service if installed, otherwise in the background",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				port, _, err := panel.Ensure(ctx, Version)
				if err != nil {
					return fail(ExitError, "%v", err)
				}
				return a.done(map[string]any{"url": panel.BaseURL(port)}, "The panel is running on %s; 'devopus panel open' signs you in.\n", panel.BaseURL(port))
			},
		},
		&cobra.Command{
			Use:   "stop",
			Short: "Stop the panel; the agent and the environments keep running",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := panel.Stop(ctx); err != nil {
					return fail(ExitError, "%v", err)
				}
				return a.done(map[string]any{"running": false}, "The panel is stopped.\n")
			},
		},
		&cobra.Command{
			Use:   "restart",
			Short: "Restart the panel",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := panel.Stop(ctx); err != nil {
					return fail(ExitError, "%v", err)
				}
				port, _, err := panel.Ensure(ctx, Version)
				if err != nil {
					return fail(ExitError, "%v", err)
				}
				return a.done(map[string]any{"url": panel.BaseURL(port)}, "The panel is running again on %s.\n", panel.BaseURL(port))
			},
		},
		&cobra.Command{
			Use:   "status",
			Short: "Whether the panel runs, its address, service and files",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				version, port, err := panel.Health(ctx)
				extra := map[string]string{"data": paths.Panel()}
				if err == nil {
					extra["url"] = panel.BaseURL(port)
				}
				return a.componentStatus(service.Panel, err == nil, version, panel.Pid(), paths.PanelLog(), extra)
			},
		},
		a.logFileCmd("Show the panel's log", paths.PanelLog),
		a.panelUninstallCmd(),
		run,
	)
	return cmd
}

func (a *app) panelInstallCmd() *cobra.Command {
	var yes bool
	var port int
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Run the local panel as a service that starts at login",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := service.For(service.Panel)
			if err != nil {
				return fail(ExitError, "%v", err)
			}
			file, err := s.File()
			if err != nil {
				return fail(ExitError, "%v", err)
			}
			if temporary(s.Exe) {
				return fail(ExitInvalid, "this devopus is a temporary build (%s); install it first and run 'devopus panel install' from there", s.Exe)
			}
			ok, err := a.confirm(yes, fmt.Sprintf("Run the Devopus panel as a service?\n  %s\n  It serves the panel on 127.0.0.1 at login; only a link from 'devopus panel open' signs in.", file))
			if err != nil {
				return err
			}
			if !ok {
				return fail(ExitError, "nothing changed")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := panel.Stop(ctx); err != nil {
				return fail(ExitError, "%v", err)
			}
			if port != 0 {
				if err := os.MkdirAll(paths.Panel(), 0o700); err != nil {
					return fail(ExitError, "%v", err)
				}
				if err := os.WriteFile(paths.PanelPort(), []byte(fmt.Sprintf("%d\n", port)), 0o600); err != nil {
					return fail(ExitError, "%v", err)
				}
			}
			if err := s.Install(); err != nil {
				return fail(ExitError, "%v", err)
			}
			got, err := panel.Wait(ctx, 15*time.Second)
			if err != nil {
				return fail(ExitError, "%v", err)
			}
			if port != 0 && got != port {
				a.progress("! port %d is taken, the panel uses %d", port, got)
			}
			return a.done(map[string]any{"service": file, "url": panel.BaseURL(got)},
				"✓ panel    running as a service on %s (%s)\n\nOpen it with 'devopus panel open'.\n", panel.BaseURL(got), file)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	cmd.Flags().IntVar(&port, "port", 0, "port on 127.0.0.1 (default: the last one, or a free one)")
	return cmd
}

func (a *app) panelUninstallCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop the panel, remove its service and its sign-in token",
		Long:  "Signed-in browsers lose access: the next install makes a new token. The agent is not touched.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ok, err := a.confirm(yes, "Remove the local panel (its service and token)? The agent and the environments stay.")
			if err != nil {
				return err
			}
			if !ok {
				return fail(ExitError, "nothing changed")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := a.removePanel(ctx); err != nil {
				return fail(ExitError, "%v", err)
			}
			return a.done(map[string]any{"removed": true}, "The panel is removed.\n")
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	return cmd
}

// removePanel stops the panel, removes its service and its files.
func (a *app) removePanel(ctx context.Context) error {
	s, err := service.For(service.Panel)
	if err != nil {
		return err
	}
	had := service.Installed(service.Panel)
	if err := s.Uninstall(); err != nil {
		return fmt.Errorf("remove the panel's service: %w", err)
	}
	if err := panel.Stop(ctx); err != nil {
		return err
	}
	if err := os.RemoveAll(paths.Panel()); err != nil {
		return err
	}
	if had {
		a.progress("✓ panel    stopped, service removed")
	} else {
		a.progress("✓ panel    stopped")
	}
	return nil
}

func (a *app) panelOpenCmd() *cobra.Command {
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "open",
		Short: "Open the local panel in the browser, signed in (starts it if needed)",
		Long: "Opens the panel signed in. The link carries a token that only works on this device;\n" +
			"the panel listens on 127.0.0.1 only. Starts the panel if it is not running.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			port, started, err := panel.Ensure(ctx, Version)
			if err != nil {
				return fail(ExitError, "%v", err)
			}
			if started && !a.json && !service.Installed(service.Panel) {
				fmt.Fprintf(a.stderr, "Started the panel in the background (log: %s); 'devopus panel install' runs it as a service\n", paths.PanelLog())
			}
			if err := client.Ping(ctx, client.New(paths.Socket())); err != nil && !a.json {
				fmt.Fprintln(a.stderr, "The agent is not running, the panel will wait for it: 'devopus agent start'")
			}
			link, err := panel.SignInURL(port)
			if err != nil {
				return fail(ExitError, "%v", err)
			}
			base := panel.BaseURL(port)
			if a.json {
				return a.writeJSON(map[string]string{"url": base, "link": link})
			}
			fmt.Fprintln(a.stdout, base)
			if noOpen {
				fmt.Fprintf(a.stdout, "Sign in with: %s\n", link)
				return nil
			}
			return openBrowser(link)
		},
	}
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "print the sign-in link instead of opening the browser")
	return cmd
}

// panelLine describes the local panel for `devopus status`.
func panelLine(ctx context.Context) string {
	_, port, err := panel.Health(ctx)
	if err != nil {
		return "not running ('devopus panel open' starts it)"
	}
	return panel.BaseURL(port) + " ('devopus panel open' signs you in)"
}

func openBrowser(url string) error {
	opener := "xdg-open"
	if goruntime.GOOS == "darwin" {
		opener = "open"
	}
	if err := exec.Command(opener, url).Run(); err != nil {
		return fail(ExitError, "%s: %v; open %s yourself", opener, err, url)
	}
	return nil
}
