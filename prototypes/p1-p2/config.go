package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Subset of workspace.yaml v1 (specs/workspace-yaml.md) needed for P-1/P-2.
// Modules are folders of one repository (`path`); per-module `repo` is out of scope.

type Workspace struct {
	Version  int                `yaml:"version"`
	Project  string             `yaml:"project"`
	Domain   string             `yaml:"domain"`
	Services map[string]Service `yaml:"services"`
	Modules  map[string]Module  `yaml:"modules"`
	Features FeaturePolicy      `yaml:"features"`
}

type Service struct {
	Image  string            `yaml:"image"`
	Port   int               `yaml:"port"`   // TCP port other modules connect to
	Expose string            `yaml:"expose"` // web UI as <expose>.<env-host>
	UIPort int               `yaml:"ui_port"`
	Volume string            `yaml:"volume"` // data dir inside the container
	Env    map[string]string `yaml:"env"`
}

type Module struct {
	Path    string            `yaml:"path"`
	Image   string            `yaml:"image"`
	Build   string            `yaml:"build"` // Dockerfile dir relative to the repo root
	Port    int               `yaml:"port"`
	Routes  []string          `yaml:"routes"`
	Uses    []string          `yaml:"uses"`
	Env     map[string]string `yaml:"env"`
	Command string            `yaml:"command"`
	Hooks   struct {
		Migrate string `yaml:"migrate"`
	} `yaml:"hooks"`
}

type FeaturePolicy struct {
	Database  string `yaml:"database"`  // clone | shared
	Isolation string `yaml:"isolation"` // simple (contextual is out of scope)
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

func LoadWorkspace(path string) (*Workspace, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ws Workspace
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&ws); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	ws.applyDefaults()
	if err := ws.Validate(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &ws, nil
}

func (ws *Workspace) applyDefaults() {
	if ws.Features.Database == "" {
		ws.Features.Database = "clone"
	}
	if ws.Features.Isolation == "" {
		ws.Features.Isolation = "simple"
	}
	for name, m := range ws.Modules {
		if m.Port == 0 {
			m.Port = 80
		}
		ws.Modules[name] = m
	}
}

func (ws *Workspace) Validate(root string) error {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	if ws.Version != 1 {
		add("version: expected 1, got %d", ws.Version)
	}
	if !nameRe.MatchString(ws.Project) {
		add("project: %q must match %s", ws.Project, nameRe)
	}
	if !strings.HasSuffix(ws.Domain, ".localhost") {
		add("domain: %q must end with .localhost", ws.Domain)
	}
	if len(ws.Modules) == 0 {
		add("modules: at least one module is required")
	}
	if ws.Features.Database != "clone" && ws.Features.Database != "shared" {
		add("features.database: must be clone or shared")
	}
	if ws.Features.Isolation != "simple" {
		add("features.isolation: only simple is supported by this prototype")
	}

	for name, s := range ws.Services {
		if !nameRe.MatchString(name) {
			add("services.%s: invalid name", name)
		}
		if _, dup := ws.Modules[name]; dup {
			add("services.%s: name is also used by a module", name)
		}
		if s.Image == "" {
			add("services.%s.image: required", name)
		} else if !strings.Contains(s.Image, ":") || strings.HasSuffix(s.Image, ":latest") {
			add("services.%s.image: pin an explicit tag (got %q)", name, s.Image)
		}
		if s.Port == 0 {
			add("services.%s.port: required", name)
		}
		if s.Expose != "" && s.UIPort == 0 {
			add("services.%s.ui_port: required with expose", name)
		}
	}

	routeOwner := map[string]string{}
	for name, m := range ws.Modules {
		if !nameRe.MatchString(name) {
			add("modules.%s: invalid name", name)
		}
		if m.Path == "" {
			add("modules.%s.path: required", name)
		} else if st, err := os.Stat(filepath.Join(root, m.Path)); err != nil || !st.IsDir() {
			add("modules.%s.path: %q is not a directory", name, m.Path)
		}
		if (m.Image == "") == (m.Build == "") {
			add("modules.%s: set exactly one of image or build", name)
		}
		if m.Command == "" {
			add("modules.%s.command: required", name)
		}
		for _, r := range m.Routes {
			if !strings.HasPrefix(r, "/") {
				add("modules.%s.routes: %q must start with /", name, r)
			}
			if other, ok := routeOwner[r]; ok {
				add("modules.%s.routes: %q is already routed to %s", name, r, other)
			}
			routeOwner[r] = name
		}
		for _, u := range m.Uses {
			_, isMod := ws.Modules[u]
			_, isSvc := ws.Services[u]
			if !isMod && !isSvc {
				add("modules.%s.uses: unknown %q", name, u)
			}
			if u == name {
				add("modules.%s.uses: module uses itself", name)
			}
		}
		for k, v := range m.Env {
			if strings.Contains(strings.ToUpper(k), "API_URL") && strings.Contains(v, "://") {
				fmt.Fprintf(os.Stderr, "warning: modules.%s.env.%s is an absolute URL baked into the module; features will not route it\n", name, k)
			}
		}
	}

	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("invalid workspace:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

func (ws *Workspace) ModuleNames() []string  { return sortedKeys(ws.Modules) }
func (ws *Workspace) ServiceNames() []string { return sortedKeys(ws.Services) }

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
