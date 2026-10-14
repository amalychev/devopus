package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// pki holds a local CA limited by Name Constraints to localhost, a leaf the CA may issue
// (app.shop.localhost) and one it must not be able to issue (evil.example.com). Go's
// x509.CreateCertificate does not enforce the parent's constraints, which is exactly what
// an attacker holding the CA key would do.
type pki struct {
	Dir              string
	CAPath           string
	Good, Evil       tls.Certificate
	GoodPEM, EvilPEM [2]string // cert, key paths

	// Variant 2: root (constrained) signs an intermediate (constrained) and its key is
	// thrown away; the agent keeps only the intermediate key. Leaves come with the chain.
	Root2Path    string
	Good2, Evil2 tls.Certificate
}

func newPKI(dir string) (*pki, error) {
	p := &pki{Dir: dir, CAPath: filepath.Join(dir, "ca.pem")}
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber:                serial(),
		Subject:                     pkix.Name{CommonName: "Docklet P-6 local CA"},
		NotBefore:                   time.Now().Add(-time.Hour),
		NotAfter:                    time.Now().AddDate(1, 0, 0),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         []string{"localhost"},
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	ca, _ := x509.ParseCertificate(caDER)
	if err := writePEM(p.CAPath, "CERTIFICATE", caDER); err != nil {
		return nil, err
	}
	issue := func(name string) (tls.Certificate, [2]string, error) {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{
			SerialNumber: serial(),
			Subject:      pkix.Name{CommonName: name},
			DNSNames:     []string{name},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().AddDate(0, 0, 30),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
		if err != nil {
			return tls.Certificate{}, [2]string{}, err
		}
		keyDER, _ := x509.MarshalECPrivateKey(key)
		paths := [2]string{filepath.Join(dir, name+".pem"), filepath.Join(dir, name+"-key.pem")}
		if err := writePEM(paths[0], "CERTIFICATE", der); err != nil {
			return tls.Certificate{}, paths, err
		}
		if err := writePEM(paths[1], "EC PRIVATE KEY", keyDER); err != nil {
			return tls.Certificate{}, paths, err
		}
		c, err := tls.LoadX509KeyPair(paths[0], paths[1])
		return c, paths, err
	}
	if p.Good, p.GoodPEM, err = issue("app.shop.localhost"); err != nil {
		return nil, err
	}
	if p.Evil, p.EvilPEM, err = issue("evil.example.com"); err != nil {
		return nil, err
	}
	if err := p.withIntermediate(); err != nil {
		return nil, err
	}
	return p, nil
}

func constrainedCA(cn string) *x509.Certificate {
	return &x509.Certificate{
		SerialNumber:                serial(),
		Subject:                     pkix.Name{CommonName: cn},
		NotBefore:                   time.Now().Add(-time.Hour),
		NotAfter:                    time.Now().AddDate(1, 0, 0),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		KeyUsage:                    x509.KeyUsageCertSign,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         []string{"localhost"},
	}
}

func (p *pki) withIntermediate() error {
	rootKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rootTmpl := constrainedCA("Docklet P-6 root")
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	if err != nil {
		return err
	}
	root, _ := x509.ParseCertificate(rootDER)
	p.Root2Path = filepath.Join(p.Dir, "root2.pem")
	if err := writePEM(p.Root2Path, "CERTIFICATE", rootDER); err != nil {
		return err
	}
	intKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	intTmpl := constrainedCA("Docklet P-6 intermediate")
	intTmpl.MaxPathLenZero = true
	intDER, err := x509.CreateCertificate(rand.Reader, intTmpl, root, &intKey.PublicKey, rootKey)
	if err != nil {
		return err
	}
	rootKey = nil // the root key is never stored
	inter, _ := x509.ParseCertificate(intDER)
	leaf := func(name string) (tls.Certificate, error) {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
			SerialNumber: serial(), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(0, 0, 30),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}, inter, &key.PublicKey, intKey)
		if err != nil {
			return tls.Certificate{}, err
		}
		return tls.Certificate{Certificate: [][]byte{der, intDER}, PrivateKey: key}, nil
	}
	if p.Good2, err = leaf("app.shop.localhost"); err != nil {
		return err
	}
	p.Evil2, err = leaf("evil.example.com")
	return err
}

func writePEM(path, typ string, der []byte) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o644)
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	return n
}
