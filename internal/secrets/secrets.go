// Package secrets is the end-to-end encryption of a workspace's secrets (ADR-0009,
// docs/architecture/security.md): age with X25519 keys.
//
//   - The workspace has a key pair. Its public half (the recipient) is on the
//     workspace server: anyone allowed to set a secret encrypts to it.
//   - Every device has a key pair of its own. The workspace's private key reaches a
//     device wrapped — encrypted — to the device's public key, by a device that holds
//     it already (an admin's or a teamlead's agent), or from the recovery key.
//   - The server keeps public keys, wrapped keys and ciphertexts: it reads no value.
package secrets

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
)

// NewKey makes a key pair: a workspace's or a device's.
func NewKey() (*age.X25519Identity, error) { return age.GenerateX25519Identity() }

// ParseKey reads a private key: AGE-SECRET-KEY-1…, the form of the recovery key too.
func ParseKey(s string) (*age.X25519Identity, error) {
	id, err := age.ParseX25519Identity(strings.TrimSpace(s))
	if err != nil {
		return nil, errors.New("not a Devopus key: it starts with AGE-SECRET-KEY-1")
	}
	return id, nil
}

// Seal encrypts a value to a public key (age1…).
func Seal(value []byte, publicKey string) ([]byte, error) {
	r, err := age.ParseX25519Recipient(publicKey)
	if err != nil {
		return nil, fmt.Errorf("public key: %w", err)
	}
	var out bytes.Buffer
	w, err := age.Encrypt(&out, r)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(value); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Open decrypts what Seal made for key.
func Open(sealed []byte, key *age.X25519Identity) ([]byte, error) {
	r, err := age.Decrypt(bytes.NewReader(sealed), key)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// Wrap encrypts a workspace key for a device.
func Wrap(workspaceKey *age.X25519Identity, devicePublic string) ([]byte, error) {
	return Seal([]byte(workspaceKey.String()), devicePublic)
}

// Unwrap opens a workspace key wrapped for this device.
func Unwrap(wrapped []byte, device *age.X25519Identity) (*age.X25519Identity, error) {
	b, err := Open(wrapped, device)
	if err != nil {
		return nil, fmt.Errorf("the workspace key is not for this device: %w", err)
	}
	return ParseKey(string(b))
}
