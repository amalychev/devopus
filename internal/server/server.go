// Package server is the workspace server (ADR-0016): the "control desk" that keeps
// workspaces, members, roles, permissions, invites, projects and the audit log. It
// runs no containers: agents do. One installation keeps many workspaces; every table
// carries workspace_id. Users come from Auth: the server checks their access tokens
// with Auth's public keys (JWKS) and decides permissions itself (ADR-0015).
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/amalychev/devopus/gen/devopus/sync/v1/syncv1connect"
	"github.com/amalychev/devopus/gen/devopus/workspace/v1/workspacev1connect"
	"github.com/amalychev/devopus/internal/auth/mail"
	"github.com/amalychev/devopus/internal/auth/token"
	"github.com/amalychev/devopus/internal/server/store"
)

// InviteTTL is how long an invite can be accepted.
const InviteTTL = 7 * 24 * time.Hour

// Config configures the server.
type Config struct {
	// Keys are Auth's public keys; AuthIssuer is the iss of its tokens.
	Keys       token.KeyFunc
	AuthIssuer string
	// AuthURL is where the web interface's sign-in goes (the server talks to Auth for
	// the browser).
	AuthURL string
	// PublicURL is the server's address as clients use it. It is the audience of the
	// access tokens it accepts: a token for another server — a self-hosted one run by
	// someone else, say — does not work here, and ours not there.
	PublicURL string
	// Mailer sends invites; the log when nil.
	Mailer mail.Mailer
	Log    *slog.Logger
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// Service is the workspace server.
type Service struct {
	workspacev1connect.UnimplementedWorkspaceServiceHandler

	cfg  Config
	pool *pgxpool.Pool
	q    *store.Queries

	seen sync.Map // user id → email already saved
	hub  hub      // wakes the agents' watchers of a workspace
}

// New opens the server on a migrated database.
func New(pool *pgxpool.Pool, cfg Config) (*Service, error) {
	if cfg.Keys == nil || cfg.AuthIssuer == "" {
		return nil, errors.New("server: Auth's keys and issuer are not set")
	}
	if cfg.PublicURL == "" {
		return nil, errors.New("server: the public URL is not set")
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Mailer == nil {
		cfg.Mailer = mail.Log{Logger: cfg.Log}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{cfg: cfg, pool: pool, q: store.New(pool)}, nil
}

func (s *Service) now() time.Time { return s.cfg.Now().UTC().Truncate(time.Microsecond) }

// Handler serves the API: every call goes through the access check.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	check := connect.WithInterceptors(s.accessInterceptor())
	mux.Handle(workspacev1connect.NewWorkspaceServiceHandler(s, check))
	mux.Handle(syncv1connect.NewSyncServiceHandler(&syncService{s: s}, check))
	s.webHandler(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.pool.Ping(r.Context()); err != nil {
			http.Error(w, "database: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// tx runs fn in a transaction. A committed change wakes the agents of the caller's
// workspace: they look at their desired state again.
func (s *Service) tx(ctx context.Context, fn func(q *store.Queries) error) error {
	if err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return fn(s.q.WithTx(tx)) }); err != nil {
		return err
	}
	if ws := callerOf(ctx).workspace; ws != "" {
		s.hub.wake(ws)
	}
	return nil
}

// audit records a change in the workspace's log.
func (s *Service) audit(ctx context.Context, q *store.Queries, ws, action, target, detail string) error {
	return q.AddAudit(ctx, store.AddAuditParams{
		WorkspaceID: ws, ActorID: callerOf(ctx).userID, Action: action, Target: target, Detail: detail, CreatedAt: s.now(),
	})
}

var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func newID(prefix string) string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	return prefix + "_" + idEncoding.EncodeToString(b)
}

func secret(prefix string) string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func hash(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func normalizeEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

func noRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

func internal(err error) error { return connect.NewError(connect.CodeInternal, err) }

func invalid(format string, a ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, errorf(format, a...))
}

func denied(format string, a ...any) error {
	return connect.NewError(connect.CodePermissionDenied, errorf(format, a...))
}

func notFound(format string, a ...any) error {
	return connect.NewError(connect.CodeNotFound, errorf(format, a...))
}

func errorf(format string, a ...any) error { return fmt.Errorf(format, a...) }
