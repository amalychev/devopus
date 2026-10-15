// Package store keeps the device's desired state (ADR-0001). In stage 1 the CLI
// changes it through the local API; from stage 2 the cloud sends it.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/docklet/docklet/gen/docklet/agent/v1"
)

// Store is the desired state persisted as JSON. Every change bumps the revision,
// is written atomically and wakes the reconciler.
type Store struct {
	path    string
	mu      sync.Mutex
	state   *agentv1.DesiredState
	changed chan struct{}
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, state: &agentv1.DesiredState{}, changed: make(chan struct{}, 1)}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	if err := protojson.Unmarshal(data, s.state); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Changed signals after every change; one pending signal covers many changes.
func (s *Store) Changed() <-chan struct{} { return s.changed }

// Snapshot is a copy of the desired state, safe to read while the store changes.
func (s *Store) Snapshot() *agentv1.DesiredState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return proto.Clone(s.state).(*agentv1.DesiredState)
}

// update applies fn to a copy and commits it only if fn succeeds and the file is written.
func (s *Store) update(fn func(st *agentv1.DesiredState) error) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := proto.Clone(s.state).(*agentv1.DesiredState)
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

// Up registers or updates a project and wants its base environment running.
// It returns the ID of the base environment.
func (s *Store) Up(name, root string, config []byte) (envID string, rev uint64, err error) {
	sum := sha256.Sum256(config)
	rev, err = s.update(func(st *agentv1.DesiredState) error {
		p := findProject(st, name)
		if p != nil && p.Root != root {
			return fmt.Errorf("%w: %q is already used by %s", ErrProjectConflict, name, p.Root)
		}
		if p == nil {
			p = &agentv1.ProjectSpec{Name: name, Root: root}
			st.Projects = append(st.Projects, p)
		}
		p.Config, p.ConfigHash = config, hex.EncodeToString(sum[:])

		env := baseEnv(st, name)
		if env == nil {
			env = &agentv1.EnvironmentSpec{Id: NewID(), Project: name, Kind: agentv1.EnvironmentKind_ENVIRONMENT_KIND_BASE}
			st.Environments = append(st.Environments, env)
		}
		env.Phase = agentv1.DesiredPhase_DESIRED_PHASE_RUNNING
		envID = env.Id
		return nil
	})
	return envID, rev, err
}

// Down stops the project's environments, or with purge removes the project and
// everything it owns.
func (s *Store) Down(name string, purge bool) (uint64, error) {
	return s.update(func(st *agentv1.DesiredState) error {
		if findProject(st, name) == nil {
			return fmt.Errorf("%w %q", ErrUnknownProject, name)
		}
		if purge {
			st.Projects = filter(st.Projects, func(p *agentv1.ProjectSpec) bool { return p.Name != name })
			st.Environments = filter(st.Environments, func(e *agentv1.EnvironmentSpec) bool { return e.Project != name })
			return nil
		}
		for _, e := range st.Environments {
			if e.Project == name {
				e.Phase = agentv1.DesiredPhase_DESIRED_PHASE_STOPPED
			}
		}
		return nil
	})
}

// SetProxy changes the device's proxy settings. Turning coexistence on or off, or
// asking for a restart, bumps the proxy generation: the proxy is recreated and probes
// 80/443 again, which is the point of moving a dev server off them.
func (s *Store) SetProxy(coexistHTTP, coexistHTTPS uint32, restart bool) (uint64, error) {
	return s.update(func(st *agentv1.DesiredState) error {
		if st.Device == nil {
			st.Device = &agentv1.DeviceSettings{}
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

func findProject(st *agentv1.DesiredState, name string) *agentv1.ProjectSpec {
	for _, p := range st.Projects {
		if p.Name == name {
			return p
		}
	}
	return nil
}

func baseEnv(st *agentv1.DesiredState, project string) *agentv1.EnvironmentSpec {
	for _, e := range st.Environments {
		if e.Project == project && e.Kind == agentv1.EnvironmentKind_ENVIRONMENT_KIND_BASE {
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
