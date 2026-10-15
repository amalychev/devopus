package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// ServerName is the name the agent's xDS server presents and Envoy checks.
const ServerName = "devopus-agent"

// Control holds the mutual TLS material of the xDS link: Envoy trusts only the control
// CA and presents the client certificate; the agent accepts only clients of that CA.
// Without it any local process could connect to the xDS port and read the private
// keys the agent hands to Envoy.
type Control struct {
	CAPEM     []byte
	ServerPEM []byte
	ServerKey []byte
	ClientPEM []byte
	ClientKey []byte
}

const controlFile = "control.pem"

// LoadOrCreateControl reads the control material from dir or creates it. Everything
// lives in one file readable only by the user.
func LoadOrCreateControl(dir string) (*Control, error) {
	path := filepath.Join(dir, controlFile)
	if data, err := os.ReadFile(path); err == nil {
		c, err := parseControl(data)
		if err == nil {
			return c, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	c, err := newControl()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var data []byte
	for _, b := range [][]byte{c.CAPEM, c.ServerPEM, c.ServerKey, c.ClientPEM, c.ClientKey} {
		data = append(data, b...)
	}
	return c, writeFile(path, data, 0o600)
}

func newControl() (*Control, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "Devopus control CA", Organization: []string{"Devopus"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caLifetime),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	ca, _ := x509.ParseCertificate(caDER)
	issue := func(cn string, usage x509.ExtKeyUsage, dns []string) ([]byte, []byte, error) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
			SerialNumber: serial(),
			Subject:      pkix.Name{CommonName: cn},
			DNSNames:     dns,
			NotBefore:    now.Add(-time.Hour),
			NotAfter:     ca.NotAfter,
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		}, ca, &key.PublicKey, caKey)
		if err != nil {
			return nil, nil, err
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			return nil, nil, err
		}
		return pemBlock("CERTIFICATE", der), pemBlock("EC PRIVATE KEY", keyDER), nil
	}
	c := &Control{CAPEM: pemBlock("CERTIFICATE", caDER)}
	if c.ServerPEM, c.ServerKey, err = issue(ServerName, x509.ExtKeyUsageServerAuth, []string{ServerName}); err != nil {
		return nil, err
	}
	if c.ClientPEM, c.ClientKey, err = issue("devopus-proxy", x509.ExtKeyUsageClientAuth, nil); err != nil {
		return nil, err
	}
	return c, nil
}

// parseControl splits the file back: CA, server cert, server key, client cert, client key.
func parseControl(data []byte) (*Control, error) {
	var blocks [][]byte
	for rest := data; ; {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			break
		}
		blocks = append(blocks, pem.EncodeToMemory(b))
	}
	if len(blocks) != 5 {
		return nil, errors.New("control.pem: expected 5 PEM blocks")
	}
	c := &Control{CAPEM: blocks[0], ServerPEM: blocks[1], ServerKey: blocks[2], ClientPEM: blocks[3], ClientKey: blocks[4]}
	if _, err := tls.X509KeyPair(c.ServerPEM, c.ServerKey); err != nil {
		return nil, err
	}
	if _, err := tls.X509KeyPair(c.ClientPEM, c.ClientKey); err != nil {
		return nil, err
	}
	return c, nil
}

// ServerTLS is the agent's side of the xDS link: it requires a client certificate of
// the control CA.
func (c *Control) ServerTLS() (*tls.Config, error) {
	cert, err := tls.X509KeyPair(c.ServerPEM, c.ServerKey)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(c.CAPEM)
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{"h2"},
	}, nil
}

// ClientTLS is the proxy's side, for tests that stand in for Envoy.
func (c *Control) ClientTLS() (*tls.Config, error) {
	cert, err := tls.X509KeyPair(c.ClientPEM, c.ClientKey)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(c.CAPEM)
	return &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, ServerName: ServerName, MinVersion: tls.VersionTLS13}, nil
}
