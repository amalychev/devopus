// Package paths locates the agent's files: state, socket, logs.
package paths

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
)

// Home is the agent's data directory: $DEVOPUS_HOME or ~/.devopus.
func Home() string {
	if h := os.Getenv("DEVOPUS_HOME"); h != "" {
		return h
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ".devopus"
	}
	return filepath.Join(h, ".devopus")
}

// Socket is the unix socket of the local API.
func Socket() string { return filepath.Join(Home(), "agent.sock") }

// State is the desired state file.
func State() string { return filepath.Join(Home(), "state.json") }

// Lock guarantees one agent per data directory.
func Lock() string { return filepath.Join(Home(), "agent.lock") }

// Log is where an agent started in the background writes.
func Log() string { return filepath.Join(Home(), "agent.log") }

// Ensure creates the data directory, readable only by the user.
func Ensure() error { return os.MkdirAll(Home(), 0o700) }

// CA is the local certificate authority: root.pem is what people trust.
func CA() string { return filepath.Join(Home(), "ca") }

// Certs keeps the issued server certificates, one pair per environment.
func Certs() string { return filepath.Join(Home(), "certs") }

// Proxy keeps the proxy's control material and its xDS port.
func Proxy() string { return filepath.Join(Home(), "proxy") }

// Instance tells agents with different data directories apart in Docker labels and
// names. The default directory gets the plain names (devopus-proxy).
func Instance() string {
	sum := sha256.Sum256([]byte(Home()))
	return hex.EncodeToString(sum[:])[:8]
}

// IsDefault reports whether the agent uses ~/.devopus.
func IsDefault() bool {
	h, err := os.UserHomeDir()
	return err == nil && Home() == filepath.Join(h, ".devopus")
}

// ProxyName is the Envoy container of this instance.
func ProxyName() string {
	if IsDefault() {
		return "devopus-proxy"
	}
	return "devopus-proxy-" + Instance()
}

// Sources keeps the checkouts of modules from other repositories:
// src/<project>/<environment>/<module>.
func Sources() string { return filepath.Join(Home(), "src") }

// Envs keeps per-environment files: hook markers and logs of hooks and builds.
func Envs() string { return filepath.Join(Home(), "envs") }

// PanelPort and PanelToken are where the agent keeps the local panel's address and
// the token `devopus ui` opens it with.
func PanelPort() string { return filepath.Join(Home(), "panel-port") }

func PanelToken() string { return filepath.Join(Home(), "panel-token") }
