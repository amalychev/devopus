package credentials

import (
	"errors"
	"sync"

	"filippo.io/age"

	"github.com/amalychev/devopus/internal/agent/paths"
	"github.com/amalychev/devopus/internal/secrets"
)

// deviceKeySuffix names the device key's item in the key store: the account is the
// data directory, as for the refresh token.
const deviceKeySuffix = "#device-key"

var deviceKeyMu sync.Mutex

// DeviceKey is this device's key pair for secrets (ADR-0009): made on first use and
// kept in the system key store, like the refresh token. The workspace's key reaches
// the device wrapped to its public half; the private half never leaves the device.
func DeviceKey() (*age.X25519Identity, error) {
	deviceKeyMu.Lock()
	defer deviceKeyMu.Unlock()
	var key *age.X25519Identity
	err := withLock(func() error {
		store := pickStore()
		if s, err := Load(); err == nil && s != nil && s.Store != "" {
			store = s.Store
		}
		st, err := open(store)
		if err != nil {
			return err
		}
		account := paths.Home() + deviceKeySuffix
		raw, err := st.get(account)
		if errors.Is(err, errNoSecret) && store != StoreFile {
			raw, err = (file{}).get(account) // kept in a file when the key store refused it
		}
		if err == nil {
			key, err = secrets.ParseKey(raw)
			return err
		}
		if !errors.Is(err, errNoSecret) {
			return err
		}
		if key, err = secrets.NewKey(); err != nil {
			return err
		}
		if err := st.set(account, key.String()); err != nil && store != StoreFile {
			return (file{}).set(account, key.String())
		} else if err != nil {
			return err
		}
		return nil
	})
	return key, err
}
