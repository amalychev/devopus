// Package service registers a Devopus component — the agent or the local panel — as a
// per-user service that starts at login and restarts after a crash: a launchd agent
// on macOS, a systemd user unit on Linux (docs/architecture/agent.md). Nothing here
// needs root.
package service

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"

	"github.com/devopus/devopus/internal/agent/paths"
)

// ErrUnsupported: this OS has no service manager Devopus knows.
var ErrUnsupported = errors.New("registering the agent as a service is supported on macOS and Linux")

// Components that run as services.
const (
	Agent = "agent"
	Panel = "panel"
)

// Service is a component's service definition for this user and data directory.
type Service struct {
	// Component is Agent or Panel; the service runs `devopus <component> run`.
	Component string
	// Label names the service: com.devopus.agent, with the instance suffix for a
	// non-default $DEVOPUS_HOME so a test agent never replaces the real one.
	Label string
	// Exe is the devopus binary the service runs.
	Exe  string
	Home string
	Log  string
	// Env is passed to the agent: DEVOPUS_HOME, and PATH and the Docker variables
	// from the shell that installed it (launchd and systemd start with a bare PATH,
	// and the agent runs git and docker build).
	Env map[string]string
}

// For describes the service of a component for this binary and data directory.
func For(component string) (*Service, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	log := paths.Log()
	switch component {
	case Agent:
	case Panel:
		log = paths.PanelLog()
	default:
		return nil, fmt.Errorf("unknown component %q", component)
	}
	label := "com.devopus." + component
	if !paths.IsDefault() {
		label += "." + paths.Instance()
	}
	env := map[string]string{"DEVOPUS_HOME": paths.Home()}
	for _, k := range []string{"PATH", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG"} {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	return &Service{Component: component, Label: label, Exe: exe, Home: paths.Home(), Log: log, Env: env}, nil
}

// File is where the service definition lives.
func (s *Service) File() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", s.Label+".plist"), nil
	case "linux":
		dir := os.Getenv("XDG_CONFIG_HOME")
		if dir == "" {
			dir = filepath.Join(home, ".config")
		}
		return filepath.Join(dir, "systemd", "user", s.unit()), nil
	}
	return "", ErrUnsupported
}

func (s *Service) unit() string { return strings.ReplaceAll(s.Label, ".", "-") + ".service" }

// Installed reports whether the component's service definition exists.
func Installed(component string) bool {
	s, err := For(component)
	if err != nil {
		return false
	}
	f, err := s.File()
	if err != nil {
		return false
	}
	_, err = os.Stat(f)
	return err == nil
}

// Definition renders the launchd plist or the systemd unit.
func (s *Service) Definition() ([]byte, error) {
	tpl := unitTemplate
	if runtime.GOOS == "darwin" {
		tpl = plistTemplate
	}
	var b bytes.Buffer
	err := template.Must(template.New("").Funcs(template.FuncMap{"xml": xmlEscape, "q": systemdQuote}).Parse(tpl)).Execute(&b, s)
	return b.Bytes(), err
}

// Install writes the definition and starts the service. An existing definition is
// replaced: reinstalling after moving the binary fixes the path.
func (s *Service) Install() error {
	file, err := s.File()
	if err != nil {
		return err
	}
	def, err := s.Definition()
	if err != nil {
		return err
	}
	if err := paths.Ensure(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Log), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		_ = launchctl("bootout", s.target()) // a previous definition, if loaded
		if err := os.WriteFile(file, def, 0o644); err != nil {
			return err
		}
		return launchctl("bootstrap", s.domain(), file)
	case "linux":
		if err := os.WriteFile(file, def, 0o644); err != nil {
			return err
		}
		if err := systemctl("daemon-reload"); err != nil {
			return err
		}
		return systemctl("enable", "--now", s.unit())
	}
	return ErrUnsupported
}

// Start starts an installed service that is not running.
func (s *Service) Start() error {
	switch runtime.GOOS {
	case "darwin":
		if err := launchctl("kickstart", s.target()); err == nil {
			return nil
		}
		// Not loaded (booted out by hand): load it again.
		file, err := s.File()
		if err != nil {
			return err
		}
		return launchctl("bootstrap", s.domain(), file)
	case "linux":
		return systemctl("start", s.unit())
	}
	return ErrUnsupported
}

// Restart stops the running component and starts it again, after an upgrade.
func (s *Service) Restart() error {
	switch runtime.GOOS {
	case "darwin":
		return launchctl("kickstart", "-k", s.target())
	case "linux":
		return systemctl("restart", s.unit())
	}
	return ErrUnsupported
}

// Uninstall stops the service and removes its definition. Missing pieces are fine.
func (s *Service) Uninstall() error {
	file, err := s.File()
	if err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		_ = launchctl("bootout", s.target())
	case "linux":
		_ = systemctl("disable", "--now", s.unit())
	}
	if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if runtime.GOOS == "linux" {
		_ = systemctl("daemon-reload")
	}
	return nil
}

func (s *Service) domain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }
func (s *Service) target() string { return s.domain() + "/" + s.Label }

func launchctl(args ...string) error { return run("launchctl", args...) }

func systemctl(args ...string) error { return run("systemctl", append([]string{"--user"}, args...)...) }

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// systemdQuote quotes a value for ExecStart= and Environment=; % is systemd's
// specifier character.
func systemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%")
	return `"` + r.Replace(s) + `"`
}

// KeepAlive restarts the agent when it crashes but not when it exits cleanly
// (SIGTERM from bootout, or another agent already holds the lock and it gives up).
const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{xml .Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{xml .Exe}}</string>
		<string>{{xml .Component}}</string>
		<string>run</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
{{- range $k, $v := .Env}}
		<key>{{xml $k}}</key>
		<string>{{xml $v}}</string>
{{- end}}
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>5</integer>
	<key>StandardOutPath</key>
	<string>{{xml .Log}}</string>
	<key>StandardErrorPath</key>
	<string>{{xml .Log}}</string>
</dict>
</plist>
`

const unitTemplate = `[Unit]
Description=Devopus {{.Component}}
Documentation=https://github.com/devopus/devopus

[Service]
ExecStart={{q .Exe}} {{.Component}} run
{{- range $k, $v := .Env}}
Environment={{q (printf "%s=%s" $k $v)}}
{{- end}}
Restart=on-failure
RestartSec=2
StandardOutput=append:{{.Log}}
StandardError=append:{{.Log}}

[Install]
WantedBy=default.target
`
