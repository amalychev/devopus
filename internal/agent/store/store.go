// Package store keeps the device's desired state (ADR-0001). In stage 1 the CLI
// changes it through the local API; from stage 2 the cloud sends it.
package store

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	agentv2 "github.com/amalychev/devopus/gen/devopus/agent/v2"
)

// Store is the desired state persisted as JSON. Every change bumps the revision,
// is written atomically and wakes the reconciler.
type Store struct {
	path    string
	mu      sync.Mutex
	state   *agentv2.DesiredState
	changed chan struct{}
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, state: &agentv2.DesiredState{}, changed: make(chan struct{}, 1)}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	if err := protojson.Unmarshal(migrate(data), s.state); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// migrate renames the fields of older state files: forkedModules of agent/v1 is
// forkedApps since applications replaced modules (R2 of docs/refactoring-plan.md).
func migrate(data []byte) []byte {
	return bytes.ReplaceAll(data, []byte(`"forkedModules":`), []byte(`"forkedApps":`))
}

// Changed signals after every change; one pending signal covers many changes.
func (s *Store) Changed() <-chan struct{} { return s.changed }

// Snapshot is a copy of the desired state, safe to read while the store changes.
func (s *Store) Snapshot() *agentv2.DesiredState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return proto.Clone(s.state).(*agentv2.DesiredState)
}

// update applies fn to a copy and commits it only if fn succeeds and the file is written.
func (s *Store) update(fn func(st *agentv2.DesiredState) error) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := proto.Clone(s.state).(*agentv2.DesiredState)
	if err := fn(next); err != nil {
		return 0, err
	}
	next.Revision = s.state.Revision + 1
	data, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(next)
	if err != nil {
		return 0, err
	}
	if err := writeAtomic(s.path, data); err != nil {
		return 0, err
	}
	s.state = next
	select {
	case s.changed <- struct{}{}:
	default:
	}
	return next.Revision, nil
}

// ErrProjectConflict: another folder already registered a project with this name.
var ErrProjectConflict = errors.New("project name is taken")

// ErrUnknownProject: no project with this name.
var ErrUnknownProject = errors.New("unknown project")

// Up registers or updates a project and wants its environments running: the base
// one, and the features 'down' stopped. It returns the ID of the base environment.
func (s *Store) Up(name, root string, config []byte) (envID string, rev uint64, err error) {
	rev, err = s.update(func(st *agentv2.DesiredState) error {
		base, err := register(st, name, root, config)
		if err != nil {
			return err
		}
		for _, e := range st.Environments {
			if e.Project == name {
				e.Phase = agentv2.DesiredPhase_DESIRED_PHASE_RUNNING
			}
		}
		envID = base.Id
		return nil
	})
	return envID, rev, err
}

// register adds or updates a project and makes sure it has a base environment.
func register(st *agentv2.DesiredState, name, root string, config []byte) (*agentv2.EnvironmentSpec, error) {
	sum := sha256.Sum256(config)
	p := findProject(st, name)
	if p != nil && p.Root != root {
		return nil, fmt.Errorf("%w: %q is already used by %s", ErrProjectConflict, name, p.Root)
	}
	if p == nil {
		p = &agentv2.ProjectSpec{Name: name, Root: root}
		st.Projects = append(st.Projects, p)
	}
	p.Config, p.ConfigHash = config, hex.EncodeToString(sum[:])
	env := baseEnv(st, name)
	if env == nil {
		env = &agentv2.EnvironmentSpec{Id: NewID(), Project: name, Kind: agentv2.EnvironmentKind_ENVIRONMENT_KIND_BASE}
		st.Environments = append(st.Environments, env)
	}
	return env, nil
}

// ErrFeatureExists: the project already has a feature with this name.
var ErrFeatureExists = errors.New("feature exists")

// ErrUnknownFeature: no feature with this name, slug or ID.
var ErrUnknownFeature = errors.New("unknown feature")

// NewFeature is a feature to create.
type NewFeature struct {
	Name   string
	Branch string
	Apps   []string
	// Reserved are names the slug must not take: the project's exposed web UIs live
	// on <expose>.<domain>, as features do on <slug>.<domain>.
	Reserved []string
}

// CreateFeature registers or updates the project, wants its base environment running
// (the feature reaches the base's applications and services) and adds the feature.
func (s *Store) CreateFeature(project, root string, config []byte, f NewFeature) (feature, base *agentv2.EnvironmentSpec, rev uint64, err error) {
	rev, err = s.update(func(st *agentv2.DesiredState) error {
		b, err := register(st, project, root, config)
		if err != nil {
			return err
		}
		b.Phase = agentv2.DesiredPhase_DESIRED_PHASE_RUNNING
		if e := findFeature(st, project, f.Name); e != nil {
			return fmt.Errorf("%w: %s already has feature %q (%s); 'devopus feature add' forks more applications in it", ErrFeatureExists, project, e.Name, e.Slug)
		}
		slug, err := uniqueSlug(st, project, f.Name, f.Reserved)
		if err != nil {
			return err
		}
		env := &agentv2.EnvironmentSpec{
			Id: NewID(), Project: project, Kind: agentv2.EnvironmentKind_ENVIRONMENT_KIND_FEATURE,
			Name: f.Name, Slug: slug, Branch: f.Branch, ForkedApps: sortedSet(f.Apps),
			Phase: agentv2.DesiredPhase_DESIRED_PHASE_RUNNING,
		}
		st.Environments = append(st.Environments, env)
		feature, base = proto.Clone(env).(*agentv2.EnvironmentSpec), proto.Clone(b).(*agentv2.EnvironmentSpec)
		return nil
	})
	return feature, base, rev, err
}

// AddFeatureApps updates the project and forks more applications in a feature.
func (s *Store) AddFeatureApps(project, root string, config []byte, feature string, applications []string) (env *agentv2.EnvironmentSpec, rev uint64, err error) {
	rev, err = s.update(func(st *agentv2.DesiredState) error {
		b, err := register(st, project, root, config)
		if err != nil {
			return err
		}
		e := findFeature(st, project, feature)
		if e == nil {
			return fmt.Errorf("%w %q in %s", ErrUnknownFeature, feature, project)
		}
		e.ForkedApps = sortedSet(append(e.ForkedApps, applications...))
		e.Phase, b.Phase = agentv2.DesiredPhase_DESIRED_PHASE_RUNNING, agentv2.DesiredPhase_DESIRED_PHASE_RUNNING
		env = proto.Clone(e).(*agentv2.EnvironmentSpec)
		return nil
	})
	return env, rev, err
}

// Feature finds a feature of a project by name, slug or ID.
func (s *Store) Feature(project, feature string) *agentv2.EnvironmentSpec {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := findFeature(s.state, project, feature); e != nil {
		return proto.Clone(e).(*agentv2.EnvironmentSpec)
	}
	return nil
}

// RemoveFeature removes a feature from the desired state; the reconciler removes its
// resources. check runs under the store's lock, so the feature cannot change between
// the check and the removal.
func (s *Store) RemoveFeature(project, feature string, check func(*agentv2.EnvironmentSpec) error) (env *agentv2.EnvironmentSpec, rev uint64, err error) {
	rev, err = s.update(func(st *agentv2.DesiredState) error {
		e := findFeature(st, project, feature)
		if e == nil {
			return fmt.Errorf("%w %q in %s", ErrUnknownFeature, feature, project)
		}
		if check != nil {
			if err := check(e); err != nil {
				return err
			}
		}
		env = proto.Clone(e).(*agentv2.EnvironmentSpec)
		st.Environments = filter(st.Environments, func(x *agentv2.EnvironmentSpec) bool { return x.Id != e.Id })
		return nil
	})
	return env, rev, err
}

// ResetData bumps the data reset numbers of the services of an environment: a feature
// (by name, slug or ID), or the base environment when env is empty.
func (s *Store) ResetData(project, env string, services []string) (*agentv2.EnvironmentSpec, uint64, error) {
	var out *agentv2.EnvironmentSpec
	rev, err := s.update(func(st *agentv2.DesiredState) error {
		if findProject(st, project) == nil {
			return fmt.Errorf("%w %q", ErrUnknownProject, project)
		}
		e := baseEnv(st, project)
		if env != "" {
			if e = findFeature(st, project, env); e == nil {
				return fmt.Errorf("%w %q in %s", ErrUnknownFeature, env, project)
			}
		}
		if e == nil {
			return fmt.Errorf("%w %q", ErrUnknownProject, project)
		}
		if e.DataResets == nil {
			e.DataResets = map[string]uint64{}
		}
		for _, svc := range services {
			e.DataResets[svc]++
		}
		out = proto.Clone(e).(*agentv2.EnvironmentSpec)
		return nil
	})
	return out, rev, err
}

func findFeature(st *agentv2.DesiredState, project, key string) *agentv2.EnvironmentSpec {
	for _, e := range st.Environments {
		if e.Project == project && e.Kind == agentv2.EnvironmentKind_ENVIRONMENT_KIND_FEATURE &&
			(strings.EqualFold(e.Name, key) || e.Slug == key || e.Id == key) {
			return e
		}
	}
	return nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slug makes the URL label of a feature name: PAY-123 → pay-123, feature/a_b → a-b.
func Slug(name string) string {
	s := nonSlug.ReplaceAllString(strings.ToLower(name), "-")
	s = strings.Trim(s, "-")
	if len(s) > 50 {
		s = strings.TrimRight(s[:50], "-")
	}
	return s
}

// uniqueSlug resolves collisions like a-b and a_b with a suffix; resources never
// collide anyway, they carry the ID (docs/architecture/docker-isolation.md).
func uniqueSlug(st *agentv2.DesiredState, project, name string, reserved []string) (string, error) {
	base := Slug(name)
	if base == "" {
		return "", fmt.Errorf("feature name %q has no letters or digits for its URL", name)
	}
	taken := map[string]bool{}
	for _, r := range reserved {
		taken[r] = true
	}
	for _, e := range st.Environments {
		if e.Project == project && e.Slug != "" {
			taken[e.Slug] = true
		}
	}
	slug := base
	for i := 2; taken[slug]; i++ {
		slug = fmt.Sprintf("%s-%d", base, i)
	}
	return slug, nil
}

func sortedSet(list []string) []string {
	out := slices.Clone(list)
	slices.Sort(out)
	return slices.Compact(out)
}

// Down stops the project's environments, or with purge removes the project and
// everything it owns.
func (s *Store) Down(name string, purge bool) (uint64, error) {
	return s.update(func(st *agentv2.DesiredState) error {
		if findProject(st, name) == nil {
			return fmt.Errorf("%w %q", ErrUnknownProject, name)
		}
		if purge {
			st.Projects = filter(st.Projects, func(p *agentv2.ProjectSpec) bool { return p.Name != name })
			st.Environments = filter(st.Environments, func(e *agentv2.EnvironmentSpec) bool { return e.Project != name })
			return nil
		}
		for _, e := range st.Environments {
			if e.Project == name {
				e.Phase = agentv2.DesiredPhase_DESIRED_PHASE_STOPPED
			}
		}
		return nil
	})
}

// SetProxy changes the device's proxy settings. Turning coexistence on or off, or
// asking for a restart, bumps the proxy generation: the proxy is recreated and probes
// 80/443 again, which is the point of moving a dev server off them.
func (s *Store) SetProxy(coexistHTTP, coexistHTTPS uint32, restart bool) (uint64, error) {
	return s.update(func(st *agentv2.DesiredState) error {
		if st.Device == nil {
			st.Device = &agentv2.DeviceSettings{}
		}
		d := st.Device
		changed := d.CoexistHttpPort != coexistHTTP || d.CoexistHttpsPort != coexistHTTPS
		d.CoexistHttpPort, d.CoexistHttpsPort = coexistHTTP, coexistHTTPS
		if changed || restart {
			d.ProxyGeneration++
		}
		return nil
	})
}

func findProject(st *agentv2.DesiredState, name string) *agentv2.ProjectSpec {
	for _, p := range st.Projects {
		if p.Name == name {
			return p
		}
	}
	return nil
}

func baseEnv(st *agentv2.DesiredState, project string) *agentv2.EnvironmentSpec {
	for _, e := range st.Environments {
		if e.Project == project && e.Kind == agentv2.EnvironmentKind_ENVIRONMENT_KIND_BASE {
			return e
		}
	}
	return nil
}

func filter[T any](list []T, keep func(T) bool) []T {
	var out []T
	for _, x := range list {
		if keep(x) {
			out = append(out, x)
		}
	}
	return out
}

// NewID is a short stable ID for Docker resources: 6 characters without look-alikes.
func NewID() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
