package link

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/amalychev/devopus/internal/agent/paths"
)

// ServerConfig makes the agent an admin's server (R13, ADR-0019): it follows one
// workspace with the device token it got when it joined, not a person's sign-in, and
// runs only versions signed by people the admin trusts.
type ServerConfig struct {
	URL           string `json:"url"`
	WorkspaceID   string `json:"workspaceId"`
	WorkspaceName string `json:"workspaceName"`
	ServerID      string `json:"serverId"`
	Name          string `json:"name"`
	Token         string `json:"token"`
}

// ServerConfigFile is where it is kept: in the agent's data directory, readable by
// the agent alone.
func ServerConfigFile() string { return filepath.Join(paths.Home(), "server.json") }

// LoadServerConfig reads it; nil when this agent is not a server's.
func LoadServerConfig() (*ServerConfig, error) {
	b, err := os.ReadFile(ServerConfigFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c ServerConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// SaveServerConfig writes it.
func SaveServerConfig(c *ServerConfig) error {
	if err := paths.Ensure(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := ServerConfigFile() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ServerConfigFile())
}
