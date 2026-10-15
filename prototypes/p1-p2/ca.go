package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// Local CA with Name Constraints limited to .localhost, so a stolen key cannot sign
// certificates for any other domain (docs/architecture/local-networking.md, 3.3).
// Installing it into trust stores needs the user's consent and is not done here.

func caDir() string { return filepath.Join(homeDir(), "ca") }

func CACertPath() string { return filepath.Join(caDir(), "ca.pem") }

func loadOrCreateCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPath, keyPath := CACertPath(), filepath.Join(caDir(), "ca-key.pem")
	if certPEM, err := os.ReadFile(certPath); err == nil {
		keyPEM, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, nil, err
		}
		cb, _ := pem.Decode(certPEM)
		kb, _ := pem.Decode(keyPEM)
		if cb == nil || kb == nil {
			return nil, nil, errors.New("corrupt CA files in " + caDir())
		}
		cert, err := x509.ParseCertificate(cb.Bytes)
		if err != nil {
			return nil, nil, err
		}
		key, err := x509.ParseECPrivateKey(kb.Bytes)
		return cert, key, err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:                serial(),
		Subject:                     pkix.Name{CommonName: "Devcorb prototype local CA", Organization: []string{"Devcorb"}},
		NotBefore:                   time.Now().Add(-time.Hour),
		NotAfter:                    time.Now().AddDate(5, 0, 0),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         []string{"localhost"}, // covers localhost and *.localhost
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	if err := os.MkdirAll(caDir(), 0o700); err != nil {
		return nil, nil, err
	}
	if err := writePEM(certPath, "CERTIFICATE", der, 0o644); err != nil {
		return nil, nil, err
	}
	if err := writePEM(keyPath, "EC PRIVATE KEY", keyDER, 0o600); err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return cert, key, err
}

// IssueCert writes one leaf certificate with a SAN per host. Exact SANs instead of a
// wildcard, because *.shop.localhost would not cover shop.localhost or mail.pay-123.shop.localhost.
func IssueCert(dir string, hosts []string) error {
	ca, caKey, err := loadOrCreateCA()
	if err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: hosts[0]},
		DNSNames:     hosts,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(0, 0, 30),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	if err := writePEM(filepath.Join(dir, "cert.pem"), "CERTIFICATE", der, 0o644); err != nil {
		return err
	}
	// Envoy runs as root in its container and reads the key through a read-only mount.
	return writePEM(filepath.Join(dir, "key.pem"), "EC PRIVATE KEY", keyDER, 0o644)
}

func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), mode)
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	return n
}
