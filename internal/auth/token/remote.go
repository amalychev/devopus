package token

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Remote is a key set fetched from Auth's JWKS URL and cached. An unknown key id
// refetches the set, at most once a minute: that is how new keys after a rotation
// are picked up.
type Remote struct {
	URL    string
	Client *http.Client

	mu      sync.Mutex
	set     JWKS
	fetched time.Time
}

// Key is a KeyFunc for Verify.
func (r *Remote) Key(kid string) (ed25519.PublicKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if k, err := r.set.Find(kid); err == nil {
		return k, nil
	}
	if time.Since(r.fetched) < time.Minute {
		return nil, fmt.Errorf("%w: unknown key %q", ErrSignature, kid)
	}
	if err := r.fetch(); err != nil {
		return nil, err
	}
	return r.set.Find(kid)
}

func (r *Remote) fetch() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return err
	}
	c := r.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("fetch keys: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch keys: %s", resp.Status)
	}
	var set JWKS
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return fmt.Errorf("fetch keys: %w", err)
	}
	if len(set.Keys) == 0 {
		return errors.New("fetch keys: the key set is empty")
	}
	r.set, r.fetched = set, time.Now()
	return nil
}
