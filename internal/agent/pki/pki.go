// Package pki issues the certificates of the local proxy (docs/architecture/local-networking.md §3).
//
// The local CA has two tiers. The root is limited by Name Constraints to localhost and
// signs an intermediate with the same constraints; the root key is thrown away right
// after that, so a stolen agent key can never sign a certificate outside .localhost,
// not even for Java, which ignores constraints on roots (P-6). Leaves come from the
// intermediate and travel with it.
//
// A separate control CA secures the xDS link between the agent and Envoy (mutual TLS):
// it never leaves the device and nobody is asked to trust it.
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// Lifetimes. The root and the intermediate outlive any reasonable install; leaves are
// short and renewed by the reconciler well before they expire.
const (
	caLifetime   = 10 * 365 * 24 * time.Hour
	LeafLifetime = 90 * 24 * time.Hour
	RenewBefore  = 30 * 24 * time.Hour
)

// PermittedDomains is what the local CA may ever sign. A DNS constraint "localhost"
// covers localhost and every name under it.
var PermittedDomains = []string{"localhost"}

// CA is the local certificate authority as the agent keeps it: the root certificate
// (what users trust) and the intermediate with its key.
type CA struct {
	Root         *x509.Certificate
	Intermediate *x509.Certificate
	key          crypto.Signer
	dir          string
}

// Files of the CA directory.
const (
	RootFile            = "root.pem"
	IntermediateFile    = "intermediate.pem"
	IntermediateKeyFile = "intermediate-key.pem"
)

// RootPath is the certificate users add to their trust stores.
func (c *CA) RootPath() string { return filepath.Join(c.dir, RootFile) }

// Fingerprint is the SHA-256 of the root certificate, as keychains show it.
func (c *CA) Fingerprint() string {
	sum := sha256.Sum256(c.Root.Raw)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// LoadOrCreateCA reads the CA from dir or creates it there. The root key exists only
// in memory for the moment it takes to sign the intermediate.
func LoadOrCreateCA(dir string) (*CA, error) {
	ca, err := LoadCA(dir)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return ca, err
	}
	return createCA(dir)
}

// LoadCA reads an existing CA; os.ErrNotExist if there is none yet.
func LoadCA(dir string) (*CA, error) {
	root, err := readCert(filepath.Join(dir, RootFile))
	if err != nil {
		return nil, err
	}
	inter, err := readCert(filepath.Join(dir, IntermediateFile))
	if err != nil {
		return nil, err
	}
	key, err := readKey(filepath.Join(dir, IntermediateKeyFile))
	if err != nil {
		return nil, err
	}
	if err := inter.CheckSignatureFrom(root); err != nil {
		return nil, fmt.Errorf("%s is not signed by %s: %w", IntermediateFile, RootFile, err)
	}
	return &CA{Root: root, Intermediate: inter, key: key, dir: dir}, nil
}

func createCA(dir string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	host = strings.TrimSuffix(host, ".local")
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	rootTmpl := constrainedCA("Devcorb local CA (" + host + ")")
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	if err != nil {
		return nil, err
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		return nil, err
	}
	interKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	interTmpl := constrainedCA("Devcorb local intermediate (" + host + ")")
	interTmpl.MaxPathLenZero = true
	interTmpl.NotAfter = root.NotAfter.Add(-24 * time.Hour)
	interDER, err := x509.CreateCertificate(rand.Reader, interTmpl, root, &interKey.PublicKey, rootKey)
	if err != nil {
		return nil, err
	}
	rootKey = nil //nolint:ineffassign // the root key is never stored: no new intermediate can be made
	inter, err := x509.ParseCertificate(interDER)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(interKey)
	if err != nil {
		return nil, err
	}
	// The key goes first: a directory with certificates but no key is unusable, the
	// reverse is just garbage the next attempt overwrites.
	if err := writeFile(filepath.Join(dir, IntermediateKeyFile), pemBlock("EC PRIVATE KEY", keyDER), 0o600); err != nil {
		return nil, err
	}
	if err := writeFile(filepath.Join(dir, IntermediateFile), pemBlock("CERTIFICATE", interDER), 0o644); err != nil {
		return nil, err
	}
	if err := writeFile(filepath.Join(dir, RootFile), pemBlock("CERTIFICATE", rootDER), 0o644); err != nil {
		return nil, err
	}
	return &CA{Root: root, Intermediate: inter, key: interKey, dir: dir}, nil
}

func constrainedCA(cn string) *x509.Certificate {
	now := time.Now()
	return &x509.Certificate{
		SerialNumber:                serial(),
		Subject:                     pkix.Name{CommonName: cn, Organization: []string{"Devcorb"}},
		NotBefore:                   now.Add(-time.Hour),
		NotAfter:                    now.Add(caLifetime),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         PermittedDomains,
	}
}

// Leaf is a server certificate with its chain (leaf, intermediate) and key, in PEM.
type Leaf struct {
	Hosts    []string
	NotAfter time.Time
	CertPEM  []byte // leaf followed by the intermediate
	KeyPEM   []byte
}

// Issue signs a server certificate for hosts. It refuses names outside the CA's
// constraints instead of producing a certificate every client would reject.
func (c *CA) Issue(hosts []string) (*Leaf, error) {
	hosts = normalize(hosts)
	if len(hosts) == 0 {
		return nil, errors.New("no hosts to issue a certificate for")
	}
	for _, h := range hosts {
		if !Permitted(h) {
			return nil, fmt.Errorf("%s is outside the local CA's domains (%s)", h, strings.Join(PermittedDomains, ", "))
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	notAfter := now.Add(LeafLifetime)
	if notAfter.After(c.Intermediate.NotAfter) {
		notAfter = c.Intermediate.NotAfter
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: hosts[0]},
		DNSNames:     hosts,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, c.Intermediate, &key.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	chain := append(pemBlock("CERTIFICATE", der), pemBlock("CERTIFICATE", c.Intermediate.Raw)...)
	cert, _ := x509.ParseCertificate(der)
	return &Leaf{Hosts: hosts, NotAfter: cert.NotAfter, CertPEM: chain, KeyPEM: pemBlock("EC PRIVATE KEY", keyDER)}, nil
}

// Permitted reports whether the CA may sign host.
func Permitted(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, d := range PermittedDomains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// Fresh reports whether a leaf covers exactly hosts and does not need renewal yet.
func (l *Leaf) Fresh(hosts []string, now time.Time) bool {
	return l != nil && slices.Equal(l.Hosts, normalize(hosts)) && now.Add(RenewBefore).Before(l.NotAfter)
}

// TLS returns the leaf as a tls.Certificate, chain included.
func (l *Leaf) TLS() (tls.Certificate, error) { return tls.X509KeyPair(l.CertPEM, l.KeyPEM) }

// Leaves keeps issued certificates on disk so that an agent restart serves the same
// ones and Envoy sees no change.
type Leaves struct{ Dir string }

func (s Leaves) paths(name string) (string, string) {
	return filepath.Join(s.Dir, name+".pem"), filepath.Join(s.Dir, name+"-key.pem")
}

// Load returns the stored leaf called name, or nil if there is none or it is unreadable.
func (s Leaves) Load(name string) *Leaf {
	certPath, keyPath := s.paths(name)
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return nil
	}
	return &Leaf{Hosts: normalize(cert.DNSNames), NotAfter: cert.NotAfter, CertPEM: certPEM, KeyPEM: keyPEM}
}

func (s Leaves) Save(name string, l *Leaf) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	certPath, keyPath := s.paths(name)
	if err := writeFile(keyPath, l.KeyPEM, 0o600); err != nil {
		return err
	}
	return writeFile(certPath, l.CertPEM, 0o644)
}

func (s Leaves) Remove(name string) error {
	certPath, keyPath := s.paths(name)
	return errors.Join(ignoreMissing(os.Remove(certPath)), ignoreMissing(os.Remove(keyPath)))
}

// Names lists the stored leaves.
func (s Leaves) Names() []string {
	entries, _ := os.ReadDir(s.Dir)
	var out []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, ".pem") && !strings.HasSuffix(n, "-key.pem") {
			out = append(out, strings.TrimSuffix(n, ".pem"))
		}
	}
	return out
}

func normalize(hosts []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	// The first host stays first: it is the certificate's common name.
	if len(out) > 1 {
		sort.Strings(out[1:])
	}
	return out
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

func pemBlock(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

func readCert(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s: no certificate", path)
	}
	return x509.ParseCertificate(block.Bytes)
}

func readKey(path string) (crypto.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s: no key", path)
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

// writeFile replaces path atomically with the given mode.
func writeFile(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func ignoreMissing(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
