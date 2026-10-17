package spec

import (
	"bytes"
	"fmt"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// OverrideFile is the personal file of overrides in a project's folder (ADR-0013). It
// stays on the developer's machine: the agent refuses it unless git ignores it.
const OverrideFile = "devopus.yaml"

// Override is what devopus.yaml may change: a subset of the project.
type Override struct {
	Apps     map[string]*AppOverride     `yaml:"apps,omitempty"`
	Services map[string]*ServiceOverride `yaml:"services,omitempty"`
}

type AppOverride struct {
	Env  map[string]string `yaml:"env,omitempty"`
	Port int               `yaml:"port,omitempty"`
}

type ServiceOverride struct {
	Version string            `yaml:"version,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
	Port    int               `yaml:"port,omitempty"`
}

// Kinds of overrides a project's policy allows (overrides.allowed).
const (
	AllowAll      = "*"        // everything devopus.yaml can say
	AllowVersions = "versions" // services' versions
	AllowEnv      = "env"      // any variable; env:NAME allows one
	AllowPorts    = "ports"    // ports of applications and services
)

// ParseOverride reads devopus.yaml; unknown keys are errors.
func ParseOverride(data []byte) (*Override, error) {
	var o Override
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&o); err != nil && err.Error() != "EOF" {
		return nil, fmt.Errorf("%s: %w (it may set apps.<name>.env/port and services.<name>.version/env/port)", OverrideFile, err)
	}
	return &o, nil
}

// OverrideResult is what an override did: the changes applied and the ones refused,
// each with its reason.
type OverrideResult struct {
	Applied  []string
	Rejected []string
}

func allows(allowed []string, kind, env string) bool {
	if slices.Contains(allowed, AllowAll) || slices.Contains(allowed, kind) {
		return true
	}
	return env != "" && slices.Contains(allowed, "env:"+env)
}

// ApplyOverride changes a copy of the project by an override, as far as allowed
// permits: the kinds of the project's policy (or * for admins and teamleads, nothing
// without overrides.use). A change the engine cannot run refuses the whole override.
func (p *Project) ApplyOverride(o *Override, allowed []string, why string) (*Project, OverrideResult) {
	out := p.Clone()
	var r OverrideResult
	refuse := func(what, reason string) { r.Rejected = append(r.Rejected, what+": "+reason) }
	try := func(what, kind, env string, apply func()) {
		if allows(allowed, kind, env) {
			apply()
			r.Applied = append(r.Applied, what)
			return
		}
		refuse(what, why)
	}
	setEnv := func(prefix string, env *map[string]string, values map[string]string) {
		for _, k := range sortedKeys(values) {
			v := values[k]
			try(fmt.Sprintf("%s.env.%s=%s", prefix, k, v), AllowEnv, k, func() {
				if *env == nil {
					*env = map[string]string{}
				}
				(*env)[k] = v
			})
		}
	}
	for _, n := range sortedKeys(o.Services) {
		so, s := o.Services[n], out.Services[n]
		prefix := "services." + n
		if s == nil {
			refuse(prefix, "the project has no such service")
			continue
		}
		if so.Version != "" && so.Version != s.Version {
			try(fmt.Sprintf("%s.version: %s → %s", prefix, s.Version, so.Version), AllowVersions, "", func() { s.Version = so.Version })
		}
		if so.Port != 0 && so.Port != s.Port {
			try(fmt.Sprintf("%s.port: %d → %d", prefix, s.Port, so.Port), AllowPorts, "", func() { s.Port = so.Port })
		}
		setEnv(prefix, &s.Env, so.Env)
	}
	for _, n := range sortedKeys(o.Apps) {
		ao, a := o.Apps[n], out.Apps[n]
		prefix := "apps." + n
		if a == nil {
			refuse(prefix, "the project has no such application")
			continue
		}
		if ao.Port != 0 && ao.Port != a.Port {
			try(fmt.Sprintf("%s.port: %d → %d", prefix, a.AppPort(), ao.Port), AllowPorts, "", func() { a.Port = ao.Port })
		}
		setEnv(prefix, &a.Env, ao.Env)
	}
	if len(r.Applied) > 0 {
		if err := out.Validate(); err != nil {
			for _, a := range r.Applied {
				refuse(a, "the project would not run with it: "+err.Error())
			}
			r.Applied = nil
			return p.Clone(), r
		}
	}
	sort.Strings(r.Applied)
	sort.Strings(r.Rejected)
	return out, r
}

// PolicyReason explains a refusal by the policy: what it allows.
func PolicyReason(allowed []string) string {
	if len(allowed) == 0 {
		return "the project's policy allows no personal overrides (a teamlead sets it: devopus project set <project> --allow-overrides versions,env)"
	}
	return "not allowed by the project's policy (overrides.allowed: " + strings.Join(allowed, ", ") + ")"
}
