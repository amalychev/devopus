package importer

import (
	"slices"
	"testing"

	"github.com/amalychev/devopus/internal/catalog"
)

const shop = `version: 1
project: shop
domain: shop.localhost
services:
  db: {image: "postgres:17.6", port: 5432}
  cache: {image: "redis:7", port: 6379}
  search: {image: "ghcr.io/acme/search:2", port: 9000}
modules:
  api:
    path: api
    runtime: php:8.3
    uses: [db, cache]
    workers:
      - command: php artisan queue:work
        replicas: 2
    cron:
      - schedule: "* * * * *"
        command: php artisan schedule:run
  admin:
    path: admin
    runtime: php:8.3
    uses: [api, db]
  web:
    path: web
    runtime: node:22
    uses: [api, cache]
  legacy:
    path: legacy
    runtime: elixir:1.17
`

func TestFromV1(t *testing.T) {
	p, _, err := Parse([]byte(shop))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.AppNames(), []string{"admin", "api", "legacy", "web"}) {
		t.Fatalf("apps %v", p.AppNames())
	}
	// One php service for both php applications; services attach to every user.
	php := p.Services["php"]
	if php == nil || php.Type != "php" || php.Version != "8.3" || !slices.Equal(php.Apps, []string{"admin", "api"}) {
		t.Fatalf("php = %+v", php)
	}
	if n := p.Services["nodejs"]; n == nil || n.ImageRef() != "node:22" || !slices.Equal(n.Apps, []string{"web"}) {
		t.Fatalf("nodejs = %+v", n)
	}
	if db := p.Services["db"]; db.Type != "postgres" || db.Version != "17.6" || !slices.Equal(db.Apps, []string{"admin", "api"}) {
		t.Errorf("db = %+v", db)
	}
	if c := p.Services["cache"]; !slices.Equal(c.Apps, []string{"api", "web"}) {
		t.Errorf("cache attached to %v", c.Apps)
	}
	if s := p.Services["search"]; s.Type != catalog.Custom || s.ImageRef() != "ghcr.io/acme/search:2" {
		t.Errorf("search = %+v", s)
	}
	// Workers and cron become processes of their application.
	if q := p.Services["api-worker-1"]; q == nil || q.Type != catalog.Queue || q.Replicas != 2 || !slices.Equal(q.Apps, []string{"api"}) {
		t.Errorf("worker = %+v", q)
	}
	if c := p.Services["api-cron-1"]; c == nil || c.Type != catalog.Cronjob || c.Schedule != "* * * * *" {
		t.Errorf("cron = %+v", c)
	}
	// Uses of applications stay with applications.
	if !slices.Equal(p.Apps["admin"].Uses, []string{"api"}) {
		t.Errorf("admin uses %v", p.Apps["admin"].Uses)
	}
	if !slices.Equal(p.Needs("admin"), []string{"api", "db"}) {
		t.Errorf("admin needs %v", p.Needs("admin"))
	}
	// A language the catalog does not know keeps its image and the code mount.
	if l := p.Apps["legacy"]; l.Image != "elixir:1.17" || p.CodeMount("legacy") != "/app" {
		t.Errorf("legacy = %+v, mount %q", l, p.CodeMount("legacy"))
	}
	if p.CodeMount("api") != "/app" {
		t.Errorf("api mount %q", p.CodeMount("api"))
	}
}

func TestRuntimeVersionsApart(t *testing.T) {
	p, _, err := Parse([]byte(`version: 1
project: p
domain: p.localhost
modules:
  a: {path: a, runtime: "php:8.2"}
  b: {path: b, runtime: "php:8.3"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Services["php-8-2"] == nil || p.Services["php-8-3"] == nil {
		t.Fatalf("services %v", p.ServiceNames())
	}
}
