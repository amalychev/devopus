package acme

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// pebble starts Let's Encrypt's test CA in a container, validating nothing: what is
// checked here is the order, the certificate and keeping it. That the CA reaches the
// challenge through the proxy is checked in the proxy's tests.
func pebble(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker")
	}
	out, err := exec.Command("docker", "run", "-d", "--rm", "-e", "PEBBLE_VA_ALWAYS_VALID=1", "-e", "PEBBLE_VA_NOSLEEP=1",
		"-p", "127.0.0.1::14000", "ghcr.io/letsencrypt/pebble:latest").CombinedOutput()
	if err != nil {
		t.Skipf("pebble: %s", out)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
	port, err := exec.Command("docker", "port", id, "14000").Output()
	if err != nil {
		t.Fatal(err)
	}
	dir := "https://" + strings.TrimSpace(strings.Split(string(port), "\n")[0]) + "/dir"
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if r, err := insecure().Get(dir); err == nil {
			r.Body.Close()
			return dir
		}
		if time.Now().After(deadline) {
			t.Fatal("pebble did not start")
		}
	}
}

func insecure() *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
}

func TestCertificateFromCA(t *testing.T) {
	dir := pebble(t)
	ready := make(chan struct{}, 1)
	m := &Manager{Dir: t.TempDir(), Directory: dir, HTTPClient: insecure(), Ready: func() { ready <- struct{}{} }}
	hosts := []string{"Staging.Shop.Example.com"}

	// The first ask starts getting it.
	if _, err := m.Cert(hosts); err == nil {
		t.Fatal("a certificate at once")
	} else if _, ok := err.(ErrPending); !ok {
		t.Fatalf("not pending: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Minute):
		t.Fatalf("no certificate: %v", m.lastErrs)
	}
	leaf, err := m.Cert(hosts)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(leaf.CertPEM)
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil || cert.DNSNames[0] != "staging.shop.example.com" || leaf.NotAfter.Before(time.Now().Add(24*time.Hour)) {
		t.Fatalf("certificate: %v %v", cert.DNSNames, err)
	}
	if _, err := leaf.TLS(); err != nil {
		t.Errorf("key pair: %v", err)
	}

	// Kept: another manager on the folder serves it without the CA.
	again := &Manager{Dir: m.Dir, Directory: "https://127.0.0.1:1/dir"}
	if l, err := again.Cert(hosts); err != nil || string(l.CertPEM) != string(leaf.CertPEM) {
		t.Errorf("from the folder: %v", err)
	}
	// Close to expiry it is still served while the renewal runs.
	again.Now = func() time.Time { return leaf.NotAfter.Add(-24 * time.Hour) }
	if l, err := again.Cert(hosts); err != nil || l == nil {
		t.Errorf("renewing: %v", err)
	}
	// A failed renewal is not retried at once.
	time.Sleep(time.Second)
	again.mu.Lock()
	busy, failed := again.busy["staging.shop.example.com"], !again.failed["staging.shop.example.com"].IsZero()
	again.mu.Unlock()
	if !busy && !failed {
		t.Error("no renewal was tried")
	}
}

func TestChallengeHandler(t *testing.T) {
	m := &Manager{}
	m.tokens = map[string]string{"tok": "tok.thumb"}
	srv := httptest.NewServer(m)
	defer srv.Close()
	get := func(path string) (int, string) {
		r, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	if code, body := get(ChallengePath + "tok"); code != 200 || body != "tok.thumb" {
		t.Errorf("token: %d %q", code, body)
	}
	for _, p := range []string{ChallengePath + "other", "/", "/tok"} {
		if code, _ := get(p); code != 404 {
			t.Errorf("%s: %d", p, code)
		}
	}
}
