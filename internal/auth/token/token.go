// Package token signs and checks Devopus access tokens: JWTs signed with Ed25519
// (alg EdDSA) whose public keys Auth publishes as JWKS. Workspace servers check
// tokens with a Remote key set, without calling Auth on every request.
package token

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DefaultAudience is the audience of tokens for Devopus itself rather than one workspace.
const DefaultAudience = "devopus"

// Claims are what an access token says: the user (sub) and the session (sid).
type Claims struct {
	Issuer  string `json:"iss"`
	Subject string `json:"sub"`
	Session string `json:"sid"`
	// Email is the user's email: workspace servers match invites by it.
	Email    string `json:"email,omitempty"`
	Audience string `json:"aud"`
	IssuedAt int64  `json:"iat"`
	Expiry   int64  `json:"exp"`
}

// ExpiresAt is the expiry as a time.
func (c Claims) ExpiresAt() time.Time { return time.Unix(c.Expiry, 0) }

// Key is a signing key.
type Key struct {
	ID      string
	Private ed25519.PrivateKey
}

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

var b64 = base64.RawURLEncoding

// Sign makes a signed token.
func Sign(k Key, c Claims) (string, error) {
	h, err := json.Marshal(header{Alg: "EdDSA", Typ: "JWT", Kid: k.ID})
	if err != nil {
		return "", err
	}
	p, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signing := b64.EncodeToString(h) + "." + b64.EncodeToString(p)
	sig := ed25519.Sign(k.Private, []byte(signing))
	return signing + "." + b64.EncodeToString(sig), nil
}

// Errors of Verify.
var (
	ErrMalformed = errors.New("malformed token")
	ErrSignature = errors.New("bad token signature")
	ErrExpired   = errors.New("token expired")
	ErrClaims    = errors.New("token is not for this service")
)

// KeyFunc returns the public key with an id.
type KeyFunc func(kid string) (ed25519.PublicKey, error)

// Expect is what Verify requires of the claims.
type Expect struct {
	Issuer   string
	Audience string
	Now      time.Time
}

// Verify checks a token's signature and claims.
func Verify(tok string, keys KeyFunc, want Expect) (Claims, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return Claims{}, ErrMalformed
	}
	hb, err := b64.DecodeString(parts[0])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var h header
	if err := json.Unmarshal(hb, &h); err != nil || h.Alg != "EdDSA" {
		return Claims{}, ErrMalformed
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	pub, err := keys(h.Kid)
	if err != nil {
		return Claims{}, err
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return Claims{}, ErrSignature
	}
	pb, err := b64.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var c Claims
	if err := json.Unmarshal(pb, &c); err != nil {
		return Claims{}, ErrMalformed
	}
	now := want.Now
	if now.IsZero() {
		now = time.Now()
	}
	if now.Unix() >= c.Expiry {
		return c, ErrExpired
	}
	if (want.Issuer != "" && c.Issuer != want.Issuer) || (want.Audience != "" && c.Audience != want.Audience) || c.Subject == "" || c.Session == "" {
		return c, ErrClaims
	}
	return c, nil
}

// JWK is a public Ed25519 key (RFC 8037).
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
}

// JWKS is a key set as Auth publishes it.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// PublicJWK describes the public half of a key.
func PublicJWK(k Key) JWK {
	return JWK{Kty: "OKP", Crv: "Ed25519", X: b64.EncodeToString(k.Private.Public().(ed25519.PublicKey)), Kid: k.ID, Use: "sig", Alg: "EdDSA"}
}

// Find returns the key with an id.
func (s JWKS) Find(kid string) (ed25519.PublicKey, error) {
	for _, k := range s.Keys {
		if k.Kid != kid {
			continue
		}
		if k.Kty != "OKP" || k.Crv != "Ed25519" {
			return nil, fmt.Errorf("key %s is not Ed25519", kid)
		}
		x, err := b64.DecodeString(k.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("key %s is malformed", kid)
		}
		return ed25519.PublicKey(x), nil
	}
	return nil, fmt.Errorf("%w: unknown key %q", ErrSignature, kid)
}
