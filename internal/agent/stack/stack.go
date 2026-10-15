// Package stack turns a workspace.yaml into the containers of one environment: its
// services, modules and workers, the hooks that prepare the modules, the volumes and
// the images to build. It only describes them; the reconciler makes them exist.
package stack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-shellwords"

	"github.com/devcorb/devcorb/internal/agent/runtime"
	"github.com/devcorb/devcorb/internal/workspace"
)

// Labels on the containers of an environment, besides the common ones.
const (
	// LabelUnit is the unit's name in the environment: mysql, api, api-worker-1.
	LabelUnit = "com.devcorb.unit"
	// LabelModule is the module or service a unit belongs to.
	LabelModule = "com.devcorb.module"
)

// Roles of the containers (runtime.LabelRole).
const (
	RoleService = "service"
	RoleModule  = "module"
	RoleWorker  = "worker"
	RoleHook    = "hook"
)

// Where the local CA appears in module containers (local-networking.md: no image
// rebuild, environment variables point at the files).
const (
	CAPath     = "/devcorb/ca.pem"
	BundlePath = "/devcorb/bundle.pem"
)

// Env is what the plan needs to know about the environment besides workspace.yaml.
type Env struct {
	ID      string
	Project string
	Slug    string
	// Root is the repository root: the folder of workspace.yaml.
	Root string
	// Network is the environment network's ID, Subnet its /24.
	Network string
	Subnet  netip.Prefix
	// URL is the environment's public URL, the value of ${{ env.url }}.
	URL string
	// Sources maps modules to the folder of their code on the host.
	Sources map[string]string
	// Heads maps modules to the commit of their code; a new one reruns migrate.
	Heads   map[string]string
	Secrets map[string]string
	// CA is the local root; Bundle is the public roots plus it.
	CA, Bundle []byte
	// DNS is the environment's DNS server.
	DNS netip.Addr
	// Labels go on everything: managed, instance, project, environment, kind.
	Labels map[string]string
}

// Unit is one long-running container.
type Unit struct {
	Name string
	Role string
	// Owner is the module or service in workspace.yaml.
	Owner string
	Spec  runtime.ContainerSpec
	IP    netip.Addr
	// Port is where the proxy sends requests: the module's port, the service's web UI.
	Port   int
	Routes []string
	Expose string
	// Needs are units that must be ready first.
	Needs []string
	// Healthcheck: ready means healthy, not just running.
	Healthcheck bool
}

// Hook is a one-shot command in a module's container.
type Hook struct {
	Module  string
	Name    string // install, migrate
	Command string
	Spec    runtime.ContainerSpec
	// Key changes when the hook must run again: another command or image, changed lock
	// files (install), a new commit (migrate).
	Key string
}

// Plan is everything an environment runs.
type Plan struct {
	// Units in start order: dependencies first.
	Units   []Unit
	Hooks   []Hook
	Volumes []string
	Builds  []runtime.BuildSpec
	// Images to pull: services, runtimes and module images.
	Images []string
}

// Prefix is the start of every container name of the environment; the network has it too.
func Prefix(project, envID string) string { return project + "-env-" + envID }

// VolumeName namespaces a volume by environment (docker-isolation.md).
func VolumeName(project, envID, name string) string { return project + "-" + envID + "-" + name }

// Active are the modules and services the environment runs: everything that is not
// optional, and the optional ones something active uses.
func Active(w *workspace.Workspace) (modules, services []string) {
	on := map[string]bool{}
	var visit func(name string)
	visit = func(name string) {
		if on[name] {
			return
		}
		on[name] = true
		var uses []string
		if m := w.Modules[name]; m != nil {
			uses = m.Uses
		} else if s := w.Services[name]; s != nil {
			uses = s.Uses
		}
		for _, u := range uses {
			visit(u)
		}
	}
	for _, n := range w.ModuleNames() {
		if !w.Modules[n].Optional {
			visit(n)
		}
	}
	for _, n := range w.ServiceNames() {
		if !w.Services[n].Optional {
			visit(n)
		}
	}
	for _, n := range w.ModuleNames() {
		if on[n] {
			modules = append(modules, n)
		}
	}
	for _, n := range w.ServiceNames() {
		if on[n] {
			services = append(services, n)
		}
	}
	return modules, services
}

// Build makes the plan of an environment.
func Build(w *workspace.Workspace, env Env) (*Plan, error) {
	modules, services := Active(w)
	b := &builder{w: w, env: env, plan: &Plan{}, images: map[string]bool{}, volumes: map[string]bool{}}
	ips, err := pinIPs(env.Subnet, append(append([]string{}, services...), modules...))
	if err != nil {
		return nil, err
	}
	b.ips = ips
	var units []Unit
	for _, name := range services {
		u, err := b.service(name, w.Services[name])
		if err != nil {
			return nil, fmt.Errorf("services.%s: %w", name, err)
		}
		units = append(units, u)
	}
	for _, name := range modules {
		us, err := b.module(name, w.Modules[name])
		if err != nil {
			return nil, fmt.Errorf("modules.%s: %w", name, err)
		}
		units = append(units, us...)
	}
	b.plan.Units = order(units)
	// Install everywhere first, then migrate in dependency order: a module's migrations
	// may need what another one migrated.
	var installs, migrates []Hook
	for _, u := range b.plan.Units {
		if u.Role != RoleModule {
			continue
		}
		m := w.Modules[u.Owner]
		if m.Hooks == nil {
			continue
		}
		if m.Hooks.Install != "" {
			h, err := b.hook(u, "install", m.Hooks.Install, lockHash(env.Sources[u.Owner]))
			if err != nil {
				return nil, err
			}
			installs = append(installs, h)
		}
		if m.Hooks.Migrate != "" {
			h, err := b.hook(u, "migrate", m.Hooks.Migrate, env.Heads[u.Owner])
			if err != nil {
				return nil, err
			}
			migrates = append(migrates, h)
		}
	}
	b.plan.Hooks = append(installs, migrates...)
	for v := range b.volumes {
		b.plan.Volumes = append(b.plan.Volumes, v)
	}
	sort.Strings(b.plan.Volumes)
	for img := range b.images {
		b.plan.Images = append(b.plan.Images, img)
	}
	sort.Strings(b.plan.Images)
	return b.plan, nil
}

type builder struct {
	w       *workspace.Workspace
	env     Env
	plan    *Plan
	ips     map[string]netip.Addr
	images  map[string]bool
	volumes map[string]bool
}

func (b *builder) labels(role, unit, owner string) map[string]string {
	l := map[string]string{runtime.LabelRole: role, LabelUnit: unit, LabelModule: owner}
	for k, v := range b.env.Labels {
		l[k] = v
	}
	return l
}

func (b *builder) service(name string, s *workspace.Service) (Unit, error) {
	env, err := b.expandEnv(s.Env)
	if err != nil {
		return Unit{}, err
	}
	spec := runtime.ContainerSpec{
		Name:    Prefix(b.env.Project, b.env.ID) + "-" + name,
		Image:   s.Image,
		Env:     env,
		Labels:  b.labels(RoleService, name, name),
		Network: b.env.Network,
		IP:      b.ips[name],
		Aliases: []string{name},
		DNS:     dns(b.env.DNS),
		Tmpfs:   tmpfs(s.Tmpfs),
		Ulimits: ulimits(s.Ulimits),
	}
	if spec.Cmd, err = b.command(s.Command); err != nil {
		return Unit{}, fmt.Errorf("command: %w", err)
	}
	if spec.Entrypoint, err = b.command(s.Entrypoint); err != nil {
		return Unit{}, fmt.Errorf("entrypoint: %w", err)
	}
	if spec.Health, err = health(s.Healthcheck); err != nil {
		return Unit{}, err
	}
	for _, v := range s.Volumes {
		if err := b.mount(&spec, v, true); err != nil {
			return Unit{}, err
		}
	}
	b.images[s.Image] = true
	finish(&spec)
	return Unit{Name: name, Role: RoleService, Owner: name, Spec: spec, IP: b.ips[name], Port: s.ExposePort(), Expose: s.Expose,
		Needs: s.Uses, Healthcheck: spec.Health != nil && spec.Health.Test[0] != "NONE"}, nil
}

func (b *builder) module(name string, m *workspace.Module) ([]Unit, error) {
	src := b.env.Sources[name]
	if src == "" {
		return nil, fmt.Errorf("no source folder")
	}
	image := m.Image
	switch {
	case m.Runtime != "":
		image = workspace.RuntimeImage(m.Runtime)
	case m.Build != nil:
		ctxDir := filepath.Join(b.env.Root, filepath.FromSlash(m.Build.Context))
		if m.Repo != "" {
			ctxDir = filepath.Join(src, filepath.FromSlash(m.Build.Context))
		}
		bs, err := b.build(name, ctxDir, m.Build)
		if err != nil {
			return nil, err
		}
		image = bs.Tag
		b.plan.Builds = append(b.plan.Builds, bs)
	}
	if m.Build == nil {
		b.images[image] = true
	}
	env, err := b.expandEnv(m.Env)
	if err != nil {
		return nil, err
	}
	base := runtime.ContainerSpec{
		Image:   image,
		Env:     append(caEnv(), env...),
		Network: b.env.Network,
		DNS:     dns(b.env.DNS),
		Init:    true,
		Tmpfs:   tmpfs(m.Tmpfs),
		Ulimits: ulimits(m.Ulimits),
		Files: []runtime.File{
			{Path: CAPath, Data: b.env.CA, Mode: 0o644},
			{Path: BundlePath, Data: b.env.Bundle, Mode: 0o644},
		},
	}
	if mount := m.CodeMount(); mount != "" {
		base.Binds = []runtime.Bind{{Source: src, Target: mount}}
		base.WorkDir = mount
	}
	if base.Entrypoint, err = b.command(m.Entrypoint); err != nil {
		return nil, fmt.Errorf("entrypoint: %w", err)
	}
	for _, v := range m.Volumes {
		if err := b.mount(&base, v, false); err != nil {
			return nil, err
		}
	}

	main := clone(base)
	main.Name = Prefix(b.env.Project, b.env.ID) + "-" + name
	main.Labels = b.labels(RoleModule, name, name)
	main.IP = b.ips[name]
	main.Aliases = []string{name}
	if main.Cmd, err = b.command(m.Command); err != nil {
		return nil, fmt.Errorf("command: %w", err)
	}
	if main.Health, err = health(m.Healthcheck); err != nil {
		return nil, err
	}
	finish(&main)
	units := []Unit{{Name: name, Role: RoleModule, Owner: name, Spec: main, IP: main.IP, Port: m.ModulePort(), Routes: m.Routes,
		Needs: m.Uses, Healthcheck: main.Health != nil && main.Health.Test[0] != "NONE"}}

	for i, wk := range m.Workers {
		replicas := max(wk.Replicas, 1)
		for r := 1; r <= replicas; r++ {
			unit := fmt.Sprintf("%s-worker-%d", name, i+1)
			if replicas > 1 {
				unit += "-" + strconv.Itoa(r)
			}
			s := clone(base)
			s.Name = Prefix(b.env.Project, b.env.ID) + "-" + unit
			s.Labels = b.labels(RoleWorker, unit, name)
			if s.Cmd, err = b.command(wk.Command); err != nil {
				return nil, fmt.Errorf("workers[%d]: %w", i, err)
			}
			finish(&s)
			units = append(units, Unit{Name: unit, Role: RoleWorker, Owner: name, Spec: s, Needs: append([]string{name}, m.Uses...)})
		}
	}
	return units, nil
}

// hook runs command in a copy of the module's container: same image, code, volumes,
// network and variables, so it reaches the services the module uses.
func (b *builder) hook(u Unit, name, command, input string) (Hook, error) {
	cmd, err := b.expand(command)
	if err != nil {
		return Hook{}, fmt.Errorf("modules.%s.hooks.%s: %w", u.Owner, name, err)
	}
	s := clone(u.Spec)
	s.Name = u.Spec.Name + "-" + name
	s.Labels = b.labels(RoleHook, u.Name+"-"+name, u.Owner)
	s.IP, s.Aliases, s.Health = netip.Addr{}, nil, nil
	s.Cmd = []string{"sh", "-c", cmd}
	finish(&s)
	sum := sha256.Sum256([]byte(s.Labels[runtime.LabelConfig] + "\n" + input))
	return Hook{Module: u.Owner, Name: name, Command: cmd, Spec: s, Key: hex.EncodeToString(sum[:])[:16]}, nil
}

// build describes the image of a module with build; its tag changes with the
// Dockerfile and the build options, not with every change of the code.
func (b *builder) build(name, ctxDir string, bd *workspace.Build) (runtime.BuildSpec, error) {
	args := map[string]string{}
	for k, v := range bd.Args {
		e, err := b.expand(v)
		if err != nil {
			return runtime.BuildSpec{}, fmt.Errorf("build.args.%s: %w", k, err)
		}
		args[k] = e
	}
	dockerfile := bd.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	content, err := os.ReadFile(filepath.Join(ctxDir, filepath.FromSlash(dockerfile)))
	if err != nil {
		return runtime.BuildSpec{}, fmt.Errorf("build: %w", err)
	}
	spec := runtime.BuildSpec{Context: ctxDir, Dockerfile: bd.Dockerfile, Target: bd.Target, Args: args}
	key, _ := json.Marshal(spec)
	sum := sha256.Sum256(append(key, content...))
	spec.Tag = "devcorb/" + b.env.Project + "-" + name + ":" + hex.EncodeToString(sum[:])[:12]
	spec.Labels = map[string]string{runtime.LabelManaged: "true", runtime.LabelProject: b.env.Project, runtime.LabelInstance: b.env.Labels[runtime.LabelInstance]}
	return spec, nil
}

// mount adds a volume entry: name:/path is a volume of the environment; a service may
// also mount a file or folder of the repository (./config/x.xml:/etc/x.xml[:ro]).
func (b *builder) mount(spec *runtime.ContainerSpec, v string, binds bool) error {
	parts := strings.Split(v, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return fmt.Errorf("volume %q: expected source:target", v)
	}
	src, target := parts[0], parts[1]
	ro := len(parts) == 3 && parts[2] == "ro"
	if strings.HasPrefix(src, ".") || strings.HasPrefix(src, "/") {
		if !binds {
			return fmt.Errorf("volume %q: modules get their code from the checkout; mount only named volumes", v)
		}
		if path.IsAbs(src) {
			return fmt.Errorf("volume %q: folders outside the repository are not supported yet", v)
		}
		host := filepath.Join(b.env.Root, filepath.FromSlash(src))
		if rel, err := filepath.Rel(b.env.Root, host); err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("volume %q: folders outside the repository are not supported yet", v)
		}
		spec.Binds = append(spec.Binds, runtime.Bind{Source: host, Target: target, ReadOnly: ro})
		return nil
	}
	name := VolumeName(b.env.Project, b.env.ID, src)
	b.volumes[name] = true
	spec.Volumes = append(spec.Volumes, runtime.VolumeMount{Name: name, Target: target})
	return nil
}

func (b *builder) expandEnv(env map[string]string) ([]string, error) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		v, err := b.expand(env[k])
		if err != nil {
			return nil, fmt.Errorf("env.%s: %w", k, err)
		}
		out = append(out, k+"="+v)
	}
	return out, nil
}

// expand resolves ${{ }} for the base environment (specs/workspace-yaml.md).
func (b *builder) expand(s string) (string, error) {
	return b.w.Expand(s, func(r workspace.Ref) (string, error) {
		switch r.Kind {
		case workspace.RefSecret:
			v, ok := b.env.Secrets[r.Name]
			if !ok {
				return "", fmt.Errorf("secret %s is not in .env.secrets", r.Name)
			}
			return v, nil
		case workspace.RefEnv:
			switch r.Attr {
			case "url":
				return b.env.URL, nil
			case "slug":
				return b.env.Slug, nil
			default:
				return b.env.ID, nil
			}
		case workspace.RefModule:
			port := b.w.Modules[r.Name].ModulePort()
			switch r.Attr {
			case "url":
				return fmt.Sprintf("http://%s:%d", r.Name, port), nil
			case "host":
				return r.Name, nil
			default:
				return strconv.Itoa(port), nil
			}
		case workspace.RefService:
			s := b.w.Services[r.Name]
			switch r.Attr {
			case "host":
				return r.Name, nil
			case "port":
				if s.Port == 0 {
					return "", fmt.Errorf("%s has no port", r.Name)
				}
				return strconv.Itoa(s.Port), nil
			default:
				return b.w.DatabaseName(r.Name), nil
			}
		}
		return "", fmt.Errorf("cannot resolve %s", r.Raw)
	})
}

// command splits a command line like compose does with a string: words, quotes
// honoured. A line with shell syntax (&&, pipes, redirects, variables) runs in sh -c.
func (b *builder) command(line string) ([]string, error) {
	line, err := b.expand(line)
	if err != nil || strings.TrimSpace(line) == "" {
		return nil, err
	}
	return Command(line), nil
}

// Command splits a command line into words; shell syntax makes it sh -c line.
func Command(line string) []string {
	p := shellwords.NewParser()
	words, err := p.Parse(line)
	if err != nil || p.Position >= 0 || strings.ContainsAny(line, "$`*?\n") {
		return []string{"sh", "-c", line}
	}
	return words
}

// caEnv points TLS libraries at the bundle, so modules trust the environment's own
// HTTPS URL and public sites alike (P-6: curl, Python, Node, PHP).
func caEnv() []string {
	return []string{
		"SSL_CERT_FILE=" + BundlePath,
		"CURL_CA_BUNDLE=" + BundlePath,
		"REQUESTS_CA_BUNDLE=" + BundlePath,
		"NODE_EXTRA_CA_CERTS=" + CAPath,
	}
}

func dns(ip netip.Addr) []netip.Addr {
	if !ip.IsValid() {
		return nil
	}
	return []netip.Addr{ip}
}

func tmpfs(list []string) map[string]string {
	if len(list) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, t := range list {
		target, opts, _ := strings.Cut(t, ":")
		out[target] = opts
	}
	return out
}

func ulimits(m map[string]*workspace.Ulimit) []runtime.Ulimit {
	var out []runtime.Ulimit
	for name, u := range m {
		if u == nil {
			continue
		}
		soft, hard := u.Soft, u.Hard
		if u.Single != 0 {
			soft, hard = u.Single, u.Single
		}
		out = append(out, runtime.Ulimit{Name: name, Soft: int64(soft), Hard: int64(hard)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func health(h *workspace.Healthcheck) (*runtime.Health, error) {
	if h == nil {
		return nil, nil
	}
	if h.Disable {
		return &runtime.Health{Test: []string{"NONE"}}, nil
	}
	if len(h.Test) == 0 {
		return nil, nil
	}
	out := &runtime.Health{Retries: h.Retries}
	switch h.Test[0] {
	case "CMD", "CMD-SHELL", "NONE":
		out.Test = h.Test
	default:
		if len(h.Test) == 1 {
			out.Test = []string{"CMD-SHELL", h.Test[0]}
		} else {
			out.Test = append([]string{"CMD"}, h.Test...)
		}
	}
	for _, d := range []struct {
		s   string
		dst *time.Duration
	}{{h.Interval, &out.Interval}, {h.Timeout, &out.Timeout}, {h.StartPeriod, &out.StartPeriod}} {
		if d.s == "" {
			continue
		}
		v, err := time.ParseDuration(d.s)
		if err != nil {
			return nil, fmt.Errorf("healthcheck: %w", err)
		}
		*d.dst = v
	}
	return out, nil
}

// pinIPs gives every service and module a fixed address in the upper half of the
// network, which Docker does not hand out (.128–.252; .253 is DNS, .254 the proxy).
// The address depends on the name, so adding a module rarely moves another.
func pinIPs(subnet netip.Prefix, names []string) (map[string]netip.Addr, error) {
	const first, last = 128, 252
	if len(names) > last-first+1 {
		return nil, fmt.Errorf("%d services and modules do not fit into one environment (at most %d)", len(names), last-first+1)
	}
	sorted := append([]string{}, names...)
	sort.Strings(sorted)
	taken := map[int]bool{}
	out := map[string]netip.Addr{}
	base := subnet.Masked().Addr().As4()
	for _, n := range sorted {
		h := fnv.New32a()
		h.Write([]byte(n))
		slot := int(h.Sum32() % uint32(last-first+1))
		for taken[slot] {
			slot = (slot + 1) % (last - first + 1)
		}
		taken[slot] = true
		a := base
		a[3] = byte(first + slot)
		out[n] = netip.AddrFrom4(a)
	}
	return out, nil
}

// order sorts units so that each comes after what it needs. Cycles among modules are
// allowed (specs/workspace-yaml.md, rule 3): their members start in name order.
func order(units []Unit) []Unit {
	byName := map[string]int{}
	for i, u := range units {
		byName[u.Name] = i
	}
	done := map[string]bool{}
	visiting := map[string]bool{}
	var out []Unit
	var visit func(i int)
	visit = func(i int) {
		u := units[i]
		if done[u.Name] || visiting[u.Name] {
			return
		}
		visiting[u.Name] = true
		for _, n := range u.Needs {
			if j, ok := byName[n]; ok {
				visit(j)
			}
		}
		visiting[u.Name] = false
		done[u.Name] = true
		out = append(out, u)
	}
	for i := range units {
		visit(i)
	}
	return out
}

func clone(s runtime.ContainerSpec) runtime.ContainerSpec {
	c := s
	c.Env = append([]string{}, s.Env...)
	c.Binds = append([]runtime.Bind{}, s.Binds...)
	c.Volumes = append([]runtime.VolumeMount{}, s.Volumes...)
	c.Files = append([]runtime.File{}, s.Files...)
	return c
}

// finish labels a spec with the hash of everything it is made of: another hash means
// the container must be recreated.
func finish(spec *runtime.ContainerSpec) {
	labels := spec.Labels
	spec.Labels = nil
	data, _ := json.Marshal(spec)
	spec.Labels = labels
	keys := make([]string, 0, len(labels))
	for k := range labels {
		if k != runtime.LabelConfig {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		data = append(data, k+"="+labels[k]+"\n"...)
	}
	sum := sha256.Sum256(data)
	spec.Labels[runtime.LabelConfig] = hex.EncodeToString(sum[:])[:16]
}

// lockFiles are the dependency manifests whose change reruns install.
var lockFiles = []string{
	"package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "pnpm-lock.yaml", "bun.lockb", "bun.lock",
	"composer.lock", "requirements.txt", "requirements-dev.txt", "poetry.lock", "Pipfile.lock", "uv.lock",
	"Gemfile.lock", "go.sum", "Cargo.lock", "mix.lock",
	"package.json", "composer.json", "pyproject.toml", "Gemfile",
}

func lockHash(dir string) string {
	h := sha256.New()
	for _, f := range lockFiles {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			continue
		}
		fmt.Fprintf(h, "%s %d\n", f, len(data))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
