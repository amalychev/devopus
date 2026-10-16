package token

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newKey(t *testing.T, id string) Key {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return Key{ID: id, Private: priv}
}

func TestSignVerify(t *testing.T) {
	k := newKey(t, "k1")
	now := time.Unix(1_800_000_000, 0)
	c := Claims{Issuer: "https://auth", Subject: "usr_1", Session: "ses_1", Audience: DefaultAudience, IssuedAt: now.Unix(), Expiry: now.Add(15 * time.Minute).Unix()}
	tok, err := Sign(k, c)
	if err != nil {
		t.Fatal(err)
	}
	set := JWKS{Keys: []JWK{PublicJWK(k)}}
	want := Expect{Issuer: "https://auth", Audience: DefaultAudience, Now: now}
	got, err := Verify(tok, set.Find, want)
	if err != nil || got != c {
		t.Fatalf("Verify = %+v, %v", got, err)
	}

	late := want
	late.Now = now.Add(15 * time.Minute)
	if _, err := Verify(tok, set.Find, late); !errors.Is(err, ErrExpired) {
		t.Errorf("expired: %v", err)
	}
	other := want
	other.Audience = "wsp_1"
	if _, err := Verify(tok, set.Find, other); !errors.Is(err, ErrClaims) {
		t.Errorf("audience: %v", err)
	}

	// A token signed by a key not in the set, and a tampered payload.
	k2 := newKey(t, "k1")
	forged, _ := Sign(k2, c)
	if _, err := Verify(forged, set.Find, want); !errors.Is(err, ErrSignature) {
		t.Errorf("forged: %v", err)
	}
	parts := strings.Split(tok, ".")
	c.Subject = "usr_2"
	p, _ := json.Marshal(c)
	if _, err := Verify(parts[0]+"."+b64.EncodeToString(p)+"."+parts[2], set.Find, want); !errors.Is(err, ErrSignature) {
		t.Errorf("tampered: %v", err)
	}
	if _, err := Verify("abc", set.Find, want); !errors.Is(err, ErrMalformed) {
		t.Errorf("malformed: %v", err)
	}
}

func TestRemotePicksUpRotatedKeys(t *testing.T) {
	k1, k2 := newKey(t, "k1"), newKey(t, "k2")
	set := JWKS{Keys: []JWK{PublicJWK(k1)}}
	fetches := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		_ = json.NewEncoder(w).Encode(set)
	}))
	defer srv.Close()

	r := &Remote{URL: srv.URL}
	if _, err := r.Key("k1"); err != nil || fetches != 1 {
		t.Fatalf("k1: %v, %d fetches", err, fetches)
	}
	set.Keys = append(set.Keys, PublicJWK(k2))
	// Within a minute of the last fetch an unknown key is not refetched.
	if _, err := r.Key("k2"); err == nil || fetches != 1 {
		t.Fatalf("k2 too early: %v, %d fetches", err, fetches)
	}
	r.fetched = time.Now().Add(-2 * time.Minute)
	if _, err := r.Key("k2"); err != nil || fetches != 2 {
		t.Fatalf("k2: %v, %d fetches", err, fetches)
	}
}
