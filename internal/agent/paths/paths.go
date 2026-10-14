// Package paths locates the agent's files: state, socket, logs.
package paths

import (
	"os"
	"path/filepath"
)

// Home is the agent's data directory: $DOCKLET_HOME or ~/.docklet.
func Home() string {
	if h := os.Getenv("DOCKLET_HOME"); h != "" {
		return h
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ".docklet"
	}
	return filepath.Join(h, ".docklet")
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
