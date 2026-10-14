package compose

import (
	"bytes"
	"fmt"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/docklet/docklet/internal/workspace"
)

// Export renders the base environment as a docker-compose file: the "no lock-in"
// promise of specs/workspace-yaml.md. There is no proxy, so modules with routes are
// published on 127.0.0.1 one port each, and ${{ }} expressions become plain values.
// Secrets become ${NAME} for `docker compose --env-file .env.secrets`.
func Export(w *workspace.Workspace) ([]byte, error) {
	x := &exporter{w: w, hostPort: map[string]int{}}
	x.assignHostPorts()

	out := composeFile{Name: w.Project, Services: map[string]*composeService{}}
	volumes := map[string]bool{}
	for _, name := range w.ServiceNames() {
		s, err := x.service(name, w.Services[name])
		if err != nil {
			return nil, err
		}
		out.Services[name] = s
		collectVolumes(volumes, s.Volumes)
	}
	for _, name := range w.ModuleNames() {
		svcs, err := x.module(name, w.Modules[name])
		if err != nil {
			return nil, err
		}
		for n, s := range svcs {
			if _, clash := out.Services[n]; clash {
				return nil, fmt.Errorf("exported service name %q is taken", n)
			}
			out.Services[n] = s
			collectVolumes(volumes, s.Volumes)
		}
	}
	if len(volumes) > 0 {
		out.Volumes = map[string]struct{}{}
		for v := range volumes {
			out.Volumes[v] = struct{}{}
		}
	}

	var buf bytes.Buffer
	buf.WriteString(x.header())
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type composeFile struct {
	Name     string                     `yaml:"name"`
	Services map[string]*composeService `yaml:"services"`
	Volumes  map[string]struct{}        `yaml:"volumes,omitempty"`
}

type composeService struct {
	Image       string                       `yaml:"image,omitempty"`
	Build       *composeBuild                `yaml:"build,omitempty"`
	Profiles    []string                     `yaml:"profiles,omitempty"`
	Entrypoint  string                       `yaml:"entrypoint,omitempty"`
	Command     any                          `yaml:"command,omitempty"`
	WorkingDir  string                       `yaml:"working_dir,omitempty"`
	Environment map[string]string            `yaml:"environment,omitempty"`
	Ports       []string                     `yaml:"ports,omitempty"`
	Volumes     []string                     `yaml:"volumes,omitempty"`
	DependsOn   []string                     `yaml:"depends_on,omitempty"`
	Healthcheck *workspace.Healthcheck       `yaml:"healthcheck,omitempty"`
	Tmpfs       []string                     `yaml:"tmpfs,omitempty"`
	Ulimits     map[string]*workspace.Ulimit `yaml:"ulimits,omitempty"`
	Init        bool                         `yaml:"init,omitempty"`
	Deploy      *composeDeploy               `yaml:"deploy,omitempty"`
}

type composeBuild struct {
	Context    string            `yaml:"context"`
	Dockerfile string            `yaml:"dockerfile,omitempty"`
	Target     string            `yaml:"target,omitempty"`
	Args       map[string]string `yaml:"args,omitempty"`
}

type composeDeploy struct {
	Replicas int `yaml:"replicas"`
}

type exporter struct {
	w        *workspace.Workspace
	hostPort map[string]int // module or exposed service → port on 127.0.0.1
	rootPort int            // port of the module serving "/": the URL of the environment
	notes    []string
}

// assignHostPorts gives every routed module and exposed service its own local port,
// starting at 8080 with the module that serves "/".
func (x *exporter) assignHostPorts() {
	var names []string
	for _, n := range x.w.ModuleNames() {
		m := x.w.Modules[n]
		switch {
		case slices.Contains(m.Routes, "/"):
			names = append([]string{n}, names...)
		case len(m.Routes) > 0:
			names = append(names, n)
		}
	}
	for _, n := range x.w.ServiceNames() {
		if x.w.Services[n].Expose != "" {
			names = append(names, n)
		}
	}
	for i, n := range names {
		x.hostPort[n] = 8080 + i
	}
	if len(names) > 0 && x.w.Modules[names[0]] != nil {
		x.rootPort = x.hostPort[names[0]]
	}
}

func (x *exporter) service(name string, s *workspace.Service) (*composeService, error) {
	env, err := x.env(s.Env)
	if err != nil {
		return nil, fmt.Errorf("services.%s: %w", name, err)
	}
	cmd, err := x.expand(s.Command)
	if err != nil {
		return nil, fmt.Errorf("services.%s.command: %w", name, err)
	}
	out := &composeService{
		Image:       s.Image,
		Entrypoint:  s.Entrypoint,
		Command:     emptyNil(cmd),
		Environment: env,
		Volumes:     s.Volumes,
		DependsOn:   s.Uses,
		Healthcheck: s.Healthcheck,
		Tmpfs:       s.Tmpfs,
		Ulimits:     s.Ulimits,
	}
	if s.Optional {
		out.Profiles = []string{"optional"}
	}
	if p, ok := x.hostPort[name]; ok {
		out.Ports = []string{fmt.Sprintf("127.0.0.1:%d:%d", p, s.ExposePort())}
	}
	if s.Data != nil && s.Data.Seed != "" {
		x.notes = append(x.notes, fmt.Sprintf("%s: data seed %s is applied by Docklet only", name, s.Data.Seed))
	}
	return out, nil
}

// module returns the module's service and one service per worker.
func (x *exporter) module(name string, m *workspace.Module) (map[string]*composeService, error) {
	env, err := x.env(m.Env)
	if err != nil {
		return nil, fmt.Errorf("modules.%s: %w", name, err)
	}
	dir := moduleDir(name, m)
	base := composeService{
		Image:       m.Image,
		Entrypoint:  m.Entrypoint,
		Environment: env,
		DependsOn:   m.Uses,
		Healthcheck: m.Healthcheck,
		Tmpfs:       m.Tmpfs,
		Ulimits:     m.Ulimits,
		Volumes:     m.Volumes,
		Init:        true,
	}
	if m.Optional {
		base.Profiles = []string{"optional"}
	}
	switch {
	case m.Build != nil:
		ctx := m.Build.Context
		if m.Repo != "" {
			ctx = path.Join(dir, ctx)
		}
		base.Build = &composeBuild{Context: dotSlash(ctx), Dockerfile: m.Build.Dockerfile, Target: m.Build.Target, Args: m.Build.Args}
	case m.Runtime != "":
		// Platform runtime images arrive with the agent; the official image is the
		// closest stand-in. Code is mounted from the checkout.
		base.Image = m.Runtime
		base.WorkingDir = "/app"
		base.Volumes = append([]string{dotSlash(dir) + ":/app"}, base.Volumes...)
	}
	if m.Repo != "" {
		x.notes = append(x.notes, fmt.Sprintf("%s: clone %s into ./%s", name, m.Repo, dir))
	}

	main := base
	cmd, err := x.expand(m.Command)
	if err != nil {
		return nil, fmt.Errorf("modules.%s.command: %w", name, err)
	}
	var prelude []string
	if m.Hooks != nil {
		for _, h := range []string{m.Hooks.Install, m.Hooks.Migrate} {
			if h != "" {
				prelude = append(prelude, h)
			}
		}
	}
	switch {
	case len(prelude) > 0 && cmd != "":
		main.Command = []string{"sh", "-c", strings.Join(append(prelude, cmd), " && ")}
	case len(prelude) > 0:
		x.notes = append(x.notes, fmt.Sprintf("%s: hooks (%s) need a command to run before; run them by hand", name, strings.Join(prelude, "; ")))
	default:
		main.Command = emptyNil(cmd)
	}
	if p, ok := x.hostPort[name]; ok {
		main.Ports = []string{fmt.Sprintf("127.0.0.1:%d:%d", p, m.ModulePort())}
	}
	out := map[string]*composeService{name: &main}

	for i, wk := range m.Workers {
		s := base
		c, err := x.expand(wk.Command)
		if err != nil {
			return nil, fmt.Errorf("modules.%s.workers[%d]: %w", name, i, err)
		}
		s.Command = c
		s.Healthcheck = nil
		s.DependsOn = append(append([]string{}, m.Uses...), name)
		if wk.Replicas > 1 {
			s.Deploy = &composeDeploy{Replicas: wk.Replicas}
		}
		out[fmt.Sprintf("%s-worker-%d", name, i+1)] = &s
	}
	if len(m.Cron) > 0 {
		x.notes = append(x.notes, fmt.Sprintf("%s: %d cron entries are run by Docklet only", name, len(m.Cron)))
	}
	if len(m.Routes) > 1 || (len(m.Routes) == 1 && m.Routes[0] != "/") {
		x.notes = append(x.notes, fmt.Sprintf("%s: serves %s on one host behind Docklet; here it has its own port", name, strings.Join(m.Routes, ", ")))
	}
	return out, nil
}

func (x *exporter) env(env map[string]string) (map[string]string, error) {
	if len(env) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for k, v := range env {
		e, err := x.expand(v)
		if err != nil {
			return nil, fmt.Errorf("env.%s: %w", k, err)
		}
		out[k] = e
	}
	return out, nil
}

// expand resolves ${{ }} for the base environment without a proxy.
func (x *exporter) expand(s string) (string, error) {
	return x.w.Expand(s, func(r workspace.Ref) (string, error) {
		switch r.Kind {
		case workspace.RefSecret:
			return "${" + r.Name + "}", nil
		case workspace.RefEnv:
			switch r.Attr {
			case "url":
				if x.rootPort == 0 {
					return "http://localhost", nil
				}
				return "http://localhost:" + strconv.Itoa(x.rootPort), nil
			default: // slug, id
				return "main", nil
			}
		case workspace.RefModule:
			m := x.w.Modules[r.Name]
			switch r.Attr {
			case "url":
				return fmt.Sprintf("http://%s:%d", r.Name, m.ModulePort()), nil
			case "host":
				return r.Name, nil
			default:
				return strconv.Itoa(m.ModulePort()), nil
			}
		case workspace.RefService:
			s := x.w.Services[r.Name]
			switch r.Attr {
			case "host":
				return r.Name, nil
			case "port":
				if s.Port == 0 {
					return "", fmt.Errorf("%s has no port", r.Name)
				}
				return strconv.Itoa(s.Port), nil
			default: // database
				return databaseName(x.w, r.Name), nil
			}
		}
		return "", fmt.Errorf("cannot export %s", r.Raw)
	})
}

// databaseName is the database the base environment uses in a service: the one the
// image creates on first start, else the project name.
func databaseName(w *workspace.Workspace, service string) string {
	for _, k := range []string{"POSTGRES_DB", "MYSQL_DATABASE", "MARIADB_DATABASE", "MONGO_INITDB_DATABASE"} {
		if v := w.Services[service].Env[k]; v != "" && !workspace.HasExpr(v) {
			return v
		}
	}
	return strings.ReplaceAll(w.Project, "-", "_")
}

func (x *exporter) header() string {
	var b strings.Builder
	b.WriteString("# Exported by `docklet export compose` from workspace.yaml: the base environment\n")
	b.WriteString("# without the Docklet proxy, features or database clones.\n")
	b.WriteString("#   docker compose --env-file .env.secrets up\n")
	var ports []string
	for n, p := range x.hostPort {
		ports = append(ports, fmt.Sprintf("%s → http://localhost:%d", n, p))
	}
	sort.Strings(ports)
	if len(ports) > 0 {
		b.WriteString("# Open: " + strings.Join(ports, ", ") + "\n")
	}
	sort.Strings(x.notes)
	for _, n := range x.notes {
		b.WriteString("# - " + n + "\n")
	}
	return b.String()
}

// moduleDir is where the module's code is relative to the exported file.
func moduleDir(name string, m *workspace.Module) string {
	if m.Path != "" {
		return m.Path
	}
	return name
}

func dotSlash(p string) string {
	p = path.Clean(p)
	if p == "." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "./") || path.IsAbs(p) {
		return p
	}
	return "./" + p
}

func collectVolumes(set map[string]bool, vols []string) {
	for _, v := range vols {
		src, _, ok := strings.Cut(v, ":")
		if ok && !strings.HasPrefix(src, ".") && !strings.HasPrefix(src, "/") && !strings.HasPrefix(src, "~") {
			set[src] = true
		}
	}
}

func emptyNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}
