package stack

import (
	"fmt"
	"sort"
	"strings"

	"github.com/devopus/devopus/internal/workspace"
)

// Isolation is what a feature environment runs itself and what it takes from the base
// environment (Simple Isolation, ADR-0005, P-2).
type Isolation struct {
	// Forked modules are on the feature branch.
	Forked []string
	// Modules run in the feature: the forked ones, the modules that use them (so their
	// logical names reach the fork), and optional modules they need that the base
	// environment does not run.
	Modules []string
	// Services the feature runs itself: with database: clone every service its modules
	// use, with shared only those the base environment does not run.
	Services []string
	// Shared are modules and services of the base environment that the feature's modules
	// use; they reach them by their logical names through the proxy.
	SharedModules  []string
	SharedServices []string
	// NoMigrate are modules whose migrations would run on a service shared with the base
	// environment (docs/architecture/data.md: migrations only on the feature's own data).
	NoMigrate []string
}

// Feature plans a feature that forks the given modules.
func Feature(w *workspace.Workspace, forked []string) (*Isolation, error) {
	if len(forked) == 0 {
		return nil, fmt.Errorf("a feature forks at least one module")
	}
	baseModules, baseServices := Active(w)
	inBase := set(append(append([]string{}, baseModules...), baseServices...))
	run := map[string]bool{}
	for _, f := range forked {
		if w.Modules[f] == nil {
			return nil, fmt.Errorf("unknown module %q (modules: %s)", f, strings.Join(w.ModuleNames(), ", "))
		}
		run[f] = true
	}
	for changed := true; changed; {
		changed = false
		for _, n := range w.ModuleNames() {
			if run[n] {
				// What a module of the feature needs and the base does not run comes along.
				for _, u := range w.Modules[n].Uses {
					if w.Modules[u] != nil && !run[u] && !inBase[u] {
						run[u], changed = true, true
					}
				}
				continue
			}
			if !inBase[n] {
				continue
			}
			for _, u := range w.Modules[n].Uses {
				if run[u] {
					run[n], changed = true, true
					break
				}
			}
		}
	}

	clone := w.Policy().Database == "clone"
	own := map[string]bool{}
	var addService func(name string)
	addService = func(name string) {
		if own[name] {
			return
		}
		own[name] = true
		for _, u := range w.Services[name].Uses {
			addService(u)
		}
	}
	shared := map[string]bool{}
	for n := range run {
		for _, u := range w.Modules[n].Uses {
			switch {
			case w.Modules[u] != nil && !run[u]:
				shared[u] = true
			case w.Services[u] != nil && (clone || !inBase[u]):
				addService(u)
			}
		}
	}
	for n := range run {
		for _, u := range w.Modules[n].Uses {
			if w.Services[u] != nil && !own[u] {
				shared[u] = true
			}
		}
	}

	iso := &Isolation{Forked: sorted(set(forked))}
	for _, n := range w.ModuleNames() {
		if run[n] {
			iso.Modules = append(iso.Modules, n)
			m := w.Modules[n]
			if m.Hooks != nil && m.Hooks.Migrate != "" && usesAny(m.Uses, shared, w) {
				iso.NoMigrate = append(iso.NoMigrate, n)
			}
		}
		if shared[n] {
			iso.SharedModules = append(iso.SharedModules, n)
		}
	}
	for _, n := range w.ServiceNames() {
		if own[n] {
			iso.Services = append(iso.Services, n)
		}
		if shared[n] {
			iso.SharedServices = append(iso.SharedServices, n)
		}
	}
	return iso, nil
}

// Dependents are the modules that run in the feature without being forked.
func (iso *Isolation) Dependents() []string {
	forked := set(iso.Forked)
	var out []string
	for _, m := range iso.Modules {
		if !forked[m] {
			out = append(out, m)
		}
	}
	return out
}

// usesAny reports whether a module uses one of the shared services.
func usesAny(uses []string, shared map[string]bool, w *workspace.Workspace) bool {
	for _, u := range uses {
		if shared[u] && w.Services[u] != nil {
			return true
		}
	}
	return false
}

func set(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, s := range list {
		out[s] = true
	}
	return out
}

func sorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
