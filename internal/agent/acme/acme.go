// Package acme gets the certificates of a server's public environments from Let's
// Encrypt (R14, ADR-0020). Envoy terminates TLS, so the HTTP-01 challenge is the one
// that works: the proxy sends /.well-known/acme-challenge/ on port 80 to this
// package's handler on the host. Certificates and the account key are kept in a
// folder of the agent; one is renewed 30 days before it expires.
package acme

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/amalychev/devopus/internal/agent/pki"
)

const (
	// LetsEncrypt is the production directory.
	LetsEncrypt = acme.LetsEncryptURL
	// ChallengePath is where the CA reads the HTTP-01 answers.
	ChallengePath = "/.well-known/acme-challenge/"
	// renewBefore: a certificate is renewed when it has less left.
	renewBefore = 30 * 24 * time.Hour
	// retryAfter: after a failure the CA is not asked again sooner, to keep within its
	// limits (Let's Encrypt allows 5 failed validations per host an hour).
	retryAfter = 10 * time.Minute
)

// ErrPending: the certificate is being got, or the last attempt failed and the next
// one waits; Err says why.
type ErrPending struct{ Err error }

func (e ErrPending) Error() string {
	if e.Err == nil {
		return "the certificate is on its way from the CA"
	}
	return "no certificate from the CA yet: " + e.Err.Error()
}

// Manager gets and keeps certificates.
type Manager struct {
	// Dir keeps the account key and the certificates.
	Dir string
	// Directory is the CA's ACME directory; Let's Encrypt when empty.
	Directory string
	// Email is told to the CA for expiry notices; optional.
	Email string
	// HTTPClient talks to the CA; http.DefaultClient when nil.
	HTTPClient *http.Client
	// Ready is called when a certificate arrives: the agent applies it.
	Ready func()
	Log   *slog.Logger
	// Now is the clock; time.Now when nil.
	Now func() time.Time

	mu       sync.Mutex
	client   *acme.Client
	tokens   map[string]string
	busy     map[string]bool
	failed   map[string]time.Time
	lastErrs map[string]error
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// ServeHTTP answers the CA's HTTP-01 challenges; everything else is not here.
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.URL.Path, ChallengePath)
	m.mu.Lock()
	answer, known := m.tokens[token]
	m.mu.Unlock()
	if !ok || !known {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(answer))
}

// Cert is the certificate for hosts. One far from expiry comes from the folder; one
// close to it comes too while its renewal runs in the background; with none the
// first is got in the background and ErrPending is returned. Ready is called when
// it arrives.
func (m *Manager) Cert(hosts []string) (*pki.Leaf, error) {
	hosts = names(hosts)
	if len(hosts) == 0 {
		return nil, errors.New("no hosts")
	}
	key := strings.Join(hosts, ",")
	leaf := m.load(key)
	now := m.now()
	if leaf != nil && now.Add(renewBefore).Before(leaf.NotAfter) {
		return leaf, nil
	}
	m.mu.Lock()
	if m.busy == nil {
		m.busy, m.failed, m.lastErrs = map[string]bool{}, map[string]time.Time{}, map[string]error{}
	}
	last := m.lastErrs[key]
	if !m.busy[key] && now.Sub(m.failed[key]) >= retryAfter {
		m.busy[key] = true
		go m.obtain(hosts, key)
	}
	m.mu.Unlock()
	if leaf != nil && now.Before(leaf.NotAfter) {
		return leaf, nil // renewing
	}
	return nil, ErrPending{Err: last}
}

func (m *Manager) obtain(hosts []string, key string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	err := m.issue(ctx, hosts, key)
	m.mu.Lock()
	m.busy[key] = false
	if err != nil {
		m.failed[key], m.lastErrs[key] = m.now(), err
	} else {
		delete(m.failed, key)
		delete(m.lastErrs, key)
	}
	m.mu.Unlock()
	if m.Log != nil {
		if err != nil {
			m.Log.Warn("certificate from the CA", "hosts", key, "err", err)
		} else {
			m.Log.Info("certificate from the CA", "hosts", key)
		}
	}
	if err == nil && m.Ready != nil {
		m.Ready()
	}
}

func (m *Manager) issue(ctx context.Context, hosts []string, key string) error {
	c, err := m.acmeClient(ctx)
	if err != nil {
		return err
	}
	order, err := c.AuthorizeOrder(ctx, acme.DomainIDs(hosts...))
	if err != nil {
		return fmt.Errorf("order: %w", err)
	}
	for _, u := range order.AuthzURLs {
		z, err := c.GetAuthorization(ctx, u)
		if err != nil {
			return err
		}
		if z.Status == acme.StatusValid {
			continue
		}
		var chal *acme.Challenge
		for _, ch := range z.Challenges {
			if ch.Type == "http-01" {
				chal = ch
			}
		}
		if chal == nil {
			return fmt.Errorf("%s: the CA offers no http-01 challenge", z.Identifier.Value)
		}
		answer, err := c.HTTP01ChallengeResponse(chal.Token)
		if err != nil {
			return err
		}
		m.mu.Lock()
		if m.tokens == nil {
			m.tokens = map[string]string{}
		}
		m.tokens[chal.Token] = answer
		m.mu.Unlock()
		_, err = c.Accept(ctx, chal)
		if err == nil {
			_, err = c.WaitAuthorization(ctx, z.URI)
		}
		m.mu.Lock()
		delete(m.tokens, chal.Token)
		m.mu.Unlock()
		if err != nil {
			return fmt.Errorf("%s: %w (the CA must reach http://%s%s through port 80)", z.Identifier.Value, err, z.Identifier.Value, ChallengePath)
		}
	}
	uri := order.URI
	if order, err = c.WaitOrder(ctx, uri); err != nil {
		return err
	}
	if order.URI == "" {
		order.URI = uri
	}
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: hosts[0]}, DNSNames: hosts}, certKey)
	if err != nil {
		return err
	}
	ders, _, err := c.CreateCertFromOrder(ctx, order, csr, true)
	if err != nil {
		return fmt.Errorf("certificate: %w", err)
	}
	var chain []byte
	for _, d := range ders {
		chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: d})...)
	}
	keyDER, err := x509.MarshalECPrivateKey(certKey)
	if err != nil {
		return err
	}
	return m.save(key, chain, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

// acmeClient is the client of the agent's account at the CA, registered once.
func (m *Manager) acmeClient(ctx context.Context) (*acme.Client, error) {
	m.mu.Lock()
	c := m.client
	m.mu.Unlock()
	if c != nil {
		return c, nil
	}
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return nil, err
	}
	accountKey, err := m.accountKey()
	if err != nil {
		return nil, err
	}
	dir := m.Directory
	if dir == "" {
		dir = LetsEncrypt
	}
	c = &acme.Client{Key: accountKey, DirectoryURL: dir, HTTPClient: m.HTTPClient, UserAgent: "devopus-agent"}
	acct := &acme.Account{}
	if m.Email != "" {
		acct.Contact = []string{"mailto:" + m.Email}
	}
	if _, err := c.Register(ctx, acct, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil, fmt.Errorf("the account at %s: %w", dir, err)
	}
	m.mu.Lock()
	m.client = c
	m.mu.Unlock()
	return c, nil
}

func (m *Manager) accountKey() (crypto.Signer, error) {
	file := filepath.Join(m.Dir, "account-key.pem")
	if b, err := os.ReadFile(file); err == nil {
		if blk, _ := pem.Decode(b); blk != nil {
			return x509.ParseECPrivateKey(blk.Bytes)
		}
		return nil, fmt.Errorf("%s: not a key", file)
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, err
	}
	return k, writeFile(file, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

func (m *Manager) paths(key string) (string, string) {
	base := filepath.Join(m.Dir, strings.ReplaceAll(key, ",", "+"))
	return base + ".pem", base + "-key.pem"
}

func (m *Manager) load(key string) *pki.Leaf {
	certFile, keyFile := m.paths(key)
	certPEM, err1 := os.ReadFile(certFile)
	keyPEM, err2 := os.ReadFile(keyFile)
	if err1 != nil || err2 != nil {
		return nil
	}
	blk, _ := pem.Decode(certPEM)
	if blk == nil {
		return nil
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return nil
	}
	return &pki.Leaf{Hosts: strings.Split(key, ","), NotAfter: cert.NotAfter, CertPEM: certPEM, KeyPEM: keyPEM}
}

func (m *Manager) save(key string, certPEM, keyPEM []byte) error {
	certFile, keyFile := m.paths(key)
	if err := writeFile(keyFile, keyPEM); err != nil {
		return err
	}
	return writeFile(certFile, certPEM)
}

func writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// names are the hosts as certificates name them: lower case, without duplicates, the
// first one first.
func names(hosts []string) []string {
	var out []string
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
		if h != "" && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}
