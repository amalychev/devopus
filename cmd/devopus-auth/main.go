// devopus-auth is the Auth service (ADR-0014): accounts, sign-in and tokens. It is a
// deploy of its own in the Devopus cloud; in development it runs on the developer's
// machine with codes written to its log (docs/development.md).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/amalychev/devopus/internal/auth"
	"github.com/amalychev/devopus/internal/auth/mail"
	"github.com/amalychev/devopus/internal/auth/store"
)

// Version is set by the release build.
var Version = "dev"

// DefaultListen is where Auth listens in development; the CLI's default Auth URL.
const DefaultListen = "127.0.0.1:7480"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "devopus-auth: "+err.Error())
		os.Exit(1)
	}
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func run(args []string) error {
	if len(args) == 0 {
		args = []string{"help"}
	}
	fs := flag.NewFlagSet("devopus-auth "+args[0], flag.ContinueOnError)
	db := fs.String("database-url", env("DEVOPUS_AUTH_DATABASE_URL", ""), "PostgreSQL connection URL ($DEVOPUS_AUTH_DATABASE_URL)")
	listen := fs.String("listen", env("DEVOPUS_AUTH_LISTEN", DefaultListen), "address to listen on ($DEVOPUS_AUTH_LISTEN)")
	issuer := fs.String("issuer", env("DEVOPUS_AUTH_ISSUER", ""), "public URL of the service (default: http://<listen>) ($DEVOPUS_AUTH_ISSUER)")
	mailSpec := fs.String("mail", env("DEVOPUS_AUTH_MAIL", "log"), "\"log\" or smtp://user:password@host:587 ($DEVOPUS_AUTH_MAIL)")
	from := fs.String("mail-from", env("DEVOPUS_AUTH_MAIL_FROM", ""), "sender address of the emails ($DEVOPUS_AUTH_MAIL_FROM)")
	trustProxy := fs.Bool("trust-proxy", os.Getenv("DEVOPUS_AUTH_TRUST_PROXY") == "1", "take the client address from X-Forwarded-For ($DEVOPUS_AUTH_TRUST_PROXY=1)")

	switch args[0] {
	case "run", "migrate":
	case "version":
		fmt.Println(Version)
		return nil
	default:
		fmt.Fprintf(os.Stderr, `devopus-auth %s — the Devopus Auth service

Usage:
  devopus-auth run [flags]      migrate the database and serve
  devopus-auth migrate [flags]  migrate the database only
  devopus-auth version

Flags:
`, Version)
		fs.SetOutput(os.Stderr)
		fs.PrintDefaults()
		if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
			return nil
		}
		return fmt.Errorf("unknown command %q", args[0])
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *db == "" {
		return errors.New("the database is not set: --database-url or $DEVOPUS_AUTH_DATABASE_URL")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, *db)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool); err != nil {
		return err
	}
	if args[0] == "migrate" {
		log.Info("the database is up to date")
		return nil
	}

	if *issuer == "" {
		*issuer = "http://" + *listen
	}
	mailer, err := mail.Parse(*mailSpec, *from, log)
	if err != nil {
		return err
	}
	svc, err := auth.New(ctx, pool, auth.Config{Issuer: *issuer, Mailer: mailer, Log: log, TrustProxy: *trustProxy})
	if err != nil {
		return err
	}
	go svc.Run(ctx)

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: svc.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("devopus-auth is serving", "version", Version, "listen", ln.Addr().String(), "issuer", *issuer, "mail", mail.Redact(*mailSpec))
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
