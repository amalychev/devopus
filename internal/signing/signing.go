// Package signing signs and checks versions of projects' settings (ADR-0018, risk R21,
// layer 2). Whoever controls the workspace server could otherwise make every agent
// of the team run what they like; with signatures an agent applies a version that
// runs new code only when a person the workspace's admin trusts made it.
//
//   - Every device has an Ed25519 signing key, kept like its other keys.
//   - The workspace's root of trust is the signing key of its admin's device. An agent
//     pins it the first time it sees the workspace, and does not take another one
//     from the server afterwards.
//   - The admin certifies, with the root, the devices of the people who change
//     settings: a certificate binds a key to a person and a device.
//   - The author's device signs each version: the workspace, the project, the
//     version number and the hash of the settings exactly as stored.
package signing

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Key is a device's signing key.
type Key struct {
	Private ed25519.PrivateKey
}

// NewKey makes a signing key.
func NewKey() (Key, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	return Key{Private: priv}, err
}

// ParseKey reads a key as String wrote it.
func ParseKey(s string) (Key, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(strings.TrimSpace(s), "ed25519:"))
	if err != nil || len(b) != ed25519.SeedSize {
		return Key{}, errors.New("not a signing key")
	}
	return Key{Private: ed25519.NewKeyFromSeed(b)}, nil
}

// String is the private key for the key store.
func (k Key) String() string {
	return "ed25519:" + base64.StdEncoding.EncodeToString(k.Private.Seed())
}

// Public is the public half.
func (k Key) Public() ed25519.PublicKey { return k.Private.Public().(ed25519.PublicKey) }

// Sign signs a message of this package.
func (k Key) Sign(msg []byte) []byte { return ed25519.Sign(k.Private, msg) }

// Fingerprint is a short form of a public key for people to compare: the admin reads
// it out, the developer sees it in 'devopus trust show'.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:10])
	return s[0:4] + "-" + s[4:8] + "-" + s[8:12] + "-" + s[12:16]
}

// SettingsMessage is what the author signs for a version: it cannot be shown as
// another project's, another workspace's or another number's version.
func SettingsMessage(workspace, project string, version int32, config []byte) []byte {
	sum := sha256.Sum256(config)
	return []byte("devopus-settings-v1\n" + workspace + "\n" + project + "\n" + strconv.Itoa(int(version)) + "\n" + hex.EncodeToString(sum[:]))
}

// SignerMessage is what the root signs to certify a device of a person.
func SignerMessage(workspace, userID, email, deviceID string, pub ed25519.PublicKey) []byte {
	return []byte("devopus-signer-v1\n" + workspace + "\n" + userID + "\n" + email + "\n" + deviceID + "\n" + base64.StdEncoding.EncodeToString(pub))
}

// Signer is a device whose signatures count: the root's, or one it certified.
type Signer struct {
	DeviceID, UserID, Email string
	Public                  ed25519.PublicKey
	// Certificate is the root's signature of SignerMessage.
	Certificate []byte
}

// Set is what an agent checks versions with: the root it pinned and the signers the
// server says the root certified — each certificate is checked, so the server can
// add no one.
type Set struct {
	Workspace string
	Root      ed25519.PublicKey
	// RootDevice and RootEmail are whose device holds the root.
	RootDevice, RootEmail string
	Signers               []Signer
}

// Errors of Verify.
var (
	ErrUnsigned  = errors.New("the version is not signed")
	ErrNoRoot    = errors.New("the workspace has no root of trust yet")
	ErrUntrusted = errors.New("signed by a device the workspace's admin has not certified")
	ErrSignature = errors.New("the signature does not match the settings")
)

// Verify checks a version and returns who signed it.
func (s Set) Verify(project string, version int32, config []byte, deviceID string, sig []byte) (email string, err error) {
	if len(sig) == 0 || deviceID == "" {
		return "", ErrUnsigned
	}
	if len(s.Root) != ed25519.PublicKeySize {
		return "", ErrNoRoot
	}
	pub, email := ed25519.PublicKey(nil), ""
	if deviceID == s.RootDevice {
		pub, email = s.Root, s.RootEmail
	} else {
		for _, sg := range s.Signers {
			if sg.DeviceID == deviceID && s.Certified(sg) {
				pub, email = sg.Public, sg.Email
				break
			}
		}
	}
	if pub == nil {
		return "", ErrUntrusted
	}
	if !ed25519.Verify(pub, SettingsMessage(s.Workspace, project, version, config), sig) {
		return "", fmt.Errorf("%w (device %s of %s)", ErrSignature, deviceID, email)
	}
	return email, nil
}

// Certified: the root signed this signer's certificate.
func (s Set) Certified(sg Signer) bool {
	return len(s.Root) == ed25519.PublicKeySize && len(sg.Public) == ed25519.PublicKeySize &&
		ed25519.Verify(s.Root, SignerMessage(s.Workspace, sg.UserID, sg.Email, sg.DeviceID, sg.Public), sg.Certificate)
}
