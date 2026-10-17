package credentials

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Keeper keeps a fresh access token for a long-running process: the agent works on
// behalf of the user signed in on this device and refreshes the tokens itself. It
// picks up a new sign-in (devopus login) and a sign-out (devopus logout) on its own.
type Keeper struct {
	Log *slog.Logger

	mu     sync.Mutex
	access Access            // for Auth: keeps the session alive
	others map[string]Access // by audience: for workspace servers
	quiet  bool              // "signed out" is logged once
}

// margin is how long before expiry an access token is replaced.
const margin = 2 * time.Minute

// Token is a valid access token, refreshed when needed.
func (k *Keeper) Token(ctx context.Context) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.ensure(ctx); err != nil {
		return "", err
	}
	return k.access.Token, nil
}

// TokenFor is a valid access token for a workspace server (credentials.Audience).
func (k *Keeper) TokenFor(ctx context.Context, audience string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.ensure(ctx); err != nil {
		return "", err
	}
	a, ok := k.others[audience]
	if ok && a.Session.SessionID == k.access.Session.SessionID && time.Until(a.ExpiresAt) > margin {
		return a.Token, nil
	}
	a, err := RefreshFor(ctx, audience)
	if err != nil {
		return "", err
	}
	if k.others == nil {
		k.others = map[string]Access{}
	}
	k.others[audience] = a
	return a.Token, nil
}

// Session is the sign-in the keeper works with; nil when nobody is signed in.
func (k *Keeper) Session() *Session {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.access.Token == "" {
		return nil
	}
	s := k.access.Session
	return &s
}

// ensure refreshes unless the token in memory belongs to the stored session and is
// not about to expire.
func (k *Keeper) ensure(ctx context.Context) error {
	s, err := Load()
	if err != nil {
		return err
	}
	if s == nil || !s.Live(time.Now()) {
		k.access = Access{}
		return ErrSignedOut
	}
	if k.access.Token != "" && k.access.Session.SessionID == s.SessionID && time.Until(k.access.ExpiresAt) > margin {
		return nil
	}
	a, err := Refresh(ctx)
	if err != nil {
		k.access = Access{}
		return err
	}
	k.access = a
	return nil
}

// Run keeps the token fresh until ctx ends.
func (k *Keeper) Run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		k.mu.Lock()
		err := k.ensure(ctx)
		k.mu.Unlock()
		switch {
		case err == nil:
			k.quiet = false
		case errors.Is(err, ErrSignedOut):
			if !k.quiet && k.Log != nil {
				k.Log.Info("nobody is signed in on this device: run 'devopus login'", "reason", err)
			}
			k.quiet = true
		default:
			if k.Log != nil {
				k.Log.Warn("refresh the sign-in", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
