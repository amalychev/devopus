package store

import (
	"errors"
	"path/filepath"
	"testing"

	agentv1 "github.com/amalychev/devopus/gen/devopus/agent/v1"
)

func TestUpDownPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id, rev, err := s.Up("shop", "/src/shop", []byte("version: 1"))
	if err != nil || rev != 1 || len(id) != 6 {
		t.Fatalf("up: %q %d %v", id, rev, err)
	}
	select {
	case <-s.Changed():
	default:
		t.Error("no change signal")
	}

	// Up again keeps the environment ID and updates the config.
	id2, rev, _ := s.Up("shop", "/src/shop", []byte("version: 1\n# v2"))
	if id2 != id || rev != 2 {
		t.Errorf("second up: %q %d", id2, rev)
	}
	if _, _, err := s.Up("shop", "/elsewhere/shop", nil); !errors.Is(err, ErrProjectConflict) {
		t.Errorf("conflict: %v", err)
	}
	if _, err := s.Down("nope", false); !errors.Is(err, ErrUnknownProject) {
		t.Errorf("unknown: %v", err)
	}
	if _, err := s.Down("shop", false); err != nil {
		t.Fatal(err)
	}

	// A new process reads what the previous one wrote; failed updates left no trace.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st := s2.Snapshot()
	if st.Revision != 3 || len(st.Projects) != 1 || string(st.Projects[0].Config) != "version: 1\n# v2" {
		t.Fatalf("reloaded: %v", st)
	}
	if e := st.Environments[0]; e.Id != id || e.Phase != agentv1.DesiredPhase_DESIRED_PHASE_STOPPED {
		t.Errorf("env: %v", e)
	}

	if _, err := s2.Down("shop", true); err != nil {
		t.Fatal(err)
	}
	if st := s2.Snapshot(); len(st.Projects) != 0 || len(st.Environments) != 0 {
		t.Errorf("after purge: %v", st)
	}
}

func TestFeatures(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	f, base, _, err := s.CreateFeature("shop", "/src/shop", []byte("version: 1"), NewFeature{Name: "PAY-123", Branch: "feature/PAY-123", Modules: []string{"web", "api", "web"}, Reserved: []string{"mail"}})
	if err != nil {
		t.Fatal(err)
	}
	if f.Slug != "pay-123" || f.Kind != agentv1.EnvironmentKind_ENVIRONMENT_KIND_FEATURE || len(f.ForkedModules) != 2 {
		t.Errorf("feature = %v", f)
	}
	if base.Kind != agentv1.EnvironmentKind_ENVIRONMENT_KIND_BASE || base.Phase != agentv1.DesiredPhase_DESIRED_PHASE_RUNNING {
		t.Errorf("base = %v", base)
	}
	if _, _, _, err := s.CreateFeature("shop", "/src/shop", nil, NewFeature{Name: "pay-123"}); !errors.Is(err, ErrFeatureExists) {
		t.Errorf("same name: %v", err)
	}
	// Different names, same slug; and a slug an exposed web UI has.
	g, _, _, err := s.CreateFeature("shop", "/src/shop", nil, NewFeature{Name: "pay_123"})
	if err != nil || g.Slug != "pay-123-2" {
		t.Errorf("collision: %v %v", g, err)
	}
	m, _, _, err := s.CreateFeature("shop", "/src/shop", nil, NewFeature{Name: "Mail", Reserved: []string{"mail"}})
	if err != nil || m.Slug != "mail-2" {
		t.Errorf("reserved: %v %v", m, err)
	}
	if _, _, _, err := s.CreateFeature("shop", "/src/shop", nil, NewFeature{Name: "---"}); err == nil {
		t.Error("a name without letters must fail")
	}

	e, _, err := s.AddFeatureModules("shop", "/src/shop", nil, "pay-123", []string{"payments", "api"})
	if err != nil || len(e.ForkedModules) != 3 {
		t.Errorf("add: %v %v", e, err)
	}
	if _, _, err := s.AddFeatureModules("shop", "/src/shop", nil, "nope", nil); !errors.Is(err, ErrUnknownFeature) {
		t.Errorf("add to unknown: %v", err)
	}

	// Down stops everything, up starts features again.
	if _, err := s.Down("shop", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Up("shop", "/src/shop", nil); err != nil {
		t.Fatal(err)
	}
	for _, e := range s.Snapshot().Environments {
		if e.Phase != agentv1.DesiredPhase_DESIRED_PHASE_RUNNING {
			t.Errorf("%s is %v after up", e.Id, e.Phase)
		}
	}

	refuse := errors.New("unsaved")
	if _, _, err := s.RemoveFeature("shop", f.Id, func(*agentv1.EnvironmentSpec) error { return refuse }); !errors.Is(err, refuse) {
		t.Errorf("check: %v", err)
	}
	if s.Feature("shop", "PAY-123") == nil {
		t.Fatal("a refused removal removed the feature")
	}
	if _, _, err := s.RemoveFeature("shop", "PAY-123", nil); err != nil {
		t.Fatal(err)
	}
	if s.Feature("shop", "pay-123") != nil || len(s.Snapshot().Environments) != 3 {
		t.Errorf("after remove: %v", s.Snapshot().Environments)
	}
}
