// Package workspace is the model of workspace.yaml v1 (specs/workspace-yaml.md):
// loading, defaults, validation and ${{ }} expressions.
package workspace

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Version is the only format version this package reads and writes.
const Version = 1

// FileName is the conventional name of the project file.
const FileName = "workspace.yaml"

type Workspace struct {
	Version   int                 `yaml:"version"`
	Project   string              `yaml:"project"`
	Domain    string              `yaml:"domain"`
	Services  map[string]*Service `yaml:"services,omitempty"`
	Modules   map[string]*Module  `yaml:"modules"`
	Features  *FeaturePolicy      `yaml:"features,omitempty"`
	Overrides *OverridePolicy     `yaml:"overrides,omitempty"`
	Data      *DataPolicy         `yaml:"data,omitempty"`
}

// Service is an infrastructure dependency without code: a database, a cache, a mail catcher.
type Service struct {
	Image       string             `yaml:"image"`
	Port        int                `yaml:"port,omitempty"`    // TCP port other modules connect to
	Expose      string             `yaml:"expose,omitempty"`  // web UI as <expose>.<env host>
	UIPort      int                `yaml:"ui_port,omitempty"` // port of the web UI; defaults to port
	Data        *ServiceData       `yaml:"data,omitempty"`
	Env         map[string]string  `yaml:"env,omitempty"`
	Command     string             `yaml:"command,omitempty"`
	Entrypoint  string             `yaml:"entrypoint,omitempty"`
	Volumes     []string           `yaml:"volumes,omitempty"`
	Healthcheck *Healthcheck       `yaml:"healthcheck,omitempty"`
	Uses        []string           `yaml:"uses,omitempty"`
	Optional    bool               `yaml:"optional,omitempty"`
	Tmpfs       []string           `yaml:"tmpfs,omitempty"`
	Ulimits     map[string]*Ulimit `yaml:"ulimits,omitempty"`
	PerFeature  string             `yaml:"per_feature,omitempty"` // clone | shared | instance
}

type ServiceData struct {
	Seed string `yaml:"seed,omitempty"` // snapshot://name/tag | file://path | hook://module.hook
}

// Module is a unit of code: a repository or a folder of the main repository.
type Module struct {
	Repo    string `yaml:"repo,omitempty"`
	Path    string `yaml:"path,omitempty"`
	Branch  string `yaml:"branch,omitempty"`
	Runtime string `yaml:"runtime,omitempty"`
	Image   string `yaml:"image,omitempty"`
	Build   *Build `yaml:"build,omitempty"`
	// Mount is where the module's code appears in its container: /app on a runtime,
	// nowhere by default on an image or a build (the image carries its code).
	Mount       string             `yaml:"mount,omitempty"`
	Routes      []string           `yaml:"routes,omitempty"`
	Port        int                `yaml:"port,omitempty"`
	Uses        []string           `yaml:"uses,omitempty"`
	Env         map[string]string  `yaml:"env,omitempty"`
	Command     string             `yaml:"command,omitempty"`
	Entrypoint  string             `yaml:"entrypoint,omitempty"`
	Healthcheck *Healthcheck       `yaml:"healthcheck,omitempty"`
	Tmpfs       []string           `yaml:"tmpfs,omitempty"`
	Ulimits     map[string]*Ulimit `yaml:"ulimits,omitempty"`
	Optional    bool               `yaml:"optional,omitempty"`
	Hooks       *Hooks             `yaml:"hooks,omitempty"`
	Cron        []Cron             `yaml:"cron,omitempty"`
	Workers     []Worker           `yaml:"workers,omitempty"`
	Volumes     []string           `yaml:"volumes,omitempty"`
	// Ports is accepted only to warn: outside access goes through the proxy.
	Ports []any `yaml:"ports,omitempty"`
}

type Build struct {
	Context    string            `yaml:"context"`
	Dockerfile string            `yaml:"dockerfile,omitempty"`
	Target     string            `yaml:"target,omitempty"`
	Args       map[string]string `yaml:"args,omitempty"`
}

type Hooks struct {
	Install string `yaml:"install,omitempty"`
	Migrate string `yaml:"migrate,omitempty"`
	Seed    string `yaml:"seed,omitempty"`
	Build   string `yaml:"build,omitempty"`
}

type Cron struct {
	Schedule string `yaml:"schedule"`
	Command  string `yaml:"command"`
}

type Worker struct {
	Command  string `yaml:"command"`
	Replicas int    `yaml:"replicas,omitempty"`
}

// Healthcheck mirrors the compose healthcheck.
type Healthcheck struct {
	Test        Command `yaml:"test,omitempty"`
	Interval    string  `yaml:"interval,omitempty"`
	Timeout     string  `yaml:"timeout,omitempty"`
	Retries     int     `yaml:"retries,omitempty"`
	StartPeriod string  `yaml:"start_period,omitempty"`
	Disable     bool    `yaml:"disable,omitempty"`
}

// Command is a string or a list of strings, as in compose (`CMD-SHELL …` or `[CMD, …]`).
type Command []string

func (c *Command) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*c = Command{n.Value}
		return nil
	case yaml.SequenceNode:
		var list []string
		if err := n.Decode(&list); err != nil {
			return err
		}
		*c = list
		return nil
	}
	return fmt.Errorf("line %d: expected a string or a list of strings", n.Line)
}

func (c Command) MarshalYAML() (any, error) {
	if len(c) == 1 {
		return c[0], nil
	}
	return []string(c), nil
}

// Ulimit is a single number or {soft, hard}, as in compose.
type Ulimit struct {
	Single int `yaml:"-"`
	Soft   int `yaml:"soft,omitempty"`
	Hard   int `yaml:"hard,omitempty"`
}

func (u *Ulimit) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&u.Single)
	}
	type plain Ulimit
	return n.Decode((*plain)(u))
}

func (u Ulimit) MarshalYAML() (any, error) {
	if u.Single != 0 {
		return u.Single, nil
	}
	return map[string]int{"soft": u.Soft, "hard": u.Hard}, nil
}

type FeaturePolicy struct {
	Database     string `yaml:"database,omitempty"` // clone | shared
	SleepAfter   string `yaml:"sleep_after,omitempty"`
	Cleanup      string `yaml:"cleanup,omitempty"`   // on_merge | manual | after:<duration>
	Isolation    string `yaml:"isolation,omitempty"` // simple | contextual
	BranchPrefix string `yaml:"branch_prefix,omitempty"`
	MaxRunning   int    `yaml:"max_running,omitempty"`
}

type OverridePolicy struct {
	Allowed []string `yaml:"allowed,omitempty"`
}

type DataPolicy struct {
	Snapshots map[string]*Snapshot `yaml:"snapshots,omitempty"`
}

type Snapshot struct {
	Source string            `yaml:"source"`
	Mask   map[string]string `yaml:"mask,omitempty"`
}

// Defaults of specs/workspace-yaml.md.
const (
	DefaultBranch       = "main"
	DefaultModulePort   = 8080
	DefaultDatabase     = "clone"
	DefaultSleepAfter   = "2h"
	DefaultCleanup      = "on_merge"
	DefaultIsolation    = "simple"
	DefaultBranchPrefix = "feature/"
)

// Policy returns the feature policy with defaults filled in.
func (w *Workspace) Policy() FeaturePolicy {
	p := FeaturePolicy{}
	if w.Features != nil {
		p = *w.Features
	}
	if p.Database == "" {
		p.Database = DefaultDatabase
	}
	if p.SleepAfter == "" {
		p.SleepAfter = DefaultSleepAfter
	}
	if p.Cleanup == "" {
		p.Cleanup = DefaultCleanup
	}
	if p.Isolation == "" {
		p.Isolation = DefaultIsolation
	}
	if p.BranchPrefix == "" {
		p.BranchPrefix = DefaultBranchPrefix
	}
	return p
}

// ModulePort is the port the module listens on inside its container.
func (m *Module) ModulePort() int {
	if m.Port != 0 {
		return m.Port
	}
	return DefaultModulePort
}

// DefaultMount is where a module on a platform runtime finds its code.
const DefaultMount = "/app"

// CodeMount is where the module's code is mounted in its container; empty: nowhere.
func (m *Module) CodeMount() string {
	if m.Mount != "" {
		return m.Mount
	}
	if m.Runtime != "" {
		return DefaultMount
	}
	return ""
}

// BaseBranch is the branch the base environment runs.
func (m *Module) BaseBranch() string {
	if m.Branch != "" {
		return m.Branch
	}
	return DefaultBranch
}

// ExposePort is the port of the service's web UI.
func (s *Service) ExposePort() int {
	if s.UIPort != 0 {
		return s.UIPort
	}
	return s.Port
}

func (w *Workspace) ModuleNames() []string  { return sortedKeys(w.Modules) }
func (w *Workspace) ServiceNames() []string { return sortedKeys(w.Services) }

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// runtimeImages are official images whose name differs from the runtime's.
var runtimeImages = map[string]string{
	"go":     "golang",
	"java":   "eclipse-temurin",
	"dotnet": "mcr.microsoft.com/dotnet/sdk",
	"bun":    "oven/bun",
	"deno":   "denoland/deno",
}

// RuntimeImage is the image a platform runtime runs on. Until Devopus ships its own
// runtime images (with ext-opentelemetry for PHP), it is the official image of the
// language: php:8.3 → php:8.3, go:1.25 → golang:1.25.
func RuntimeImage(runtime string) string {
	lang, version, _ := strings.Cut(runtime, ":")
	if img, ok := runtimeImages[lang]; ok {
		lang = img
	}
	return lang + ":" + version
}

// DatabaseName is the database the base environment uses in a service: the one the
// image creates on first start, else the project name.
func (w *Workspace) DatabaseName(service string) string {
	for _, k := range []string{"POSTGRES_DB", "MYSQL_DATABASE", "MARIADB_DATABASE", "MONGO_INITDB_DATABASE"} {
		if v := w.Services[service].Env[k]; v != "" && !HasExpr(v) {
			return v
		}
	}
	return strings.ReplaceAll(w.Project, "-", "_")
}
