// Package credentials keeps the user's sign-in on this device (ADR-0014): the refresh
// token in the system key store (Keychain, Secret Service; a 0600 file where there is
// none), the session's description in session.json, the access token only in the
// memory of the process that refreshed it. The CLI and the agent share the session:
// every refresh runs under a file lock and saves the rotated token, so neither ever
// presents a token the other already spent.
package credentials

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"

	authv1 "github.com/amalychev/devopus/gen/devopus/auth/v1"
	"github.com/amalychev/devopus/gen/devopus/auth/v1/authv1connect"
	"github.com/amalychev/devopus/internal/agent/paths"
)

// DefaultAuthURL is Auth in development (devopus-auth run). The cloud address comes
// once the product has its domain.
const DefaultAuthURL = "http://127.0.0.1:7480"

// AuthURL is the Auth service to sign in with: $DEVOPUS_AUTH_URL or DefaultAuthURL.
func AuthURL() string {
	if u := os.Getenv("DEVOPUS_AUTH_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return DefaultAuthURL
}

// Session describes the sign-in on this device; the refresh token is not in it.
type Session struct {
	AuthURL       string    `json:"authUrl"`
	UserID        string    `json:"userId"`
	Email         string    `json:"email"`
	SessionID     string    `json:"sessionId"`
	IdleExpiresAt time.Time `json:"idleExpiresAt"`
	ExpiresAt     time.Time `json:"expiresAt"`
	// Store is where the refresh token is: keychain, secret-service or file.
	Store string `json:"store"`
}

// Live reports whether the session has not run out by the clock. Auth may still have
// revoked it: a refresh finds that out.
func (s *Session) Live(now time.Time) bool {
	return now.Before(s.IdleExpiresAt) && now.Before(s.ExpiresAt)
}

// ErrSignedOut means there is no usable sign-in on this device.
var ErrSignedOut = errors.New("not signed in")

func sessionFile() string { return filepath.Join(paths.Home(), "session.json") }

// Load reads the session; nil when nobody is signed in.
func Load() (*Session, error) {
	b, err := os.ReadFile(sessionFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", sessionFile(), err)
	}
	return &s, nil
}

// Save keeps a new sign-in: the session and its refresh token.
func Save(s Session, refresh string) error {
	return withLock(func() error { return save(s, refresh) })
}

func save(s Session, refresh string) error {
	if s.Store == "" {
		s.Store = pickStore()
	}
	st, err := open(s.Store)
	if err != nil {
		return err
	}
	if err := st.set(paths.Home(), refresh); err != nil {
		if s.Store == StoreFile {
			return err
		}
		// The key store is there but refuses (a locked keychain over SSH): keep the file.
		s.Store = StoreFile
		if err := (file{}).set(paths.Home(), refresh); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(sessionFile(), append(b, '\n'))
}

// Clear forgets the sign-in on this device. It does not tell Auth: SignOut does.
func Clear() error {
	return withLock(forget)
}

func forget() error {
	s, err := Load()
	if err != nil || s == nil {
		_ = os.Remove(sessionFile())
		return err
	}
	if st, err := open(s.Store); err == nil {
		_ = st.del(paths.Home())
	}
	if err := os.Remove(sessionFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// StoredRefreshToken is the refresh token of the session on this device: what sign-out
// hands to Auth.
func StoredRefreshToken() (string, error) {
	s, err := Load()
	if err != nil {
		return "", err
	}
	if s == nil {
		return "", ErrSignedOut
	}
	return refreshToken(s)
}

// refreshToken reads the stored refresh token.
func refreshToken(s *Session) (string, error) {
	st, err := open(s.Store)
	if err != nil {
		return "", err
	}
	rt, err := st.get(paths.Home())
	if errors.Is(err, errNoSecret) {
		return "", ErrSignedOut
	}
	return rt, err
}

// Client is an Auth client.
func Client(authURL string) authv1connect.AuthServiceClient {
	return authv1connect.NewAuthServiceClient(&http.Client{Timeout: 30 * time.Second}, authURL)
}

// Access is a fresh access token.
type Access struct {
	Token     string
	ExpiresAt time.Time
	Session   Session
}

// Refresh trades the stored refresh token for new tokens and stores the rotated one.
// A session Auth no longer accepts is forgotten on the device: the error wraps
// ErrSignedOut with Auth's reason.
func Refresh(ctx context.Context) (Access, error) { return RefreshFor(ctx, "") }

// RefreshFor is Refresh with an access token for one workspace server: its audience
// is the server's address, and no other server accepts it. Empty: Auth's own calls.
func RefreshFor(ctx context.Context, audience string) (Access, error) {
	var a Access
	err := withLock(func() error {
		s, err := Load()
		if err != nil {
			return err
		}
		if s == nil {
			return ErrSignedOut
		}
		rt, err := refreshToken(s)
		if err != nil {
			return err
		}
		res, err := Client(s.AuthURL).Refresh(ctx, connect.NewRequest(&authv1.RefreshRequest{RefreshToken: rt, Audience: Audience(audience)}))
		if connect.CodeOf(err) == connect.CodeUnauthenticated {
			_ = forget()
			var ce *connect.Error
			errors.As(err, &ce)
			return fmt.Errorf("%w: %s", ErrSignedOut, ce.Message())
		}
		if err != nil {
			return fmt.Errorf("refresh the sign-in at %s: %w", s.AuthURL, err)
		}
		next := FromTokens(s.AuthURL, res.Msg.User, res.Msg.Tokens)
		next.Store = s.Store
		if err := save(next, res.Msg.Tokens.RefreshToken); err != nil {
			return err
		}
		a = Access{Token: res.Msg.Tokens.AccessToken, ExpiresAt: res.Msg.Tokens.AccessExpiresAt.AsTime(), Session: next}
		return nil
	})
	return a, err
}

// Audience is how a workspace server is named in tokens: its address without a
// trailing slash.
func Audience(serverURL string) string { return strings.TrimRight(serverURL, "/") }

// FromTokens describes the session tokens belong to.
func FromTokens(authURL string, u *authv1.User, t *authv1.Tokens) Session {
	return Session{
		AuthURL: authURL, UserID: u.GetId(), Email: u.GetEmail(), SessionID: t.GetSession().GetId(),
		IdleExpiresAt: t.GetSession().GetIdleExpiresAt().AsTime(), ExpiresAt: t.GetSession().GetExpiresAt().AsTime(),
	}
}

// withLock runs fn holding the sign-in lock of this data directory.
func withLock(fn func() error) error {
	if err := paths.Ensure(); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(paths.Home(), "session.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

// DeviceID is a random id of this device that Auth limits code requests by.
func DeviceID() string {
	p := filepath.Join(paths.Home(), "device-id")
	if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		return strings.TrimSpace(string(b))
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	_ = writeFile(p, []byte(id+"\n"))
	return id
}

// DeviceName is how sessions of this device are listed: its host name.
func DeviceName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown device"
	}
	return strings.TrimSuffix(h, ".local")
}
