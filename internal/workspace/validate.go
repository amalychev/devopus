package workspace

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Validate checks the rules of specs/workspace-yaml.md that a JSON Schema cannot
// express. doc is the source document for line numbers and may be nil.
func Validate(w *Workspace, doc *yaml.Node) Diagnostics {
	c := &collector{doc: doc}
	checkNames(c, w)
	for _, name := range w.ServiceNames() {
		checkService(c, w, name, w.Services[name])
	}
	for _, name := range w.ModuleNames() {
		checkModule(c, w, name, w.Modules[name])
	}
	checkRoutes(c, w)
	checkCycles(c, w)
	checkExpose(c, w)
	checkPolicy(c, w)
	return c.diags
}

func checkNames(c *collector, w *Workspace) {
	for _, name := range w.ModuleNames() {
		if _, dup := w.Services[name]; dup {
			c.errorf(p("modules", name), "%q is both a module and a service; names share one namespace", name)
		}
	}
}

func checkService(c *collector, w *Workspace, name string, s *Service) {
	at := func(parts ...any) []string { return p(append([]any{"services", name}, parts...)...) }
	if s.Image != "" && !ImagePinned(s.Image) {
		c.errorf(at("image"), "image %q is not pinned: use a version tag (postgres:17.2) or a digest", s.Image)
	}
	for i, u := range s.Uses {
		switch {
		case u == name:
			c.errorf(at("uses", i), "a service cannot use itself")
		case w.Services[u] != nil:
		case w.Modules[u] != nil:
			c.errorf(at("uses", i), "a service can only use services; %q is a module", u)
		default:
			c.errorf(at("uses", i), "unknown service %q", u)
		}
	}
	if s.Data != nil && strings.HasPrefix(s.Data.Seed, "hook://") {
		mod, _, _ := strings.Cut(strings.TrimPrefix(s.Data.Seed, "hook://"), ".")
		if w.Modules[mod] == nil {
			c.errorf(at("data", "seed"), "unknown module %q", mod)
		}
	}
	for i, v := range s.Volumes {
		checkServiceVolume(c, at("volumes", i), v)
	}
	checkEnv(c, w, at("env"), name, s.Env, s.Uses)
	checkExprs(c, w, at("command"), s.Command)
}

func checkModule(c *collector, w *Workspace, name string, m *Module) {
	at := func(parts ...any) []string { return p(append([]any{"modules", name}, parts...)...) }

	switch {
	case m.Repo != "" && m.Path != "":
		c.errorf(at(), "set either repo or path, not both")
	case m.Repo == "" && m.Path == "":
		c.errorf(at(), "set repo (a git URL) or path (a folder of this repository)")
	}
	if m.Path != "" {
		checkRelative(c, at("path"), m.Path)
	}

	sources := 0
	for _, set := range []bool{m.Runtime != "", m.Image != "", m.Build != nil} {
		if set {
			sources++
		}
	}
	switch {
	case sources == 0:
		c.errorf(at(), "set one of runtime, image or build")
	case sources > 1:
		c.errorf(at(), "runtime, image and build are alternatives; set exactly one")
	}
	if m.Runtime != "" && m.Command == "" {
		c.errorf(at("command"), "a module on a platform runtime needs a command to start")
	}
	if m.Image != "" && !ImagePinned(m.Image) {
		c.errorf(at("image"), "image %q is not pinned: use a version tag or a digest", m.Image)
	}
	if m.Build != nil {
		if m.Path == "" || path.Clean(m.Build.Context) != path.Clean(m.Path) {
			checkRelative(c, at("build", "context"), m.Build.Context)
		}
		for k, v := range m.Build.Args {
			checkExprs(c, w, at("build", "args", k), v)
		}
	}

	for i, u := range m.Uses {
		switch {
		case u == name:
			c.errorf(at("uses", i), "a module cannot use itself")
		case w.Modules[u] == nil && w.Services[u] == nil:
			c.errorf(at("uses", i), "unknown module or service %q", u)
		}
	}

	checkEnv(c, w, at("env"), name, m.Env, m.Uses)
	checkExprs(c, w, at("command"), m.Command)
	if m.Hooks != nil {
		checkExprs(c, w, at("hooks", "install"), m.Hooks.Install)
		checkExprs(c, w, at("hooks", "migrate"), m.Hooks.Migrate)
		checkExprs(c, w, at("hooks", "seed"), m.Hooks.Seed)
		checkExprs(c, w, at("hooks", "build"), m.Hooks.Build)
	}
	for i, cr := range m.Cron {
		if len(strings.Fields(cr.Schedule)) != 5 && !strings.HasPrefix(cr.Schedule, "@") {
			c.errorf(at("cron", i, "schedule"), "expected 5 cron fields or @hourly/@daily, got %q", cr.Schedule)
		}
		checkExprs(c, w, at("cron", i, "command"), cr.Command)
	}
	for i, wk := range m.Workers {
		checkExprs(c, w, at("workers", i, "command"), wk.Command)
	}
	for i, v := range m.Volumes {
		src, dst, ok := strings.Cut(v, ":")
		if !ok || !strings.HasPrefix(dst, "/") || src == "" || strings.ContainsAny(src[:1], "./~") || strings.Contains(src, "/") {
			c.errorf(at("volumes", i), "expected a named volume name:/path, got %q; the worktree already provides the code", v)
		}
	}
	if len(m.Ports) > 0 {
		c.warnf(at("ports"), "ports are ignored: environments are reachable only through the proxy; use routes")
	}
}

// checkEnv validates expressions and looks for secrets and absolute API URLs.
func checkEnv(c *collector, w *Workspace, at []string, owner string, env map[string]string, uses []string) {
	used := map[string]bool{}
	for _, u := range uses {
		used[u] = true
	}
	for _, k := range sortedKeys(env) {
		v := env[k]
		kp := append(append([]string{}, at...), k)
		refs := checkExprs(c, w, kp, v)
		for _, r := range refs {
			if (r.Kind == RefModule || r.Kind == RefService) && r.Name != owner && !used[r.Name] {
				c.warnf(kp, "refers to %s but %s does not list it in uses; features will not offer to fork it together", r.Name, owner)
			}
		}
		if !HasExpr(v) && v != "" && (secretKeyRe.MatchString(k) || secretValueRe.MatchString(v)) && !placeholderRe.MatchString(v) {
			c.warnf(kp, "looks like a secret in plain text; use ${{ secrets.%s }} and put the value into .env.secrets", strings.ToUpper(k))
		}
		if publicAPIKeyRe.MatchString(k) && strings.Contains(v, "://") {
			c.warnf(kp, "an absolute URL is baked into the frontend bundle, so features will call the base environment; use a relative path such as /api")
		}
	}
}

var (
	secretKeyRe    = regexp.MustCompile(`(?i)(SECRET|PASSWORD|PASSWD|TOKEN|API_?KEY|PRIVATE|CREDENTIAL)`)
	secretValueRe  = regexp.MustCompile(`^(sk_live_|rk_live_|ghp_|gho_|github_pat_|xox[bap]-|AKIA[0-9A-Z]{16}|-----BEGIN)`)
	placeholderRe  = regexp.MustCompile(`(?i)^(secret|password|changeme|example|test|dev|local|root|postgres|mysql|\$\{.*\})$`)
	publicAPIKeyRe = regexp.MustCompile(`^(VITE_|NEXT_PUBLIC_|REACT_APP_|NUXT_PUBLIC_|EXPO_PUBLIC_)\w*(API|BACKEND|SERVER)\w*$`)
)

// checkExprs validates every ${{ }} in s and returns the parsed references.
func checkExprs(c *collector, w *Workspace, at []string, s string) []Ref {
	if !HasExpr(s) {
		return nil
	}
	refs, err := w.Refs(s)
	if err != nil {
		c.errorf(at, "%v", err)
	}
	return refs
}

func checkRelative(c *collector, at []string, p string) {
	clean := path.Clean(filepath.ToSlash(p))
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		c.errorf(at, "%q must be a path inside the repository", p)
	}
}

func checkServiceVolume(c *collector, at []string, v string) {
	src, dst, ok := strings.Cut(v, ":")
	if !ok || !strings.HasPrefix(dst, "/") {
		c.errorf(at, "expected name:/path or ./file:/path, got %q", v)
		return
	}
	if !strings.HasPrefix(src, ".") && !strings.HasPrefix(src, "/") && !strings.HasPrefix(src, "~") {
		return // named volume
	}
	clean := path.Clean(src)
	if path.IsAbs(clean) || strings.HasPrefix(src, "~") || clean == ".." || strings.HasPrefix(clean, "../") {
		c.warnf(at, "bind mount outside the repository (%s) needs local confirmation on every machine", src)
	}
}

// normRoute treats /api and /api/ as the same prefix.
func normRoute(r string) string {
	if r != "/" {
		r = strings.TrimRight(r, "/")
	}
	return r
}

func checkRoutes(c *collector, w *Workspace) {
	owner := map[string]string{}
	routed := false
	for _, name := range w.ModuleNames() {
		for i, r := range w.Modules[name].Routes {
			routed = true
			n := normRoute(r)
			if prev, ok := owner[n]; ok && prev != name {
				c.errorf(p("modules", name, "routes", i), "route %s is already served by %s", r, prev)
				continue
			}
			owner[n] = name
		}
	}
	if !routed && len(w.Modules) > 0 {
		c.warnf(p("modules"), "no module has routes, so nothing is reachable at %s", w.Domain)
	}
}

// checkCycles: services must start in order, so a cycle is an error. Modules may call
// each other at run time; a cycle only means their hooks run in parallel.
func checkCycles(c *collector, w *Workspace) {
	svcGraph := map[string][]string{}
	for n, s := range w.Services {
		svcGraph[n] = s.Uses
	}
	if cyc := findCycle(svcGraph); cyc != nil {
		c.errorf(p("services", cyc[0], "uses"), "services depend on each other in a cycle: %s", strings.Join(cyc, " → "))
	}
	modGraph := map[string][]string{}
	for n, m := range w.Modules {
		for _, u := range m.Uses {
			if w.Modules[u] != nil {
				modGraph[n] = append(modGraph[n], u)
			}
		}
	}
	if cyc := findCycle(modGraph); cyc != nil {
		c.warnf(p("modules", cyc[0], "uses"), "modules use each other in a cycle (%s); their hooks will run in parallel", strings.Join(cyc, " → "))
	}
}

// findCycle returns one cycle as a path that starts and ends with the same node.
func findCycle(g map[string][]string) []string {
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var stack []string
	var found []string
	var visit func(n string) bool
	visit = func(n string) bool {
		color[n] = grey
		stack = append(stack, n)
		next := append([]string{}, g[n]...)
		sort.Strings(next)
		for _, m := range next {
			if _, ok := g[m]; !ok && color[m] == white {
				// a node without outgoing edges cannot close a cycle
				continue
			}
			switch color[m] {
			case grey:
				for i, s := range stack {
					if s == m {
						found = append(append([]string{}, stack[i:]...), m)
						return true
					}
				}
			case white:
				if visit(m) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
		return false
	}
	for _, n := range sortedKeys(g) {
		if color[n] == white && visit(n) {
			return found
		}
	}
	return nil
}

func checkExpose(c *collector, w *Workspace) {
	owner := map[string]string{}
	for _, name := range w.ServiceNames() {
		s := w.Services[name]
		if s.Expose == "" {
			continue
		}
		if s.ExposePort() == 0 {
			c.errorf(p("services", name, "expose"), "set ui_port (or port) so the proxy knows where the web UI listens")
		}
		if prev, ok := owner[s.Expose]; ok {
			c.errorf(p("services", name, "expose"), "%s.%s is already exposed by %s", s.Expose, w.Domain, prev)
		}
		owner[s.Expose] = name
	}
}

func checkPolicy(c *collector, w *Workspace) {
	if w.Features != nil && w.Features.Isolation == "contextual" {
		c.warnf(p("features", "isolation"), "contextual isolation is not available yet; features run with simple isolation")
	}
	if w.Data != nil {
		for _, name := range sortedKeys(w.Data.Snapshots) {
			src := w.Data.Snapshots[name].Source
			if w.Services[src] == nil {
				c.errorf(p("data", "snapshots", name, "source"), "unknown service %q", src)
			}
		}
	}
}

// checkPaths verifies that folders of modules exist next to the file.
func checkPaths(w *Workspace, dir string, doc *yaml.Node) Diagnostics {
	c := &collector{doc: doc}
	for _, name := range w.ModuleNames() {
		m := w.Modules[name]
		if m.Path != "" {
			if st, err := os.Stat(filepath.Join(dir, m.Path)); err != nil || !st.IsDir() {
				c.errorf(p("modules", name, "path"), "folder %q does not exist next to the workspace file", m.Path)
			}
		}
		if m.Build != nil && m.Path != "" && path.Clean(m.Build.Context) != path.Clean(m.Path) {
			if _, err := os.Stat(filepath.Join(dir, m.Build.Context)); err != nil {
				c.errorf(p("modules", name, "build", "context"), "folder %q does not exist", m.Build.Context)
			}
		}
	}
	return c.diags
}

// ImagePinned reports whether an image reference names a fixed version: a digest, or
// a tag that carries a version number. latest and names like alpine or bookworm float.
func ImagePinned(image string) bool {
	if strings.Contains(image, "@sha256:") {
		return true
	}
	last := image[strings.LastIndex(image, "/")+1:]
	i := strings.LastIndex(last, ":")
	if i < 0 {
		return false
	}
	tag := last[i+1:]
	if tag == "latest" || strings.Contains(tag, "$") {
		return false
	}
	return strings.ContainsAny(tag, "0123456789")
}
