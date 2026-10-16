package cli

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/amalychev/devopus/internal/auth/mail"
	"github.com/amalychev/devopus/internal/auth/token"
	"github.com/amalychev/devopus/internal/credentials"
	"github.com/amalychev/devopus/internal/server"
	"github.com/amalychev/devopus/internal/server/store"
)

// DefaultServerListen is where the workspace server listens in development; the CLI's
// default server URL (credentials.DefaultServerURL).
const DefaultServerListen = "127.0.0.1:7481"

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func (a *app) serverCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "The workspace server: workspaces, members, roles and projects",
		Long: "The workspace server is the team's control desk: it keeps workspaces, members, roles,\n" +
			"permissions, invites and projects, and runs no containers. Devopus runs it in the\n" +
			"cloud; self-hosted installs (server install, upgrade, backup) come later.",
	}
	var db, listen, authURL, issuer, mailSpec, from string
	run := &cobra.Command{
		Use:   "run",
		Short: "Run the workspace server in the foreground",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if db == "" {
				return fail(ExitInvalid, "the database is not set: --database-url or $DEVOPUS_SERVER_DATABASE_URL")
			}
			authURL = strings.TrimRight(authURL, "/")
			if issuer == "" {
				issuer = authURL
			}
			log := slog.New(slog.NewTextHandler(a.stderr, nil))
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			pool, err := pgxpool.New(ctx, db)
			if err != nil {
				return fail(ExitError, "%v", err)
			}
			defer pool.Close()
			if err := store.Migrate(ctx, pool); err != nil {
				return fail(ExitError, "%v", err)
			}
			mailer, err := mail.Parse(mailSpec, from, log)
			if err != nil {
				return fail(ExitInvalid, "%v", err)
			}
			keys := &token.Remote{URL: authURL + "/.well-known/jwks.json"}
			svc, err := server.New(pool, server.Config{Keys: keys.Key, AuthIssuer: issuer, Mailer: mailer, Log: log})
			if err != nil {
				return fail(ExitError, "%v", err)
			}
			ln, err := net.Listen("tcp", listen)
			if err != nil {
				return fail(ExitError, "%v", err)
			}
			srv := &http.Server{Handler: svc.Handler(), ReadHeaderTimeout: 10 * time.Second}
			go func() {
				<-ctx.Done()
				shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = srv.Shutdown(shutdown)
			}()
			log.Info("the workspace server is serving", "version", Version, "listen", ln.Addr().String(), "auth", authURL, "mail", mailSpec)
			if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				return fail(ExitError, "%v", err)
			}
			return nil
		},
	}
	f := run.Flags()
	f.StringVar(&db, "database-url", os.Getenv("DEVOPUS_SERVER_DATABASE_URL"), "PostgreSQL connection URL ($DEVOPUS_SERVER_DATABASE_URL)")
	f.StringVar(&listen, "listen", envOr("DEVOPUS_SERVER_LISTEN", DefaultServerListen), "address to listen on ($DEVOPUS_SERVER_LISTEN)")
	f.StringVar(&authURL, "auth-url", credentials.AuthURL(), "Auth, whose keys sign the users' tokens ($DEVOPUS_AUTH_URL)")
	f.StringVar(&issuer, "auth-issuer", os.Getenv("DEVOPUS_SERVER_AUTH_ISSUER"), "the iss of Auth's tokens (default: --auth-url)")
	f.StringVar(&mailSpec, "mail", envOr("DEVOPUS_SERVER_MAIL", "log"), "invite emails: \"log\" or smtp://user:password@host:587 ($DEVOPUS_SERVER_MAIL)")
	f.StringVar(&from, "mail-from", os.Getenv("DEVOPUS_SERVER_MAIL_FROM"), "sender address of the emails ($DEVOPUS_SERVER_MAIL_FROM)")
	cmd.AddCommand(run)
	return cmd
}
