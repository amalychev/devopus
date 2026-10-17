package selfhost

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/amalychev/devopus/internal/agent/paths"
)

// The agent of an admin's server (R13, ADR-0019): a system service that runs the
// projects the workspace gives the server. It runs as root — it drives Docker and
// serves 80/443 — with its own data directory; its device token is in server.json
// there, readable by root alone.
const (
	AgentUnitFile = "/etc/systemd/system/devopus-agent.service"
	AgentService  = "devopus-agent"
	AgentDataDir  = paths.ServerHome
)

// AgentConfigFile is the server's link: workspace, address and device token.
func AgentConfigFile() string { return AgentDataDir + "/server.json" }

// AgentUnit is the systemd unit of a server's agent.
func AgentUnit(binary string) string {
	return fmt.Sprintf(`[Unit]
Description=Devopus agent of this server
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
Environment=DEVOPUS_HOME=%s
ExecStart=%s agent run
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
`, AgentDataDir, binary)
}

// AgentOptions install a server's agent.
type AgentOptions struct {
	Binary string
	// Config is what the server got when it joined (link.ServerConfig as JSON).
	Config any
	Log    interface{ Write([]byte) (int, error) }
}

// InstallAgent makes this machine a server of the workspace: its link written, the
// service installed, enabled and started. Running it again replaces the link.
func InstallAgent(ctx context.Context, h Host, o AgentOptions) error {
	say := func(format string, a ...any) {
		if o.Log != nil {
			fmt.Fprintf(o.Log, format+"\n", a...)
		}
	}
	if o.Binary == "" || !strings.HasPrefix(o.Binary, "/") {
		return fmt.Errorf("the binary must be an absolute path: %q", o.Binary)
	}
	if _, err := h.Run(ctx, "docker", "version", "--format", "{{.Server.Version}}"); err != nil {
		return fmt.Errorf("docker is needed: the agent runs the projects in containers (%v)", err)
	}
	cfg, err := json.MarshalIndent(o.Config, "", "  ")
	if err != nil {
		return err
	}
	say("Data directory %s", AgentDataDir)
	if err := h.MkdirAll(AgentDataDir, 0o700); err != nil {
		return err
	}
	if err := h.WriteFile(AgentConfigFile(), append(cfg, '\n'), 0o600); err != nil {
		return err
	}
	say("Service %s", AgentUnitFile)
	if err := h.WriteFile(AgentUnitFile, []byte(AgentUnit(o.Binary)), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", AgentService}, {"restart", AgentService}} {
		if _, err := h.Run(ctx, "systemctl", args...); err != nil {
			return err
		}
	}
	return nil
}

// UninstallAgent stops and removes the service; purge removes its data directory —
// the environments' data included — too.
func UninstallAgent(ctx context.Context, h Host, purge bool) error {
	_, _ = h.Run(ctx, "systemctl", "disable", "--now", AgentService)
	if err := h.Remove(AgentUnitFile); err != nil {
		return err
	}
	if _, err := h.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if purge {
		return h.Remove(AgentDataDir)
	}
	return nil
}
