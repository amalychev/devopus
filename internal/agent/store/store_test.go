package store

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	agentv2 "github.com/amalychev/devopus/gen/devopus/agent/v2"
)

// served registers a project as the workspace server gives it.
func served(t *testing.T, s *Store, names ...string) {
	t.Helper()
	var ps []ServerProject
	for _, n := range names {
		ps = append(ps, ServerProject{Name: n, Spec: []byte("name: " + n + "\ndomain: " + n + ".localhost\n"), Version: 1})
	}
	if _, _, err := s.SyncServer("wsp_1", "", ServerKey{}, ps, func(n string) string { return "/data/projects/" + n }); err != nil {
		t.Fatal(err)
	}
}

func TestUpDownPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Up("shop"); !errors.Is(err, ErrUnknownProject) {
		t.Fatalf("up of a project the server did not give: %v", err)
	}
	served(t, s, "shop")
	select {
	case <-s.Changed():
	default:
		t.Error("no change signal")
	}
	id, rev, err := s.Up("shop")
	if err != nil || rev != 2 || len(id) != 6 {
		t.Fatalf("up: %q %d %v", id, rev, err)
	}
	// Up again keeps the environment.
	if id2, _, _ := s.Up("shop"); id2 != id {
		t.Errorf("second up: %q", id2)
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
	if len(st.Projects) != 1 || !st.Projects[0].Held || st.Projects[0].WorkspaceId != "wsp_1" {
		t.Fatalf("reloaded: %v", st)
	}
	if e := st.Environments[0]; e.Id != id || e.Phase != agentv2.DesiredPhase_DESIRED_PHASE_STOPPED {
		t.Errorf("env: %v", e)
	}
	// down --volumes of a workspace project keeps it registered and held.
	if _, err := s2.Down("shop", true); err != nil {
		t.Fatal(err)
	}
	if st := s2.Snapshot(); len(st.Projects) != 1 || len(st.Environments) != 0 {
		t.Errorf("after purge: %v", st)
	}
}

func TestFeatures(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	served(t, s, "shop")
	f, base, _, err := s.CreateFeature("shop", NewFeature{Name: "PAY-123", Branch: "feature/PAY-123", Apps: []string{"web", "api", "web"}, Reserved: []string{"mail"}})
	if err != nil {
		t.Fatal(err)
	}
	if f.Slug != "pay-123" || f.Kind != agentv2.EnvironmentKind_ENVIRONMENT_KIND_FEATURE || len(f.ForkedApps) != 2 {
		t.Errorf("feature = %v", f)
	}
	if base.Kind != agentv2.EnvironmentKind_ENVIRONMENT_KIND_BASE || base.Phase != agentv2.DesiredPhase_DESIRED_PHASE_RUNNING {
		t.Errorf("base = %v", base)
	}
	if _, _, _, err := s.CreateFeature("shop", NewFeature{Name: "pay-123"}); !errors.Is(err, ErrFeatureExists) {
		t.Errorf("same name: %v", err)
	}
	// Different names, same slug; and a slug an exposed web UI has.
	g, _, _, err := s.CreateFeature("shop", NewFeature{Name: "pay_123"})
	if err != nil || g.Slug != "pay-123-2" {
		t.Errorf("collision: %v %v", g, err)
	}
	m, _, _, err := s.CreateFeature("shop", NewFeature{Name: "Mail", Reserved: []string{"mail"}})
	if err != nil || m.Slug != "mail-2" {
		t.Errorf("reserved: %v %v", m, err)
	}
	if _, _, _, err := s.CreateFeature("shop", NewFeature{Name: "---"}); err == nil {
		t.Error("a name without letters must fail")
	}
	if _, _, _, err := s.CreateFeature("nope", NewFeature{Name: "x"}); !errors.Is(err, ErrUnknownProject) {
		t.Errorf("feature of an unknown project: %v", err)
	}

	e, _, err := s.AddFeatureApps("shop", "pay-123", []string{"payments", "api"})
	if err != nil || len(e.ForkedApps) != 3 {
		t.Errorf("add: %v %v", e, err)
	}
	if _, _, err := s.AddFeatureApps("shop", "nope", nil); !errors.Is(err, ErrUnknownFeature) {
		t.Errorf("add to unknown: %v", err)
	}

	// Down stops everything, up starts features again.
	if _, err := s.Down("shop", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Up("shop"); err != nil {
		t.Fatal(err)
	}
	for _, e := range s.Snapshot().Environments {
		if e.Phase != agentv2.DesiredPhase_DESIRED_PHASE_RUNNING {
			t.Errorf("%s is %v after up", e.Id, e.Phase)
		}
	}

	refuse := errors.New("unsaved")
	if _, _, err := s.RemoveFeature("shop", f.Id, func(*agentv2.EnvironmentSpec) error { return refuse }); !errors.Is(err, refuse) {
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

// A project registered from a folder before R10 keeps running from the state file.
func TestFolderProjectFromBefore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{"revision":"3","projects":[{"name":"local","root":"/src/local","config":"` +
		base64.StdEncoding.EncodeToString([]byte("version: 1\nproject: local\ndomain: local.localhost\nmodules:\n  web:\n    path: web\n    image: nginx:1.27\n")) +
		`"}],"environments":[{"id":"b1","project":"local","kind":"ENVIRONMENT_KIND_BASE","phase":"DESIRED_PHASE_RUNNING"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseProject(s.Snapshot().Projects[0])
	if err != nil || p.Domain != "local.localhost" || p.Apps["web"] == nil {
		t.Fatalf("legacy project: %+v %v", p, err)
	}
	if _, _, err := s.Up("local"); err != nil {
		t.Errorf("up of the legacy project: %v", err)
	}
	if _, err := s.Down("local", true); err != nil || len(s.Snapshot().Projects) != 0 {
		t.Errorf("purge of the legacy project: %v", err)
	}
}

// A state file of agent/v1 keeps its features.
func TestOpenMigratesV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	v1 := `{"revision":"3","environments":[{"id":"f1","project":"p","kind":"ENVIRONMENT_KIND_FEATURE","forkedModules":["api","web"]}]}`
	if err := os.WriteFile(path, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Snapshot().Environments[0].ForkedApps; len(got) != 2 || got[0] != "api" {
		t.Fatalf("forked apps %v", got)
	}
}

func TestSyncServer(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	root := func(name string) string { return "/data/projects/" + name }
	shop := ServerProject{Name: "shop", Spec: []byte("name: shop\ndomain: shop.localhost\n"), Version: 1}
	blog := ServerProject{Name: "blog", Spec: []byte("name: blog\ndomain: blog.localhost\n"), Version: 4}
	// A project of a folder from before R10 keeps its name.
	legacy := []byte(`{"projects":[{"name":"local","root":"/src/local","config":"dmVyc2lvbjogMQ=="}]}`)
	if err := os.WriteFile(s.path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(s.path); err != nil {
		t.Fatal(err)
	}
	clash := ServerProject{Name: "local", Spec: []byte("name: local\n"), Version: 1}

	rev, conflicts, err := s.SyncServer("wsp_1", "admin@acme.dev", ServerKey{}, []ServerProject{shop, blog, clash}, root)
	if err != nil || len(conflicts) != 1 || conflicts[0] != "local" {
		t.Fatalf("sync: %v %v", conflicts, err)
	}
	phase := func(project string) agentv2.DesiredPhase {
		for _, e := range s.Snapshot().Environments {
			if e.Project == project && e.Kind == agentv2.EnvironmentKind_ENVIRONMENT_KIND_BASE {
				return e.Phase
			}
		}
		return -1
	}
	project := func(name string) *agentv2.ProjectSpec {
		for _, p := range s.Snapshot().Projects {
			if p.Name == name {
				return p
			}
		}
		return nil
	}
	// New projects come up on their own: a new member does nothing.
	if phase("shop") != agentv2.DesiredPhase_DESIRED_PHASE_RUNNING || project("shop").Root != "/data/projects/shop" || project("shop").AdminEmail != "admin@acme.dev" {
		t.Fatalf("shop: %v %+v", phase("shop"), project("shop"))
	}
	if p, err := ParseProject(project("shop")); err != nil || p.Domain != "shop.localhost" {
		t.Fatalf("parse: %+v %v", p, err)
	}
	if string(project("local").Config) != "version: 1" || project("local").WorkspaceId != "" {
		t.Error("the folder's project was replaced")
	}
	// The same state again changes nothing.
	if again, _, _ := s.SyncServer("wsp_1", "admin@acme.dev", ServerKey{}, []ServerProject{shop, blog, clash}, root); again != rev {
		t.Errorf("revision moved without a change: %d → %d", rev, again)
	}

	// Held with down: a new version does not start it; up does.
	if _, err := s.Down("shop", false); err != nil {
		t.Fatal(err)
	}
	shop.Version, shop.Spec = 2, []byte("name: shop\ndomain: shop.test\n")
	s.SyncServer("wsp_1", "admin@acme.dev", ServerKey{}, []ServerProject{shop, blog}, root)
	if phase("shop") != agentv2.DesiredPhase_DESIRED_PHASE_STOPPED || project("shop").ConfigVersion != 2 || !project("shop").Held {
		t.Fatalf("held shop: %v %+v", phase("shop"), project("shop"))
	}
	if _, _, err := s.Up("shop"); err != nil {
		t.Fatal(err)
	}
	if phase("shop") != agentv2.DesiredPhase_DESIRED_PHASE_RUNNING || project("shop").Held || project("shop").Root != "/data/projects/shop" {
		t.Fatalf("up shop: %v %+v", phase("shop"), project("shop"))
	}

	// Gone from the server: stopped, kept, and not to be brought up.
	s.SyncServer("wsp_1", "admin@acme.dev", ServerKey{}, []ServerProject{shop}, root)
	if !project("blog").Orphaned || phase("blog") != agentv2.DesiredPhase_DESIRED_PHASE_STOPPED {
		t.Fatalf("blog: %+v", project("blog"))
	}
	if _, _, err := s.Up("blog"); !errors.Is(err, ErrUnknownProject) {
		t.Errorf("up of an orphan: %v", err)
	}
	// Back on the server: it comes up again.
	s.SyncServer("wsp_1", "admin@acme.dev", ServerKey{}, []ServerProject{shop, blog}, root)
	if project("blog").Orphaned || phase("blog") != agentv2.DesiredPhase_DESIRED_PHASE_RUNNING {
		t.Fatalf("blog back: %+v", project("blog"))
	}

	// down --volumes of a server project drops its environments, keeps it held.
	if _, err := s.Down("blog", true); err != nil {
		t.Fatal(err)
	}
	if project("blog") == nil || !project("blog").Held || phase("blog") != -1 {
		t.Fatalf("purged blog: %+v %v", project("blog"), phase("blog"))
	}
	if _, err := s.Link("blog", "/src/blog"); err != nil || project("blog").Root != "/src/blog" {
		t.Errorf("link: %v", err)
	}
	if _, err := s.Link("local", "/x"); !errors.Is(err, ErrUnknownProject) {
		t.Errorf("link of a folder's project: %v", err)
	}
}
