// Package store keeps the device's desired state (ADR-0001). In stage 1 the CLI
// changes it through the local API; from stage 2 the cloud sends it.
package store

import (
	"bytes"
	"context"
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
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	agentv2 "github.com/amalychev/devopus/gen/devopus/agent/v2"
	"github.com/amalychev/devopus/internal/agent/git"
	"github.com/amalychev/devopus/internal/importer"
	"github.com/amalychev/devopus/internal/spec"
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

// Up wants a project's environments running: the base one, and the features 'down'
// stopped. It returns the ID of the base environment.
func (s *Store) Up(name string) (envID string, rev uint64, err error) {
	rev, err = s.update(func(st *agentv2.DesiredState) error {
		base, err := registered(st, name)
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

// registered is a project the device has, released from 'devopus down', with its base
// environment. Projects come from the workspace server (SyncServer); those registered
// from folders before R10 keep running as they were.
func registered(st *agentv2.DesiredState, name string) (*agentv2.EnvironmentSpec, error) {
	p := findProject(st, name)
	if p == nil {
		return nil, fmt.Errorf("%w %q: the projects of the current workspace come from its server ('devopus project list')", ErrUnknownProject, name)
	}
	if p.Orphaned {
		return nil, fmt.Errorf("%w: %s is no longer given to this device by its workspace", ErrUnknownProject, name)
	}
	p.Held = false
	return ensureBase(st, name), nil
}

func ensureBase(st *agentv2.DesiredState, name string) *agentv2.EnvironmentSpec {
	env := baseEnv(st, name)
	if env == nil {
		env = &agentv2.EnvironmentSpec{Id: NewID(), Project: name, Kind: agentv2.EnvironmentKind_ENVIRONMENT_KIND_BASE}
		st.Environments = append(st.Environments, env)
	}
	return env
}

// ParseProject reads a registered project as the agent runs it: the spec the
// workspace server sent with the personal overrides of devopus.yaml applied, or
// workspace.yaml v1 of a folder.
func ParseProject(p *agentv2.ProjectSpec) (*spec.Project, error) {
	proj, _, err := EffectiveProject(p)
	return proj, err
}

// OverrideFile is where a project's devopus.yaml is on this device.
func OverrideFile(p *agentv2.ProjectSpec) string { return filepath.Join(p.Root, spec.OverrideFile) }

// EffectiveProject is ParseProject with what devopus.yaml did: the overrides applied,
// and the ones refused with their reasons (ADR-0013). The file must be ignored by git:
// it is personal, and must not reach the team through the repository.
func EffectiveProject(p *agentv2.ProjectSpec) (*spec.Project, spec.OverrideResult, error) {
	if len(p.Spec) == 0 {
		proj, _, err := importer.Parse(p.Config)
		return proj, spec.OverrideResult{}, err
	}
	base, err := spec.ParseYAML(p.Spec)
	if err != nil {
		return nil, spec.OverrideResult{}, err
	}
	file := OverrideFile(p)
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) || p.Root == "" {
		return base, spec.OverrideResult{}, nil
	}
	if err != nil {
		return base, spec.OverrideResult{Rejected: []string{file + ": " + err.Error()}}, nil
	}
	o, err := spec.ParseOverride(data)
	if err != nil {
		return base, spec.OverrideResult{Rejected: []string{err.Error()}}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if repo, ignored := git.Ignored(ctx, p.Root, spec.OverrideFile); repo && !ignored {
		return base, spec.OverrideResult{Rejected: []string{fmt.Sprintf("%s is not in .gitignore: it is personal and must not reach the repository; add it, and the agent applies it", file)}}, nil
	}
	why := spec.PolicyReason(p.OverridesAllowed)
	allowed := p.OverridesAllowed
	if !p.OverridesPermitted {
		why, allowed = "you do not have overrides.use in this project; its teamlead or the workspace admin can tell why", nil
	}
	out, r := base.ApplyOverride(o, allowed, why)
	return out, r, nil
}

// ServerProject is a project the workspace server gives this device.
type ServerProject struct {
	Name    string
	Spec    []byte
	Version int32
	// Secrets are encrypted to the workspace's key.
	Secrets map[string][]byte
	// OverridesAllowed and OverridesPermitted say what devopus.yaml may change.
	OverridesAllowed   []string
	OverridesPermitted bool
}

// ServerKey is the workspace's key as this device has it: wrapped for it.
type ServerKey struct {
	Generation int32
	Wrapped    []byte
}

// SyncServer makes the server's projects of a workspace the device's: new ones come
// up at once (a new member does nothing to get them), changed ones get their new
// settings, the ones the server no longer gives — removed, access ended, another
// workspace — are stopped with their data kept. A project held with 'devopus down'
// stays stopped. A project of a folder with the same name is left alone; its name is
// returned. The revision moves only when something changed.
func (s *Store) SyncServer(workspaceID, adminEmail string, key ServerKey, projects []ServerProject, root func(name string) string) (rev uint64, conflicts []string, err error) {
	rev, err = s.updateIfChanged(func(st *agentv2.DesiredState) error {
		conflicts = nil // fn runs twice: once to see whether anything changes
		given := map[string]bool{}
		for _, sp := range projects {
			p := findProject(st, sp.Name)
			if p != nil && p.WorkspaceId == "" {
				conflicts = append(conflicts, sp.Name)
				continue
			}
			given[sp.Name] = true
			start := false
			if p == nil {
				p = &agentv2.ProjectSpec{Name: sp.Name, Root: root(sp.Name)}
				st.Projects = append(st.Projects, p)
				start = true
			}
			start = start || p.Orphaned
			sum := sha256.Sum256(sp.Spec)
			p.WorkspaceId, p.Spec, p.ConfigVersion, p.AdminEmail = workspaceID, sp.Spec, sp.Version, adminEmail
			p.Config, p.ConfigHash, p.Orphaned = nil, hex.EncodeToString(sum[:]), false
			p.Secrets, p.KeyWrap, p.KeyGeneration = sp.Secrets, key.Wrapped, key.Generation
			p.OverridesAllowed, p.OverridesPermitted = sp.OverridesAllowed, sp.OverridesPermitted
			base := ensureBase(st, sp.Name)
			if start && !p.Held {
				base.Phase = agentv2.DesiredPhase_DESIRED_PHASE_RUNNING
			}
		}
		for _, p := range st.Projects {
			if p.WorkspaceId == "" || given[p.Name] || p.Orphaned {
				continue
			}
			p.Orphaned = true
			for _, e := range st.Environments {
				if e.Project == p.Name {
					e.Phase = agentv2.DesiredPhase_DESIRED_PHASE_STOPPED
				}
			}
		}
		return nil
	})
	return rev, conflicts, err
}

// updateIfChanged is update that keeps the revision when fn changed nothing.
func (s *Store) updateIfChanged(fn func(st *agentv2.DesiredState) error) (uint64, error) {
	s.mu.Lock()
	next := proto.Clone(s.state).(*agentv2.DesiredState)
	if err := fn(next); err != nil {
		s.mu.Unlock()
		return 0, err
	}
	same := proto.Equal(next, s.state)
	rev := s.state.Revision
	s.mu.Unlock()
	if same {
		return rev, nil
	}
	return s.update(fn)
}

// Link sets the folder of a server project's path applications on this device.
func (s *Store) Link(name, dir string) (uint64, error) {
	return s.update(func(st *agentv2.DesiredState) error {
		p := findProject(st, name)
		if p == nil || p.WorkspaceId == "" {
			return fmt.Errorf("%w %q of the workspace", ErrUnknownProject, name)
		}
		p.Root = dir
		return nil
	})
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

// CreateFeature wants the project's base environment running
// (the feature reaches the base's applications and services) and adds the feature.
func (s *Store) CreateFeature(project string, f NewFeature) (feature, base *agentv2.EnvironmentSpec, rev uint64, err error) {
	rev, err = s.update(func(st *agentv2.DesiredState) error {
		b, err := registered(st, project)
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

// AddFeatureApps forks more applications in a feature.
func (s *Store) AddFeatureApps(project, feature string, applications []string) (env *agentv2.EnvironmentSpec, rev uint64, err error) {
	rev, err = s.update(func(st *agentv2.DesiredState) error {
		b, err := registered(st, project)
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
// everything it owns. A project of the workspace server stays registered and held:
// the server's changes do not start it until 'devopus up'.
func (s *Store) Down(name string, purge bool) (uint64, error) {
	return s.update(func(st *agentv2.DesiredState) error {
		p := findProject(st, name)
		if p == nil {
			return fmt.Errorf("%w %q", ErrUnknownProject, name)
		}
		if p.WorkspaceId != "" {
			p.Held = true
			if purge {
				st.Environments = filter(st.Environments, func(e *agentv2.EnvironmentSpec) bool { return e.Project != name })
				return nil
			}
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
