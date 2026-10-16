// Package auth is the Auth service (ADR-0014, docs/architecture/auth.md): accounts,
// sign-in by email and one-time code, the browser (OAuth 2.0 + PKCE) and device
// (RFC 8628) flows, access and refresh tokens, sessions, JWKS and the registry of a
// user's workspaces. Roles and permissions are not here: workspace servers check them.
package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/amalychev/devopus/gen/devopus/auth/v1/authv1connect"
	"github.com/amalychev/devopus/internal/auth/mail"
	"github.com/amalychev/devopus/internal/auth/store"
	"github.com/amalychev/devopus/internal/auth/token"
)

// Lifetimes and limits of docs/architecture/auth.md.
const (
	AccessTTL      = 15 * time.Minute
	SessionIdle    = 30 * 24 * time.Hour // a session not refreshed for this long ends
	SessionMax     = 90 * 24 * time.Hour // and any session ends after this
	CodeTTL        = 10 * time.Minute    // one-time codes in emails
	CodeAttempts   = 5
	AuthCodeTTL    = time.Minute // authorization codes of the browser sign-in
	DeviceCodeTTL  = 10 * time.Minute
	DeviceInterval = 5 * time.Second
	KeyRotation    = 30 * 24 * time.Hour // a new signing key after this

	// Codes sent per hour.
	LimitPerEmail  = 5
	LimitPerIP     = 30
	LimitPerDevice = 10
)

// ClientID is the only OAuth client: the devopus CLI.
const ClientID = "devopus-cli"

// Config configures the service.
type Config struct {
	// Issuer is the public URL of the service: the iss of tokens and the base of the
	// sign-in pages.
	Issuer string
	Mailer mail.Mailer
	Log    *slog.Logger
	// TrustProxy takes the client address from X-Forwarded-For: only behind a proxy
	// that sets it.
	TrustProxy bool
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// Service is the Auth service.
type Service struct {
	authv1connect.UnimplementedAuthServiceHandler

	cfg  Config
	pool *pgxpool.Pool
	q    *store.Queries

	mu   sync.Mutex
	keys []store.SigningKey // newest first
}

// New opens the service on a migrated database and makes sure it has a signing key.
func New(ctx context.Context, pool *pgxpool.Pool, cfg Config) (*Service, error) {
	if cfg.Issuer == "" {
		return nil, errors.New("auth: the issuer URL is not set")
	}
	cfg.Issuer = strings.TrimRight(cfg.Issuer, "/")
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Mailer == nil {
		cfg.Mailer = mail.Log{Logger: cfg.Log}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &Service{cfg: cfg, pool: pool, q: store.New(pool)}
	if err := s.RotateKeys(ctx, false); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Service) now() time.Time { return s.cfg.Now().UTC().Truncate(time.Microsecond) }

// Run does the background work until ctx ends: picks up keys other instances made,
// rotates the signing key, deletes what expired.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := s.RotateKeys(ctx, false); err != nil {
			s.cfg.Log.Error("signing keys", "err", err)
		}
		if i%60 == 0 {
			if err := s.q.DeleteExpired(ctx, s.now()); err != nil {
				s.cfg.Log.Error("delete expired", "err", err)
			}
		}
	}
}

// Handler serves the RPCs, the sign-in pages and JWKS.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle(authv1connect.NewAuthServiceHandler(s))
	mux.HandleFunc("GET /.well-known/jwks.json", s.serveJWKS)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.pool.Ping(r.Context()); err != nil {
			http.Error(w, "database: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /authorize", s.authorizePage)
	mux.HandleFunc("POST /authorize", s.authorizePage)
	mux.HandleFunc("GET /device", s.devicePage)
	mux.HandleFunc("POST /device", s.devicePage)
	return mux
}

// tx runs fn in a transaction. fn's error rolls it back unless it is a *committed:
// the changes stay and the wrapped error is returned.
func (s *Service) tx(ctx context.Context, fn func(q *store.Queries) error) error {
	var keep *committed
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := fn(s.q.WithTx(tx))
		if errors.As(err, &keep) {
			return nil
		}
		return err
	})
	if err != nil {
		return err
	}
	if keep != nil {
		return keep.err
	}
	return nil
}

// committed is an error that keeps the transaction's changes: a failed code attempt
// is counted, a reused refresh token revokes its session.
type committed struct{ err error }

func (c *committed) Error() string { return c.err.Error() }

func commit(err error) error { return &committed{err: err} }

// --- keys and tokens

// RotateKeys loads the signing keys and makes a new one when there is none, when the
// newest is older than KeyRotation, or when force is set.
func (s *Service) RotateKeys(ctx context.Context, force bool) error {
	keys, err := s.q.SigningKeys(ctx)
	if err != nil {
		return err
	}
	if force || len(keys) == 0 || s.now().Sub(keys[0].CreatedAt) > KeyRotation {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		k := store.SigningKey{ID: newID("key"), PrivateKey: priv.Seed(), CreatedAt: s.now()}
		if err := s.q.CreateSigningKey(ctx, store.CreateSigningKeyParams(k)); err != nil {
			return err
		}
		keys = append([]store.SigningKey{k}, keys...)
	}
	s.mu.Lock()
	s.keys = keys
	s.mu.Unlock()
	return nil
}

// published are the keys JWKS lists: the newest, and older ones while tokens they
// signed can still be valid.
func (s *Service) published() []token.Key {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []token.Key
	for i, k := range s.keys {
		// A key signs until the next one appears; its tokens live AccessTTL longer.
		if i > 0 && s.now().Sub(s.keys[i-1].CreatedAt) > 2*AccessTTL {
			break
		}
		out = append(out, token.Key{ID: k.ID, Private: ed25519.NewKeyFromSeed(k.PrivateKey)})
	}
	return out
}

func (s *Service) serveJWKS(w http.ResponseWriter, _ *http.Request) {
	var set token.JWKS
	for _, k := range s.published() {
		set.Keys = append(set.Keys, token.PublicJWK(k))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_ = jsonEncode(w, set)
}

// JWKS is the published key set, for tests and servers in the same process.
func (s *Service) JWKS() token.JWKS {
	var set token.JWKS
	for _, k := range s.published() {
		set.Keys = append(set.Keys, token.PublicJWK(k))
	}
	return set
}

func (s *Service) signAccess(user store.User, sessionID, audience string) (string, time.Time, error) {
	keys := s.published()
	if len(keys) == 0 {
		return "", time.Time{}, errors.New("no signing key")
	}
	if audience == "" {
		audience = token.DefaultAudience
	}
	now := s.now()
	exp := now.Add(AccessTTL)
	tok, err := token.Sign(keys[0], token.Claims{
		Issuer: s.cfg.Issuer, Subject: user.ID, Session: sessionID, Email: user.Email, Audience: audience,
		IssuedAt: now.Unix(), Expiry: exp.Unix(),
	})
	return tok, exp.Truncate(time.Second), err
}

// Verify checks an access token Auth issued for Devopus itself.
func (s *Service) Verify(access string) (token.Claims, error) {
	set := s.JWKS()
	return token.Verify(access, set.Find, token.Expect{Issuer: s.cfg.Issuer, Audience: token.DefaultAudience, Now: s.cfg.Now()})
}

// --- helpers

var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// newID makes a random id with a prefix: usr_…, ses_…
func newID(prefix string) string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	return prefix + "_" + idEncoding.EncodeToString(b)
}

// secret makes a random token with a prefix, which secret scanners recognize.
func secret(prefix string) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func hash(parts ...string) []byte {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write([]byte(p))
	}
	return h.Sum(nil)
}

// otp makes a 6-digit code.
func otp() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1_000_000))
	return fmt.Sprintf("%06d", n.Int64())
}

// userCodeAlphabet has no vowels (no words) and no look-alike characters (RFC 8628 §6.1).
const userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"

func userCode() string {
	b := make([]byte, 8)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(userCodeAlphabet))))
		b[i] = userCodeAlphabet[n.Int64()]
	}
	return string(b[:4]) + "-" + string(b[4:])
}

// normalizeUserCode accepts the user code typed in any case, with or without the dash.
func normalizeUserCode(c string) string {
	c = strings.ToUpper(strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		return r
	}, c))
	if len(c) != 8 {
		return c
	}
	return c[:4] + "-" + c[4:]
}

// clientIP is the caller's address: the peer, or the first X-Forwarded-For entry
// behind a trusted proxy.
func (s *Service) clientIP(peer string, h http.Header) string {
	if s.cfg.TrustProxy {
		if f := h.Get("X-Forwarded-For"); f != "" {
			return strings.TrimSpace(strings.Split(f, ",")[0])
		}
	}
	if host, _, err := net.SplitHostPort(peer); err == nil {
		return host
	}
	return peer
}

func noRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

func internal(err error) error {
	return connect.NewError(connect.CodeInternal, err)
}
