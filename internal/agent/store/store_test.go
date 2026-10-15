package store

import (
	"errors"
	"path/filepath"
	"testing"

	agentv1 "github.com/devopus/devopus/gen/devopus/agent/v1"
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
