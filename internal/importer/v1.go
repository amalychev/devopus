// Package importer turns other descriptions of a stack into a project of the model
// (internal/spec): workspace.yaml v1 of stage 1, and through it docker-compose files.
package importer

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/amalychev/devopus/internal/catalog"
	"github.com/amalychev/devopus/internal/spec"
	"github.com/amalychev/devopus/internal/workspace"
)

// FromV1 converts workspace.yaml v1 (docs/product/glossary.md, «Переход от модели
// этапа 1»): modules become applications; runtimes, workers and cron become services
// of the project attached to their application; a module's uses of services become
// the services' attachments. Containers come out exactly as stage 1 made them.
func FromV1(w *workspace.Workspace) (*spec.Project, error) {
	p := &spec.Project{
		Name:     w.Project,
		Domain:   w.Domain,
		Apps:     map[string]*spec.App{},
		Services: map[string]*spec.Service{},
	}
	if f := w.Features; f != nil {
		p.Features = &spec.FeaturePolicy{Database: f.Database, SleepAfter: f.SleepAfter, Cleanup: f.Cleanup,
			Isolation: f.Isolation, BranchPrefix: f.BranchPrefix, MaxRunning: f.MaxRunning}
	}
	if o := w.Overrides; o != nil {
		p.Overrides = &spec.OverridePolicy{Allowed: append([]string{}, o.Allowed...)}
	}
	if d := w.Data; d != nil {
		p.Data = &spec.DataPolicy{Snapshots: map[string]*spec.Snapshot{}}
		for k, s := range d.Snapshots {
			if s != nil {
				p.Data.Snapshots[k] = &spec.Snapshot{Source: s.Source, Mask: s.Mask}
			}
		}
	}
	taken := map[string]bool{}
	for n := range w.Modules {
		taken[n] = true
	}
	for n := range w.Services {
		taken[n] = true
	}

	for _, n := range w.ServiceNames() {
		s := w.Services[n]
		t, version := catalog.FromImage(s.Image)
		p.Services[n] = &spec.Service{
			Type: t.Name, Version: version, Image: s.Image, Uses: s.Uses,
			Port: s.Port, UIPort: s.UIPort, Expose: s.Expose, Env: s.Env, Command: s.Command, Entrypoint: s.Entrypoint,
			Volumes: s.Volumes, Healthcheck: health(s.Healthcheck), Tmpfs: s.Tmpfs, Ulimits: ulimits(s.Ulimits),
			PerFeature: s.PerFeature, Optional: s.Optional,
		}
		if s.Data != nil {
			p.Services[n].Data = &spec.ServiceData{Seed: s.Data.Seed}
		}
	}

	runtimes := map[string][]string{} // runtime string → modules
	for _, n := range w.ModuleNames() {
		m := w.Modules[n]
		a := &spec.App{
			Repo: m.Repo, Path: m.Path, Branch: m.Branch, Image: m.Image, Mount: m.Mount,
			Routes: m.Routes, Port: m.Port, Env: m.Env, Command: m.Command, Entrypoint: m.Entrypoint,
			Healthcheck: health(m.Healthcheck), Tmpfs: m.Tmpfs, Ulimits: ulimits(m.Ulimits), Optional: m.Optional,
			Volumes: m.Volumes,
		}
		if m.Build != nil {
			a.Build = &spec.Build{Context: m.Build.Context, Dockerfile: m.Build.Dockerfile, Target: m.Build.Target, Args: m.Build.Args}
		}
		if m.Hooks != nil {
			a.Hooks = &spec.Hooks{Install: m.Hooks.Install, Migrate: m.Hooks.Migrate, Seed: m.Hooks.Seed, Build: m.Hooks.Build}
		}
		for _, u := range m.Uses {
			switch {
			case w.Modules[u] != nil:
				a.Uses = append(a.Uses, u)
			case p.Services[u] != nil:
				p.Services[u].Apps = append(p.Services[u].Apps, n)
			default:
				return nil, fmt.Errorf("modules.%s.uses: unknown %q", n, u)
			}
		}
		if m.Runtime != "" {
			if _, _, ok := catalog.FromRuntime(m.Runtime); ok {
				runtimes[m.Runtime] = append(runtimes[m.Runtime], n)
			} else {
				// A language the catalog does not know: its image, as stage 1 ran it.
				a.Image = workspace.RuntimeImage(m.Runtime)
				if a.Mount == "" {
					a.Mount = spec.DefaultMount
				}
			}
		}
		p.Apps[n] = a
		for i, wk := range m.Workers {
			name := unique(fmt.Sprintf("%s-worker-%d", n, i+1), taken)
			p.Services[name] = &spec.Service{Type: catalog.Queue, Apps: []string{n}, Command: wk.Command, Replicas: wk.Replicas}
		}
		for i, c := range m.Cron {
			name := unique(fmt.Sprintf("%s-cron-%d", n, i+1), taken)
			p.Services[name] = &spec.Service{Type: catalog.Cronjob, Apps: []string{n}, Command: c.Command, Schedule: c.Schedule}
		}
	}

	// One runtime service per runtime and version, shared by the modules on it.
	versions := map[string]int{}
	for rt := range runtimes {
		t, _, _ := catalog.FromRuntime(rt)
		versions[t.Name]++
	}
	keys := make([]string, 0, len(runtimes))
	for rt := range runtimes {
		keys = append(keys, rt)
	}
	sort.Strings(keys)
	for _, rt := range keys {
		t, version, _ := catalog.FromRuntime(rt)
		name := t.Name
		if versions[t.Name] > 1 {
			name += "-" + slug(version)
		}
		name = unique(name, taken)
		p.Services[name] = &spec.Service{Type: t.Name, Version: version, Apps: runtimes[rt]}
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// unique returns name, or name with -runtime / -2… when it is taken, and takes it.
func unique(name string, taken map[string]bool) string {
	out := name
	for i := 2; taken[out]; i++ {
		out = fmt.Sprintf("%s-%d", name, i)
	}
	taken[out] = true
	return out
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

func health(h *workspace.Healthcheck) *spec.Healthcheck {
	if h == nil {
		return nil
	}
	return &spec.Healthcheck{Test: append([]string{}, h.Test...), Interval: h.Interval, Timeout: h.Timeout,
		Retries: h.Retries, StartPeriod: h.StartPeriod, Disable: h.Disable}
}

func ulimits(m map[string]*workspace.Ulimit) map[string]*spec.Ulimit {
	if m == nil {
		return nil
	}
	out := map[string]*spec.Ulimit{}
	for k, u := range m {
		if u != nil {
			out[k] = &spec.Ulimit{Single: u.Single, Soft: u.Soft, Hard: u.Hard}
		}
	}
	return out
}

// Parse reads workspace.yaml v1 and converts it; diagnostics stay with the caller.
func Parse(config []byte) (*spec.Project, *workspace.Result, error) {
	res, err := workspace.Parse(config)
	if err != nil {
		return nil, nil, err
	}
	if res.Workspace == nil {
		return nil, res, fmt.Errorf("workspace.yaml has errors")
	}
	p, err := FromV1(res.Workspace)
	return p, res, err
}
