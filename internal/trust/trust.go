// Package trust checks and changes whether the system trusts the local CA
// (docs/architecture/local-networking.md §3). Every change asks the person first;
// the CLI does that, this package only acts.
package trust

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/devcorb/devcorb/internal/agent/pki"
)

// Trusted reports whether the system verifier accepts a certificate the local CA
// issues. On macOS Go asks the keychain's trust settings, so this sees exactly what
// Safari and Chrome see.
func Trusted(ca *pki.CA) (bool, error) {
	leaf, err := ca.Issue([]string{"devcorb-doctor.localhost"})
	if err != nil {
		return false, err
	}
	certs, err := parseChain(leaf.CertPEM)
	if err != nil {
		return false, err
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	_, err = certs[0].Verify(x509.VerifyOptions{DNSName: "devcorb-doctor.localhost", Intermediates: inter, CurrentTime: time.Now()})
	return err == nil, nil
}

// CanInstall reports whether Install works on this system without root.
func CanInstall() bool { return runtime.GOOS == "darwin" }

// Install adds the root to the user's login keychain as trusted for TLS. macOS asks
// for the account password in its own dialog; nothing else is changed.
func Install(rootPath string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("automatic install is macOS-only for now; " + ManualSteps(rootPath))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	keychain := filepath.Join(home, "Library", "Keychains", "login.keychain-db")
	cmd := exec.Command("security", "add-trusted-cert", "-r", "trustRoot", "-p", "ssl", "-k", keychain, rootPath)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("security add-trusted-cert: %w", err)
	}
	return nil
}

// ManualSteps tells people what to run where Devcorb does not install the CA itself.
func ManualSteps(rootPath string) string {
	switch runtime.GOOS {
	case "darwin":
		return fmt.Sprintf("security add-trusted-cert -r trustRoot -p ssl -k ~/Library/Keychains/login.keychain-db %s", rootPath)
	case "linux":
		return fmt.Sprintf("Debian/Ubuntu: sudo cp %[1]s /usr/local/share/ca-certificates/devcorb.crt && sudo update-ca-certificates; "+
			"Fedora: sudo cp %[1]s /etc/pki/ca-trust/source/anchors/devcorb.pem && sudo update-ca-trust", rootPath)
	}
	return "add " + rootPath + " to the system's trusted roots"
}

func parseChain(pemData []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for rest := pemData; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, errors.New("no certificate")
	}
	return out, nil
}
