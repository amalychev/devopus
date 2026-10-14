package main

import (
	"reflect"
	"strings"
	"testing"
)

func testWorkspace() *Workspace {
	ws := &Workspace{
		Version: 1, Project: "shop", Domain: "shop.localhost",
		Services: map[string]Service{
			"db":   {Image: "postgres:17-alpine", Port: 5432},
			"mail": {Image: "axllent/mailpit:v1.21", Port: 1025},
		},
		Modules: map[string]Module{
			"frontend": {Path: ".", Image: "x:1", Command: "c", Routes: []string{"/"}},
			"backend":  {Path: ".", Image: "x:1", Command: "c", Routes: []string{"/api"}, Uses: []string{"payments", "mail"}},
			"payments": {Path: ".", Image: "x:1", Command: "c", Routes: []string{"/payments"}, Uses: []string{"db"}},
			"admin":    {Path: ".", Image: "x:1", Command: "c", Routes: []string{"/admin"}, Uses: []string{"backend"}},
		},
	}
	ws.applyDefaults()
	return ws
}

func TestPlanFeatureCopiesTransitiveDependents(t *testing.T) {
	mods, svcs, err := PlanFeature(testWorkspace(), []string{"payments"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"admin", "backend", "payments"}; !reflect.DeepEqual(mods, want) {
		t.Errorf("modules = %v, want %v", mods, want)
	}
	if want := []string{"db"}; !reflect.DeepEqual(svcs, want) {
		t.Errorf("services = %v, want %v", svcs, want)
	}
}

func TestPlanFeatureLeafModuleStaysAlone(t *testing.T) {
	mods, svcs, _ := PlanFeature(testWorkspace(), []string{"frontend"})
	if !reflect.DeepEqual(mods, []string{"frontend"}) || len(svcs) != 0 {
		t.Errorf("got %v %v", mods, svcs)
	}
}

func TestPlanFeatureSharedDatabase(t *testing.T) {
	ws := testWorkspace()
	ws.Features.Database = "shared"
	_, svcs, _ := PlanFeature(ws, []string{"payments"})
	if len(svcs) != 0 {
		t.Errorf("database: shared must not copy services, got %v", svcs)
	}
}

func TestValidateCatchesMistakes(t *testing.T) {
	ws := testWorkspace()
	ws.Services["cache"] = Service{Image: "redis:latest"}
	m := ws.Modules["admin"]
	m.Uses = append(m.Uses, "ghost")
	m.Routes = []string{"/api"}
	ws.Modules["admin"] = m
	err := ws.Validate(".")
	if err == nil {
		t.Fatal("want validation errors")
	}
	for _, want := range []string{"pin an explicit tag", "unknown \"ghost\"", "already routed", "services.cache.port"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
}

func TestSlugCollision(t *testing.T) {
	st := &State{Envs: []*Env{{Slug: "a-b"}}}
	if got := st.uniqueSlug("feature/a_b"); got != "a-b-2" {
		t.Errorf("got %q", got)
	}
}

func TestEnvoyRoutesLongestPrefixFirstAndFallBackToBase(t *testing.T) {
	ws := testWorkspace()
	base := &Env{ID: "base1", Kind: KindBase, Modules: ws.ModuleNames(), Services: ws.ServiceNames(), EnvoyIP: "10.213.1.254"}
	feat := &Env{ID: "f1", Kind: KindFeature, Slug: "pay", Modules: []string{"backend", "payments"}, Services: []string{"db"}, EnvoyIP: "10.213.2.254"}
	st := &State{Project: "shop", Envs: []*Env{base, feat}}
	cfg, err := EnvoyConfig(st, ws)
	if err != nil {
		t.Fatal(err)
	}
	listeners := cfg["static_resources"].(m)["listeners"].([]m)
	ext := listeners[0]["filter_chains"].([]m)[0]["filters"].([]m)[0]["typed_config"].(m)["route_config"].(m)
	var featRoutes []m
	for _, vh := range ext["virtual_hosts"].([]m) {
		if vh["name"] == "f1" {
			featRoutes = vh["routes"].([]m)
		}
	}
	got := map[string]string{}
	var order []string
	for _, r := range featRoutes {
		p := r["match"].(m)["prefix"].(string)
		order = append(order, p)
		got[p] = r["route"].(m)["cluster"].(string)
	}
	if order[len(order)-1] != "/" {
		t.Errorf("catch-all / must be last, order %v", order)
	}
	want := map[string]string{"/payments": "f1-mod-payments", "/api": "f1-mod-backend", "/": "base1-mod-frontend", "/admin": "base1-mod-admin"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("routes = %v, want %v", got, want)
	}
	// Feature reaches shared mail over TCP through Envoy.
	found := false
	for _, l := range listeners {
		if l["name"] == "internal_tcp_f1_mail" {
			found = true
		}
	}
	if !found {
		t.Error("no internal TCP listener for shared mail in feature f1")
	}
}
