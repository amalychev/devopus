package stack

import (
	"net/netip"
	"path/filepath"
	"slices"
	"testing"

	"github.com/devopus/devopus/internal/workspace"
)

const shop = `version: 1
project: shop
domain: shop.localhost
services:
  db:
    image: postgres:17.2
    port: 5432
  cache:
    image: redis:7.4
    port: 6379
  queue:
    image: rabbitmq:4.0.5
    port: 5672
    uses: [cache]
  pdf:
    image: gotenberg/gotenberg:8.15.3
    port: 3000
    optional: true
modules:
  web:
    path: web
    runtime: node:22
    command: npm run dev
    routes: ["/"]
    uses: [api]
  api:
    path: api
    runtime: php:8.3
    command: php -S 0.0.0.0:8080
    routes: ["/api"]
    uses: [db, queue, payments]
    hooks:
      migrate: php artisan migrate
  payments:
    path: payments
    runtime: python:3.12
    command: python app.py
    routes: ["/payments"]
    uses: [db]
    hooks:
      migrate: alembic upgrade head
  invoices:
    path: invoices
    runtime: node:22
    command: node app.js
    optional: true
    uses: [pdf]
  admin:
    path: admin
    runtime: node:22
    command: node app.js
    routes: ["/admin"]
    uses: [invoices, cache]
`

func shopWorkspace(t *testing.T, database string) *workspace.Workspace {
	t.Helper()
	res, err := workspace.Parse([]byte(shop))
	if err != nil {
		t.Fatal(err)
	}
	if res.Diagnostics.HasErrors() {
		t.Fatal(res.Diagnostics.Errors())
	}
	if database != "" {
		res.Workspace.Features = &workspace.FeaturePolicy{Database: database}
	}
	return res.Workspace
}

func TestFeatureRunsForksAndTheirDependents(t *testing.T) {
	w := shopWorkspace(t, "")
	iso, err := Feature(w, []string{"payments"})
	if err != nil {
		t.Fatal(err)
	}
	// api uses payments and web uses api: both must reach the fork by its name.
	if want := []string{"api", "payments", "web"}; !slices.Equal(iso.Modules, want) {
		t.Errorf("modules = %v, want %v", iso.Modules, want)
	}
	if want := []string{"api", "web"}; !slices.Equal(iso.Dependents(), want) {
		t.Errorf("dependents = %v, want %v", iso.Dependents(), want)
	}
	// database: clone — the feature has its own copy of what its modules use, with the
	// services those depend on.
	if want := []string{"cache", "db", "queue"}; !slices.Equal(iso.Services, want) {
		t.Errorf("services = %v, want %v", iso.Services, want)
	}
	if len(iso.SharedModules) != 0 || len(iso.SharedServices) != 0 || len(iso.NoMigrate) != 0 {
		t.Errorf("shared = %v %v, no migrate %v", iso.SharedModules, iso.SharedServices, iso.NoMigrate)
	}
}

func TestFeatureSharesWhatItDoesNotRun(t *testing.T) {
	w := shopWorkspace(t, "")
	iso, err := Feature(w, []string{"web"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(iso.Modules, []string{"web"}) || len(iso.Services) != 0 {
		t.Errorf("modules = %v, services = %v", iso.Modules, iso.Services)
	}
	if !slices.Equal(iso.SharedModules, []string{"api"}) {
		t.Errorf("shared modules = %v", iso.SharedModules)
	}
}

func TestFeatureWithSharedDatabase(t *testing.T) {
	w := shopWorkspace(t, "shared")
	iso, err := Feature(w, []string{"payments"})
	if err != nil {
		t.Fatal(err)
	}
	if len(iso.Services) != 0 {
		t.Errorf("services = %v, want none", iso.Services)
	}
	if want := []string{"db", "queue"}; !slices.Equal(iso.SharedServices, want) {
		t.Errorf("shared services = %v, want %v", iso.SharedServices, want)
	}
	// Migrations would change the base environment's database.
	if want := []string{"api", "payments"}; !slices.Equal(iso.NoMigrate, want) {
		t.Errorf("no migrate = %v, want %v", iso.NoMigrate, want)
	}
}

func TestFeatureBringsWhatTheBaseDoesNotRun(t *testing.T) {
	// invoices is optional and active in the base through admin; pdf comes with it.
	w := shopWorkspace(t, "shared")
	w.Modules["admin"].Uses = []string{"cache"}
	iso, err := Feature(w, []string{"admin"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(iso.Modules, []string{"admin"}) {
		t.Errorf("modules = %v", iso.Modules)
	}
	w.Modules["admin"].Uses = []string{"invoices", "cache"}
	if iso, err = Feature(w, []string{"admin"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(iso.SharedModules, []string{"invoices"}) {
		t.Errorf("shared modules = %v: invoices runs in the base", iso.SharedModules)
	}

	// Forking a module the base does not run brings what it needs, even with shared data.
	w.Modules["admin"].Uses = []string{"cache"}
	if iso, err = Feature(w, []string{"invoices"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(iso.Modules, []string{"invoices"}) || !slices.Equal(iso.Services, []string{"pdf"}) {
		t.Errorf("modules = %v, services = %v", iso.Modules, iso.Services)
	}
}

func TestFeatureUnknownModule(t *testing.T) {
	if _, err := Feature(shopWorkspace(t, ""), []string{"nope"}); err == nil {
		t.Fatal("want an error")
	}
}

func TestBuildFeature(t *testing.T) {
	w := shopWorkspace(t, "shared")
	iso, err := Feature(w, []string{"payments"})
	if err != nil {
		t.Fatal(err)
	}
	root, wt := t.TempDir(), t.TempDir()
	sources := map[string]string{}
	for _, m := range iso.Modules {
		sources[m] = filepath.Join(wt, m)
	}
	p, err := Build(w, Env{ID: "f1", Project: "shop", Root: root, CodeRoot: wt, Isolation: iso,
		Network: "n", Subnet: netip.MustParsePrefix("10.213.9.0/24"), Sources: sources})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, u := range p.Units {
		names = append(names, u.Name)
	}
	slices.Sort(names)
	if want := []string{"api", "payments", "web"}; !slices.Equal(names, want) {
		t.Errorf("units = %v, want %v", names, want)
	}
	for _, h := range p.Hooks {
		if h.Name == "migrate" {
			t.Errorf("migrate of %s runs on the shared database", h.Module)
		}
	}
}
