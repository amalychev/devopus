package spec

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/amalychev/devopus/internal/catalog"
)

// Changes between two versions of a project's settings, for people to read and for
// the agent to decide (risk R21): a risky change makes the machine run code or reach
// what it did not before — a new image outside the catalog, commands, hooks, builds,
// folders of the host, another repository. Whoever controls the workspace's settings
// controls those on every machine of the team, so the agent asks before it applies
// them. Versions of catalog services, variables, routes and ports are not risky.

// Change is one difference.
type Change struct {
	What  string
	Risky bool
	// Section is apps.<name> or services.<name>; empty for the project's own settings.
	Section string
}

// Diff lists what changed from old to next, risky changes first.
func Diff(old, next *Project) []Change {
	var out []Change
	add := func(risky bool, format string, a ...any) {
		c := Change{What: fmt.Sprintf(format, a...), Risky: risky}
		if path, _, ok := strings.Cut(c.What, ":"); ok && (strings.HasPrefix(path, "apps.") || strings.HasPrefix(path, "services.")) {
			kind, rest, _ := strings.Cut(path, ".")
			name, _, _ := strings.Cut(rest, ".")
			c.Section = kind + "." + name
		}
		out = append(out, c)
	}
	for _, n := range union(old.AppNames(), next.AppNames()) {
		a, b := old.Apps[n], next.Apps[n]
		p := "apps." + n
		switch {
		case a == nil:
			add(true, "%s: new application from %s, command %q", p, codeOf(b), b.Command)
			continue
		case b == nil:
			add(false, "%s: removed", p)
			continue
		}
		risky := func(field string, x, y any) {
			if !same(x, y) {
				add(true, "%s.%s: %v → %v", p, field, show(x), show(y))
			}
		}
		risky("repo", a.Repo, b.Repo)
		risky("path", a.Path, b.Path)
		risky("image", a.Image, b.Image)
		risky("build", a.Build, b.Build)
		risky("command", a.Command, b.Command)
		risky("entrypoint", a.Entrypoint, b.Entrypoint)
		risky("hooks", a.Hooks, b.Hooks)
		risky("volumes", a.Volumes, b.Volumes)
		risky("mount", a.Mount, b.Mount)
		if a.Branch != b.Branch {
			add(false, "%s.branch: %s → %s", p, a.BaseBranch(), b.BaseBranch())
		}
		if !reflect.DeepEqual(a.Routes, b.Routes) {
			add(false, "%s.routes: %v → %v", p, a.Routes, b.Routes)
		}
		if a.Port != b.Port {
			add(false, "%s.port: %d → %d", p, a.AppPort(), b.AppPort())
		}
		envChanges(add, p, a.Env, b.Env)
	}
	for _, n := range union(old.ServiceNames(), next.ServiceNames()) {
		a, b := old.Services[n], next.Services[n]
		p := "services." + n
		switch {
		case a == nil:
			add(!trustedImage(b), "%s: new %s %s", p, b.Type, show(b.ImageRef()))
			continue
		case b == nil:
			add(false, "%s: removed", p)
			continue
		}
		if a.ImageRef() != b.ImageRef() || a.Type != b.Type {
			add(!trustedImage(b) || a.Type != b.Type, "%s: %s → %s", p, show(a.ImageRef()), show(b.ImageRef()))
		}
		risky := func(field string, x, y any) {
			if !same(x, y) {
				add(true, "%s.%s: %v → %v", p, field, show(x), show(y))
			}
		}
		risky("command", a.Command, b.Command)
		risky("entrypoint", a.Entrypoint, b.Entrypoint)
		risky("volumes", a.Volumes, b.Volumes)
		if !reflect.DeepEqual(a.Apps, b.Apps) {
			add(false, "%s.apps: %v → %v", p, a.Apps, b.Apps)
		}
		if a.Port != b.Port {
			add(false, "%s.port: %d → %d", p, a.Port, b.Port)
		}
		envChanges(add, p, a.Env, b.Env)
	}
	if old.Domain != next.Domain {
		add(false, "domain: %s → %s", old.Domain, next.Domain)
	}
	if !reflect.DeepEqual(old.Features, next.Features) || !reflect.DeepEqual(old.Overrides, next.Overrides) || !reflect.DeepEqual(old.Data, next.Data) {
		add(false, "policies changed")
	}
	slices.SortStableFunc(out, func(x, y Change) int {
		switch {
		case x.Risky && !y.Risky:
			return -1
		case !x.Risky && y.Risky:
			return 1
		}
		return 0
	})
	return out
}

// Risky are the risky changes of a diff.
func Risky(changes []Change) []string {
	var out []string
	for _, c := range changes {
		if c.Risky {
			out = append(out, c.What)
		}
	}
	return out
}

// WithSection is the project with one section replaced by its YAML, as its author
// sent it; empty YAML removes it.
func (p *Project) WithSection(section string, data []byte) (*Project, error) {
	out := p.Clone()
	kind, name, _ := strings.Cut(section, ".")
	switch kind {
	case "apps":
		if len(data) == 0 {
			delete(out.Apps, name)
			return out, nil
		}
		var a App
		if err := yaml.Unmarshal(data, &a); err != nil {
			return nil, fmt.Errorf("%s: %w", section, err)
		}
		if out.Apps == nil {
			out.Apps = map[string]*App{}
		}
		out.Apps[name] = &a
	case "services":
		if len(data) == 0 {
			delete(out.Services, name)
			return out, nil
		}
		var sv Service
		if err := yaml.Unmarshal(data, &sv); err != nil {
			return nil, fmt.Errorf("%s: %w", section, err)
		}
		if out.Services == nil {
			out.Services = map[string]*Service{}
		}
		out.Services[name] = &sv
	default:
		return nil, fmt.Errorf("unknown section %q", section)
	}
	return out, nil
}

// same compares as YAML: nil and empty are the same.
func same(x, y any) bool {
	a, errA := yaml.Marshal(x)
	b, errB := yaml.Marshal(y)
	return errA == nil && errB == nil && string(a) == string(b)
}

// trustedImage: a service runs the catalog's image of its type, or no image of its own.
func trustedImage(s *Service) bool {
	t, ok := catalog.Lookup(s.Type)
	if !ok || t.Kind == catalog.Process {
		return ok // a process runs the application's code: its command decides
	}
	if t.Repo == "" {
		return false // custom
	}
	ref := s.ImageRef()
	repo, _, _ := strings.Cut(ref, "@")
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	repo = strings.TrimPrefix(strings.TrimPrefix(repo, "docker.io/"), "library/")
	return repo == t.Repo
}

func envChanges(add func(bool, string, ...any), p string, a, b map[string]string) {
	for _, k := range union(sortedKeys(a), sortedKeys(b)) {
		x, inA := a[k]
		y, inB := b[k]
		switch {
		case !inA:
			add(false, "%s.env.%s: set", p, k)
		case !inB:
			add(false, "%s.env.%s: removed", p, k)
		case x != y:
			add(false, "%s.env.%s: changed", p, k)
		}
	}
}

func codeOf(a *App) string {
	if a.Repo != "" {
		return a.Repo
	}
	if a.Path != "" {
		return "./" + a.Path
	}
	return a.Image
}

func show(v any) string {
	switch x := v.(type) {
	case string:
		if x == "" {
			return "—"
		}
		return fmt.Sprintf("%q", x)
	case nil:
		return "—"
	}
	if rv := reflect.ValueOf(v); (rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Slice || rv.Kind() == reflect.Map) && rv.IsNil() {
		return "—"
	}
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Pointer {
		return fmt.Sprintf("%+v", rv.Elem().Interface())
	}
	return fmt.Sprintf("%v", v)
}

func union(a, b []string) []string {
	out := append(append([]string{}, a...), b...)
	slices.Sort(out)
	return slices.Compact(out)
}
