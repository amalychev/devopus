package stack

import (
	"fmt"
	"sort"
	"strings"

	"github.com/amalychev/devopus/internal/spec"
)

// Isolation is what a feature environment runs itself and what it takes from the base
// environment (Simple Isolation, ADR-0005, P-2).
type Isolation struct {
	// Forked applications are on the feature branch.
	Forked []string
	// Apps run in the feature: the forked ones, the applications that use them (so their
	// logical names reach the fork), and optional applications they need that the base
	// environment does not run.
	Apps []string
	// Services the feature runs itself: with database: clone every container service
	// attached to its applications, with shared only those the base environment does
	// not run.
	Services []string
	// Shared are applications and services of the base environment that the feature's
	// applications use; they reach them by their logical names through the proxy.
	SharedApps     []string
	SharedServices []string
	// NoMigrate are applications whose migrations would run on a service shared with the
	// base environment (docs/architecture/data.md: migrations only on the feature's own
	// data).
	NoMigrate []string
}

// Feature plans a feature that forks the given applications.
func Feature(p *spec.Project, forked []string) (*Isolation, error) {
	if len(forked) == 0 {
		return nil, fmt.Errorf("a feature forks at least one application")
	}
	baseApps, baseServices := Active(p)
	inBase := set(append(append([]string{}, baseApps...), baseServices...))
	run := map[string]bool{}
	for _, f := range forked {
		if p.Apps[f] == nil {
			return nil, fmt.Errorf("unknown application %q (applications: %s)", f, strings.Join(p.AppNames(), ", "))
		}
		run[f] = true
	}
	for changed := true; changed; {
		changed = false
		for _, n := range p.AppNames() {
			if run[n] {
				// What an application of the feature needs and the base does not run comes along.
				for _, u := range p.Needs(n) {
					if p.Apps[u] != nil && !run[u] && !inBase[u] {
						run[u], changed = true, true
					}
				}
				continue
			}
			if !inBase[n] {
				continue
			}
			for _, u := range p.Needs(n) {
				if run[u] {
					run[n], changed = true, true
					break
				}
			}
		}
	}

	clone := p.Policy().Database == "clone"
	own := map[string]bool{}
	var addService func(name string)
	addService = func(name string) {
		if own[name] {
			return
		}
		own[name] = true
		for _, u := range p.Needs(name) {
			if p.Services[u] != nil {
				addService(u)
			}
		}
	}
	shared := map[string]bool{}
	for n := range run {
		for _, u := range p.Needs(n) {
			switch {
			case p.Apps[u] != nil && !run[u]:
				shared[u] = true
			case p.Services[u] != nil && (clone || !inBase[u]):
				addService(u)
			}
		}
	}
	for n := range run {
		for _, u := range p.Needs(n) {
			if p.Services[u] != nil && !own[u] {
				shared[u] = true
			}
		}
	}

	iso := &Isolation{Forked: sorted(set(forked))}
	for _, n := range p.AppNames() {
		if run[n] {
			iso.Apps = append(iso.Apps, n)
			a := p.Apps[n]
			if a.Hooks != nil && a.Hooks.Migrate != "" && usesAny(p.Needs(n), shared, p) {
				iso.NoMigrate = append(iso.NoMigrate, n)
			}
		}
		if shared[n] {
			iso.SharedApps = append(iso.SharedApps, n)
		}
	}
	for _, n := range p.ServiceNames() {
		if own[n] {
			iso.Services = append(iso.Services, n)
		}
		if shared[n] {
			iso.SharedServices = append(iso.SharedServices, n)
		}
	}
	return iso, nil
}

// Dependents are the applications that run in the feature without being forked.
func (iso *Isolation) Dependents() []string {
	forked := set(iso.Forked)
	var out []string
	for _, a := range iso.Apps {
		if !forked[a] {
			out = append(out, a)
		}
	}
	return out
}

// usesAny reports whether an application needs one of the shared services.
func usesAny(needs []string, shared map[string]bool, p *spec.Project) bool {
	for _, u := range needs {
		if shared[u] && p.Services[u] != nil {
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
