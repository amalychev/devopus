// Package spec is the model of a project (docs/product/glossary.md): its applications —
// usually one git repository each — and its services, each living in the project and
// attached to one or more of its applications. Agents run environments from it; the
// workspace server will keep it (ADR-0013).
package spec

import (
	"fmt"
	"sort"
	"strings"

	"github.com/amalychev/devopus/internal/catalog"
)

// Project is a product of the workspace: Google Drive, an online shop.
type Project struct {
	Name   string `yaml:"name"`
	Domain string `yaml:"domain"`
	// Apps are the parts of the product: frontend, backend, admin.
	Apps map[string]*App `yaml:"apps"`
	// Services live in the project and are attached to applications.
	Services  map[string]*Service `yaml:"services,omitempty"`
	Features  *FeaturePolicy      `yaml:"features,omitempty"`
	Overrides *OverridePolicy     `yaml:"overrides,omitempty"`
	Data      *DataPolicy         `yaml:"data,omitempty"`
}

// App is an application: its code, how it is served and prepared.
type App struct {
	// Repo is the git repository; Path a folder of the project's own repository.
	Repo   string `yaml:"repo,omitempty"`
	Path   string `yaml:"path,omitempty"`
	Branch string `yaml:"branch,omitempty"`
	// Image or Build when no runtime service is attached.
	Image string `yaml:"image,omitempty"`
	Build *Build `yaml:"build,omitempty"`
	// Mount is where the code appears in the container.
	Mount  string   `yaml:"mount,omitempty"`
	Routes []string `yaml:"routes,omitempty"`
	Port   int      `yaml:"port,omitempty"`
	// Uses are the other applications it calls.
	Uses        []string           `yaml:"uses,omitempty"`
	Env         map[string]string  `yaml:"env,omitempty"`
	Command     string             `yaml:"command,omitempty"`
	Entrypoint  string             `yaml:"entrypoint,omitempty"`
	Healthcheck *Healthcheck       `yaml:"healthcheck,omitempty"`
	Tmpfs       []string           `yaml:"tmpfs,omitempty"`
	Ulimits     map[string]*Ulimit `yaml:"ulimits,omitempty"`
	Optional    bool               `yaml:"optional,omitempty"`
	Hooks       *Hooks             `yaml:"hooks,omitempty"`
	Volumes     []string           `yaml:"volumes,omitempty"`
}

// Service is a runtime, a container or a background process of the project.
type Service struct {
	// Type is a catalog type (php, mysql, queue…) or catalog.Custom.
	Type    string `yaml:"type"`
	Version string `yaml:"version,omitempty"`
	// Image overrides the catalog's image; a custom container needs it.
	Image string `yaml:"image,omitempty"`
	// Apps are the applications the service is attached to.
	Apps []string `yaml:"apps,omitempty"`
	// Uses are the other services a container service needs.
	Uses []string `yaml:"uses,omitempty"`

	Port        int                `yaml:"port,omitempty"`
	UIPort      int                `yaml:"ui_port,omitempty"`
	Expose      string             `yaml:"expose,omitempty"`
	Data        *ServiceData       `yaml:"data,omitempty"`
	Env         map[string]string  `yaml:"env,omitempty"`
	Command     string             `yaml:"command,omitempty"`
	Entrypoint  string             `yaml:"entrypoint,omitempty"`
	Volumes     []string           `yaml:"volumes,omitempty"`
	Healthcheck *Healthcheck       `yaml:"healthcheck,omitempty"`
	Tmpfs       []string           `yaml:"tmpfs,omitempty"`
	Ulimits     map[string]*Ulimit `yaml:"ulimits,omitempty"`
	PerFeature  string             `yaml:"per_feature,omitempty"` // clone | shared | instance
	Optional    bool               `yaml:"optional,omitempty"`
	// Replicas of a queue; Schedule of a cronjob.
	Replicas int    `yaml:"replicas,omitempty"`
	Schedule string `yaml:"schedule,omitempty"`
}

type ServiceData struct {
	Seed string `yaml:"seed,omitempty"`
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

// Healthcheck mirrors the compose healthcheck; Test is CMD/CMD-SHELL/NONE or a command.
type Healthcheck struct {
	Test        []string `yaml:"test,omitempty"`
	Interval    string   `yaml:"interval,omitempty"`
	Timeout     string   `yaml:"timeout,omitempty"`
	Retries     int      `yaml:"retries,omitempty"`
	StartPeriod string   `yaml:"start_period,omitempty"`
	Disable     bool     `yaml:"disable,omitempty"`
}

// Ulimit is a single number (Single) or soft and hard limits.
type Ulimit struct {
	Single int `yaml:"single,omitempty"`
	Soft   int `yaml:"soft,omitempty"`
	Hard   int `yaml:"hard,omitempty"`
}

type FeaturePolicy struct {
	Database     string `yaml:"database,omitempty"` // clone | shared
	SleepAfter   string `yaml:"sleep_after,omitempty"`
	Cleanup      string `yaml:"cleanup,omitempty"`
	Isolation    string `yaml:"isolation,omitempty"`
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

// Defaults.
const (
	DefaultBranch       = "main"
	DefaultAppPort      = 8080
	DefaultMount        = "/app"
	DefaultDatabase     = "clone"
	DefaultSleepAfter   = "2h"
	DefaultCleanup      = "on_merge"
	DefaultIsolation    = "simple"
	DefaultBranchPrefix = "feature/"
)

// AppNames and ServiceNames are sorted.
func (p *Project) AppNames() []string     { return sortedKeys(p.Apps) }
func (p *Project) ServiceNames() []string { return sortedKeys(p.Services) }

// Kind is how a service runs: its catalog type's kind, Container for unknown types.
func (s *Service) Kind() catalog.Kind {
	if t, ok := catalog.Lookup(s.Type); ok {
		return t.Kind
	}
	return catalog.Container
}

// IsContainer reports whether the service runs as its own container.
func (s *Service) IsContainer() bool { return s.Kind() == catalog.Container }

// ImageRef is the image of a container or runtime service.
func (s *Service) ImageRef() string {
	if s.Image != "" {
		return s.Image
	}
	if t, ok := catalog.Lookup(s.Type); ok {
		return t.Image(s.Version)
	}
	return ""
}

// ExposePort is the port of the service's web UI.
func (s *Service) ExposePort() int {
	if s.UIPort != 0 {
		return s.UIPort
	}
	return s.Port
}

// Attached are the services attached to an application, sorted.
func (p *Project) Attached(app string) []string {
	var out []string
	for _, n := range p.ServiceNames() {
		if contains(p.Services[n].Apps, app) {
			out = append(out, n)
		}
	}
	return out
}

// Runtime is the runtime service of an application, if one is attached.
func (p *Project) Runtime(app string) (string, *Service) {
	for _, n := range p.Attached(app) {
		if s := p.Services[n]; s.Kind() == catalog.Runtime {
			return n, s
		}
	}
	return "", nil
}

// Processes are the queue and cronjob services of an application, sorted.
func (p *Project) Processes(app, typ string) []string {
	var out []string
	for _, n := range p.Attached(app) {
		if p.Services[n].Type == typ {
			out = append(out, n)
		}
	}
	return out
}

// Needs is the dependency graph the engine walks, with applications and container
// services in one name space: an application needs the applications it uses and the
// container services attached to it; a container service needs the services it uses.
func (p *Project) Needs(name string) []string {
	if a := p.Apps[name]; a != nil {
		out := append([]string{}, a.Uses...)
		for _, n := range p.Attached(name) {
			if p.Services[n].IsContainer() {
				out = append(out, n)
			}
		}
		return out
	}
	if s := p.Services[name]; s != nil {
		return s.Uses
	}
	return nil
}

// AppPort is the port the application listens on inside its container.
func (a *App) AppPort() int {
	if a.Port != 0 {
		return a.Port
	}
	return DefaultAppPort
}

// CodeMount is where the application's code is mounted; empty: nowhere.
func (p *Project) CodeMount(app string) string {
	a := p.Apps[app]
	if a.Mount != "" {
		return a.Mount
	}
	if _, rt := p.Runtime(app); rt != nil {
		return DefaultMount
	}
	return ""
}

// BaseBranch is the branch the base environment runs.
func (a *App) BaseBranch() string {
	if a.Branch != "" {
		return a.Branch
	}
	return DefaultBranch
}

// Policy returns the feature policy with defaults filled in.
func (p *Project) Policy() FeaturePolicy {
	f := FeaturePolicy{}
	if p.Features != nil {
		f = *p.Features
	}
	if f.Database == "" {
		f.Database = DefaultDatabase
	}
	if f.SleepAfter == "" {
		f.SleepAfter = DefaultSleepAfter
	}
	if f.Cleanup == "" {
		f.Cleanup = DefaultCleanup
	}
	if f.Isolation == "" {
		f.Isolation = DefaultIsolation
	}
	if f.BranchPrefix == "" {
		f.BranchPrefix = DefaultBranchPrefix
	}
	return f
}

// DatabaseName is the database the base environment uses in a service: the one the
// image creates on first start, else the project name.
func (p *Project) DatabaseName(service string) string {
	for _, k := range []string{"POSTGRES_DB", "MYSQL_DATABASE", "MARIADB_DATABASE", "MONGO_INITDB_DATABASE"} {
		if v := p.Services[service].Env[k]; v != "" && !HasExpr(v) {
			return v
		}
	}
	return strings.ReplaceAll(p.Name, "-", "_")
}

// Validate checks what the engine relies on. The workspace server validates more.
func (p *Project) Validate() error {
	var errs []string
	for _, n := range p.ServiceNames() {
		s := p.Services[n]
		if _, ok := catalog.Lookup(s.Type); !ok {
			errs = append(errs, fmt.Sprintf("services.%s: unknown type %q (%s)", n, s.Type, strings.Join(catalog.Names(), ", ")))
			continue
		}
		if p.Apps[n] != nil {
			errs = append(errs, fmt.Sprintf("services.%s: an application has the same name", n))
		}
		for _, a := range s.Apps {
			if p.Apps[a] == nil {
				errs = append(errs, fmt.Sprintf("services.%s: attached to unknown application %q", n, a))
			}
		}
		switch s.Kind() {
		case catalog.Process:
			if len(s.Apps) != 1 {
				errs = append(errs, fmt.Sprintf("services.%s: a %s runs the code of exactly one application", n, s.Type))
			}
			if s.Command == "" {
				errs = append(errs, fmt.Sprintf("services.%s: a %s needs a command", n, s.Type))
			}
		case catalog.Container:
			if s.ImageRef() == "" {
				errs = append(errs, fmt.Sprintf("services.%s: needs an image", n))
			}
		}
	}
	for _, n := range p.AppNames() {
		var runtimes []string
		for _, s := range p.Attached(n) {
			if p.Services[s].Kind() == catalog.Runtime {
				runtimes = append(runtimes, s)
			}
		}
		if len(runtimes) > 1 {
			errs = append(errs, fmt.Sprintf("apps.%s: one runtime per application, got %s", n, strings.Join(runtimes, ", ")))
		}
		for _, u := range p.Apps[n].Uses {
			if p.Apps[u] == nil {
				errs = append(errs, fmt.Sprintf("apps.%s: uses unknown application %q", n, u))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
