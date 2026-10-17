package cli

import (
	"context"
	"fmt"
	"os"
	goruntime "runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/amalychev/devopus/internal/credentials"
	"github.com/amalychev/devopus/internal/selfhost"
)

// The self-hosted workspace server (stage 4): the same program on the customer's
// own Ubuntu server. These commands run on that server as root; they need no Devopus
// sign-in there.

// selfhostHost is the server's host: the real one, or one that only prints.
func (a *app) selfhostHost(dry bool) (selfhost.Host, error) {
	if dry {
		return selfhost.DryRun{Out: a.stdout}, nil
	}
	if goruntime.GOOS != "linux" {
		return nil, fail(ExitInvalid, "the self-hosted workspace server installs on Linux (Ubuntu 24.04+); --dry-run shows what it does")
	}
	if os.Geteuid() != 0 {
		return nil, fail(ExitInvalid, "run it as root: sudo devopus server …")
	}
	return selfhost.System{}, nil
}

func (a *app) serverInstallCmd() *cobra.Command {
	var o selfhost.Options
	var dry bool
	cmd := &cobra.Command{
		Use:   "install --domain <domain>",
		Short: "Install the workspace server on this machine (Ubuntu with Docker), or update its settings",
		Long: "Installs the workspace server here: PostgreSQL in a container with a volume, the server\n" +
			"as the systemd service devopus-server, HTTPS on the domain with Let's Encrypt. The\n" +
			"domain's DNS must point here, ports 80 and 443 be open. Accounts stay with Devopus\n" +
			"(Auth): people sign in as usual, the workspace's data is here. Run again to change\n" +
			"the settings; the data stays.",
		Example: "  sudo devopus server install --domain ws.acme.com --mail smtp://user:pass@smtp.acme.com:587 --mail-from devopus@acme.com\n" +
			"  devopus server install --domain ws.acme.com --dry-run",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			h, err := a.selfhostHost(dry)
			if err != nil {
				return err
			}
			if o.Binary, err = os.Executable(); err != nil {
				return err
			}
			o.Log = a.stderr
			if err := selfhost.Install(cmd.Context(), h, o); err != nil {
				return fail(ExitError, "%v", err)
			}
			url := "https://" + o.Domain
			if dry {
				return nil
			}
			fmt.Fprintf(a.stderr, "Waiting for %s (the first request gets the certificate)…\n", url)
			herr := waitHealth(cmd.Context(), url)
			if herr != nil {
				fmt.Fprintf(a.stderr, "%s does not answer yet: %v\nCheck that its DNS points here and ports 80 and 443 are open; 'devopus server status' and 'journalctl -u %s' tell more.\n", url, herr, selfhost.Service)
			}
			return a.done(map[string]any{"url": url, "healthy": herr == nil},
				"✓ The workspace server runs at %s.\nOn your machine: devopus workspace create <name> --server %s\nBack it up: sudo devopus server backup\n", url, url)
		},
	}
	cmd.Flags().StringVar(&o.Domain, "domain", "", "the domain the server answers on, with HTTPS")
	cmd.Flags().StringVar(&o.AuthURL, "auth-url", credentials.AuthURL(), "Devopus Auth ($DEVOPUS_AUTH_URL)")
	cmd.Flags().StringVar(&o.Mail, "mail", "log", "invite emails: \"log\" or smtp://user:password@host:587")
	cmd.Flags().StringVar(&o.MailFrom, "mail-from", "", "sender address of the invite emails")
	cmd.Flags().BoolVar(&dry, "dry-run", false, "print what it would do; change nothing")
	return cmd
}

func waitHealth(ctx context.Context, url string) error {
	var err error
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
		if err = selfhost.Health(ctx, url); err == nil {
			return nil
		}
	}
	return err
}

func (a *app) serverStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Whether the workspace server installed here runs, and its database",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st := selfhost.ReadStatus(cmd.Context(), selfhost.System{})
			if !st.Installed {
				return a.done(map[string]any{"installed": false}, "No workspace server is installed here: sudo devopus server install --domain <domain>\n")
			}
			url := "https://" + st.Domain
			health := "answers"
			if err := selfhost.Health(cmd.Context(), url); err != nil {
				health = err.Error()
			}
			return a.done(map[string]any{"installed": true, "url": url, "service": st.Service, "database": st.Database, "health": health},
				"Server    %s\nService   %s (systemctl status %s)\nDatabase  %s (%s)\nHealth    %s\n", url, st.Service, selfhost.Service, st.Database, selfhost.PGContainer, health)
		},
	}
}

func (a *app) serverBackupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "backup",
		Short: "Dump the workspace server's database into " + selfhost.BackupDir,
		Long: "Dumps the database: workspaces, members, projects with every version, ciphertexts of\n" +
			"secrets. Secrets stay encrypted in it: the workspaces' keys are on their members'\n" +
			"devices. Copy the file somewhere safe; 'devopus server restore <file>' loads it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			h, err := a.selfhostHost(false)
			if err != nil {
				return err
			}
			file, err := selfhost.Backup(cmd.Context(), h, time.Now())
			if err != nil {
				return fail(ExitError, "%v", err)
			}
			return a.done(map[string]any{"file": file}, "Backed up to %s\n", file)
		},
	}
}

func (a *app) serverRestoreCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "restore <file>",
		Short: "Load a backup into the workspace server's database; what is there now is replaced",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			h, err := a.selfhostHost(false)
			if err != nil {
				return err
			}
			ok, err := a.confirm(yes, fmt.Sprintf("Replace the workspace server's data with %s? The server stops meanwhile.", args[0]))
			if err != nil || !ok {
				return err
			}
			if err := selfhost.Restore(cmd.Context(), h, args[0]); err != nil {
				return fail(ExitError, "%v", err)
			}
			return a.done(map[string]any{"restored": args[0]}, "Restored from %s.\n", args[0])
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	return cmd
}

func (a *app) serverUpgradeCmd() *cobra.Command {
	var version string
	cmd := &cobra.Command{
		Use:   "upgrade [--version v1.2.3]",
		Short: "Back up, put the latest devopus here and restart the workspace server",
		Long: "Backs the database up, downloads a release of devopus (the latest by default) checked\n" +
			"against its checksums, puts it in place of this binary and restarts the service; the\n" +
			"server brings its database schema up to date as it starts.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			h, err := a.selfhostHost(false)
			if err != nil {
				return err
			}
			st := selfhost.ReadStatus(ctx, h)
			if !st.Installed {
				return fail(ExitInvalid, "no workspace server is installed here")
			}
			file, err := selfhost.Backup(ctx, h, time.Now())
			if err != nil {
				return fail(ExitError, "back up first: %v", err)
			}
			fmt.Fprintf(a.stderr, "Backed up to %s\n", file)
			bin, err := selfhost.Download(ctx, selfhost.ReleaseURL(version))
			if err != nil {
				return fail(ExitError, "%v", err)
			}
			self, err := os.Executable()
			if err != nil {
				return err
			}
			if err := selfhost.Replace(self, bin); err != nil {
				return fail(ExitError, "%v", err)
			}
			if _, err := h.Run(ctx, "systemctl", "restart", selfhost.Service); err != nil {
				return fail(ExitError, "%v", err)
			}
			url := "https://" + st.Domain
			if err := waitHealth(ctx, url); err != nil {
				return fail(ExitError, "%s does not answer after the upgrade: %v; the backup is %s", url, err, file)
			}
			return a.done(map[string]any{"backup": file, "url": url}, "✓ Upgraded; %s answers. The backup from before: %s\n", url, file)
		},
	}
	cmd.Flags().StringVar(&version, "version", "latest", "the release")
	return cmd
}

func (a *app) serverUninstallCmd() *cobra.Command {
	var purge, yes, dry bool
	cmd := &cobra.Command{
		Use:   "uninstall [--purge]",
		Short: "Remove the workspace server's service; --purge also its database with all data",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			h, err := a.selfhostHost(dry)
			if err != nil {
				return err
			}
			q := "Remove the workspace server's service? The database stays."
			if purge {
				q = "Remove the workspace server with its database: every workspace, member and project here? Back it up first."
			}
			if !dry {
				ok, err := a.confirm(yes, q)
				if err != nil || !ok {
					return err
				}
			}
			if err := selfhost.Uninstall(cmd.Context(), h, purge); err != nil {
				return fail(ExitError, "%v", err)
			}
			return a.done(map[string]any{"purged": purge}, "The workspace server is removed.\n")
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "remove the database and its data too")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	cmd.Flags().BoolVar(&dry, "dry-run", false, "print what it would do; change nothing")
	return cmd
}
