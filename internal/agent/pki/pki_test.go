package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCAIssuesVerifiableLeaves(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Issue([]string{"shop.localhost", "mail.shop.localhost", "SHOP.localhost."})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(leaf.Hosts, ","); got != "shop.localhost,mail.shop.localhost" {
		t.Errorf("hosts = %s", got)
	}
	verify(t, ca, leaf.CertPEM, "mail.shop.localhost")

	// Only the intermediate key is on disk; the root key was never written.
	entries, _ := os.ReadDir(dir)
	var keys []string
	for _, e := range entries {
		if strings.Contains(e.Name(), "key") {
			keys = append(keys, e.Name())
		}
	}
	if strings.Join(keys, ",") != IntermediateKeyFile {
		t.Errorf("key files: %v", keys)
	}
	if fi, _ := os.Stat(dir + "/" + IntermediateKeyFile); fi.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v", fi.Mode().Perm())
	}

	again, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Root.Equal(ca.Root) || again.Fingerprint() != ca.Fingerprint() {
		t.Error("reloading created a new CA")
	}
}

func TestCARefusesForeignNames(t *testing.T) {
	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.Issue([]string{"shop.localhost", "evil.example.com"}); err == nil {
		t.Fatal("issued a certificate for evil.example.com")
	}
	// Someone holding the intermediate key signs a foreign name anyway: verification
	// must fail because the intermediate itself carries the constraints.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "evil.example.com"}, DNSNames: []string{"evil.example.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, ca.Intermediate, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	_, err = cert.Verify(opts(ca, "evil.example.com"))
	if err == nil || !strings.Contains(err.Error(), "constraint") {
		t.Fatalf("foreign leaf verified: %v", err)
	}
}

func TestLeavesRoundTrip(t *testing.T) {
	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := Leaves{Dir: t.TempDir()}
	if store.Load("env1") != nil {
		t.Fatal("load of a missing leaf")
	}
	leaf, err := ca.Issue([]string{"shop.localhost"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("env1", leaf); err != nil {
		t.Fatal(err)
	}
	got := store.Load("env1")
	if got == nil || string(got.CertPEM) != string(leaf.CertPEM) {
		t.Fatal("stored leaf differs")
	}
	now := time.Now()
	if !got.Fresh([]string{"shop.localhost"}, now) {
		t.Error("new leaf is not fresh")
	}
	if got.Fresh([]string{"shop.localhost", "mail.shop.localhost"}, now) {
		t.Error("leaf with fewer hosts is fresh")
	}
	if got.Fresh([]string{"shop.localhost"}, now.Add(LeafLifetime-RenewBefore+time.Hour)) {
		t.Error("leaf close to expiry is fresh")
	}
	if names := store.Names(); len(names) != 1 || names[0] != "env1" {
		t.Errorf("names = %v", names)
	}
	if err := store.Remove("env1"); err != nil || store.Load("env1") != nil {
		t.Fatalf("remove: %v", err)
	}
}

func TestControlMutualTLS(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadOrCreateControl(dir)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := LoadOrCreateControl(dir)
	if err != nil || string(c2.ClientPEM) != string(c.ClientPEM) {
		t.Fatalf("reload: %v", err)
	}
	srvCfg, _ := c.ServerTLS()
	srvCfg.NextProtos = nil
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.(*tls.Conn).Handshake()
			conn.Write([]byte("ok"))
			conn.Close()
		}
	}()
	cliCfg, _ := c.ClientTLS()
	conn, err := tls.Dial("tcp", ln.Addr().String(), cliCfg)
	if err != nil {
		t.Fatalf("client with the control certificate: %v", err)
	}
	conn.Close()

	// A client without a certificate gets nothing.
	anon := &tls.Config{RootCAs: cliCfg.RootCAs, ServerName: ServerName}
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	tc := tls.Client(raw, anon)
	buf := make([]byte, 2)
	_ = tc.Handshake()
	if n, _ := tc.Read(buf); n > 0 {
		t.Fatal("anonymous client got data")
	}
	tc.Close()
}

func verify(t *testing.T, ca *CA, chain []byte, host string) {
	t.Helper()
	block, rest := pem.Decode(chain)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	o := opts(ca, host)
	if inter, _ := pem.Decode(rest); inter == nil {
		t.Fatal("chain lacks the intermediate")
	}
	if _, err := leaf.Verify(o); err != nil {
		t.Fatalf("verify %s: %v", host, err)
	}
}

func opts(ca *CA, host string) x509.VerifyOptions {
	roots, inters := x509.NewCertPool(), x509.NewCertPool()
	roots.AddCert(ca.Root)
	inters.AddCert(ca.Intermediate)
	return x509.VerifyOptions{DNSName: host, Roots: roots, Intermediates: inters}
}
