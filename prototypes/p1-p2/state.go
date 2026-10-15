package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// State is the agent's local desired state. In the real agent it comes from the cloud
// (specs/agent-protocol.md); here it is a JSON file under DEVOPUS_HOME.
type State struct {
	Project    string `json:"project"`
	Root       string `json:"root"` // repository root with workspace.yaml
	ConfigPath string `json:"config_path"`
	HTTPPort   int    `json:"http_port"`
	HTTPSPort  int    `json:"https_port"`
	NextNet    int    `json:"next_net"` // next /24 in 10.213.0.0/16
	Envs       []*Env `json:"envs"`
}

type Env struct {
	ID       string   `json:"id"` // stable, used in every Docker resource
	Kind     string   `json:"kind"`
	Slug     string   `json:"slug"` // only in URLs
	Branch   string   `json:"branch"`
	Code     string   `json:"code"`     // checkout the modules are mounted from
	Forked   []string `json:"forked"`   // modules the developer chose to change
	Modules  []string `json:"modules"`  // modules running in this env (forked + dependents)
	Services []string `json:"services"` // services owned by this env
	Subnet   string   `json:"subnet"`
	EnvoyIP  string   `json:"envoy_ip"`
}

const (
	KindBase    = "base"
	KindFeature = "feature"
)

func homeDir() string {
	if h := os.Getenv("DEVOPUS_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".devopus-proto")
}

func statePath() string { return filepath.Join(homeDir(), "state.json") }

var errNoState = errors.New("no project is up; run `devopus up` in a folder with workspace.yaml")

func LoadState() (*State, error) {
	data, err := os.ReadFile(statePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNoState
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", statePath(), err)
	}
	return &st, nil
}

func (st *State) Save() error {
	if err := os.MkdirAll(homeDir(), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(st, "", "  ")
	tmp := statePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath())
}

func (st *State) Base() *Env {
	for _, e := range st.Envs {
		if e.Kind == KindBase {
			return e
		}
	}
	return nil
}

func (st *State) FindFeature(nameOrSlug string) *Env {
	slug := slugify(nameOrSlug)
	for _, e := range st.Envs {
		if e.Kind == KindFeature && (e.Slug == slug || e.ID == nameOrSlug) {
			return e
		}
	}
	return nil
}

func (st *State) Remove(env *Env) {
	out := st.Envs[:0]
	for _, e := range st.Envs {
		if e != env {
			out = append(out, e)
		}
	}
	st.Envs = out
}

// AllocNet hands out 10.213.N.0/24 networks; Envoy takes .254 in each.
func (st *State) AllocNet() (subnet, envoyIP string, err error) {
	if st.NextNet > 255 {
		return "", "", errors.New("address pool 10.213.0.0/16 is exhausted")
	}
	n := st.NextNet
	st.NextNet++
	return fmt.Sprintf("10.213.%d.0/24", n), fmt.Sprintf("10.213.%d.254", n), nil
}

func (st *State) Host(env *Env, ws *Workspace) string {
	if env.Kind == KindBase {
		return ws.Domain
	}
	return env.Slug + "." + ws.Domain
}

func (st *State) URL(env *Env, ws *Workspace) string {
	port := ""
	if st.HTTPSPort != 443 {
		port = fmt.Sprintf(":%d", st.HTTPSPort)
	}
	return "https://" + st.Host(env, ws) + port
}

func (st *State) ComposeProject(env *Env) string { return st.Project + "-env-" + env.ID }
func (st *State) NetworkName(env *Env) string    { return st.ComposeProject(env) }
func (st *State) Container(env *Env, name string) string {
	return st.ComposeProject(env) + "-" + name
}

func newID() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.ToLower(s)
	s = strings.TrimPrefix(s, "feature/")
	return strings.Trim(nonSlug.ReplaceAllString(s, "-"), "-")
}

// uniqueSlug resolves collisions like feature/a-b vs feature/a_b (docs/architecture/docker-isolation.md).
func (st *State) uniqueSlug(name string) string {
	base := slugify(name)
	taken := map[string]bool{}
	for _, e := range st.Envs {
		taken[e.Slug] = true
	}
	slug := base
	for i := 2; taken[slug]; i++ {
		slug = fmt.Sprintf("%s-%d", base, i)
	}
	return slug
}

// PlanFeature applies Simple Isolation: the feature runs the forked modules plus every
// module that transitively uses one of them, so their logical names resolve to the fork.
// With database: clone, services used by forked modules get their own instance.
func PlanFeature(ws *Workspace, forked []string) (modules, services []string, err error) {
	set := map[string]bool{}
	for _, f := range forked {
		if _, ok := ws.Modules[f]; !ok {
			return nil, nil, fmt.Errorf("unknown module %q (have: %s)", f, strings.Join(ws.ModuleNames(), ", "))
		}
		set[f] = true
	}
	for changed := true; changed; {
		changed = false
		for name, m := range ws.Modules {
			if set[name] {
				continue
			}
			for _, u := range m.Uses {
				if set[u] {
					set[name] = true
					changed = true
					break
				}
			}
		}
	}
	modules = sortedKeys(set)

	if ws.Features.Database == "clone" {
		owned := map[string]bool{}
		for _, f := range forked {
			for _, u := range ws.Modules[f].Uses {
				if _, ok := ws.Services[u]; ok {
					owned[u] = true
				}
			}
		}
		services = sortedKeys(owned)
	}
	sort.Strings(modules)
	return modules, services, nil
}

// SharedFrom lists what the feature takes from the base environment, for the
// "shared with main" hint (docs/architecture/data.md).
func SharedFrom(ws *Workspace, env *Env) (modules, services []string) {
	in := func(list []string, s string) bool {
		for _, x := range list {
			if x == s {
				return true
			}
		}
		return false
	}
	for _, m := range ws.ModuleNames() {
		if !in(env.Modules, m) {
			modules = append(modules, m)
		}
	}
	for _, s := range ws.ServiceNames() {
		if !in(env.Services, s) {
			services = append(services, s)
		}
	}
	return modules, services
}
