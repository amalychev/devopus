package stack

import (
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devopus/devopus/internal/agent/runtime"
	"github.com/devopus/devopus/internal/workspace"
)

const config = `version: 1
project: shop
domain: shop.localhost
services:
  db:
    image: postgres:17.2
    port: 5432
    env:
      POSTGRES_DB: shop
      POSTGRES_PASSWORD: dev
    volumes: ["pgdata:/var/lib/postgresql/data", "./db/init.sql:/docker-entrypoint-initdb.d/init.sql:ro"]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready"]
      interval: 2s
  cache:
    image: redis:7.4
    port: 6379
    optional: true
  mail:
    image: axllent/mailpit:v1.21
    port: 1025
    ui_port: 8025
    expose: mail
  search:
    image: opensearch:2.19.1
    optional: true
modules:
  web:
    path: web
    runtime: node:22
    routes: ["/"]
    command: npm run dev -- --host 0.0.0.0
    volumes: ["node_modules:/app/node_modules"]
    env:
      API_URL: ${{ api.url }}
      PUBLIC_URL: ${{ env.url }}
    hooks:
      install: npm ci
  api:
    path: api
    build: {context: api, dockerfile: Dockerfile.dev, args: {PHP_VERSION: "8.3"}}
    mount: /var/www
    port: 9000
    routes: ["/api"]
    uses: [db, cache]
    env:
      DB_URL: postgres://postgres:dev@${{ db.host }}:${{ db.port }}/${{ db.database }}
      STRIPE_KEY: ${{ secrets.STRIPE_KEY }}
    hooks:
      install: composer install
      migrate: php artisan migrate --force
    workers:
      - command: php artisan queue:work
        replicas: 2
  docs:
    path: docs
    image: nginx:1.27.4
    optional: true
`

func plan(t *testing.T) (*Plan, Env) {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"web", "api", "docs"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(root, "api", "Dockerfile.dev"), []byte("FROM php:8.3\n"), 0o644)
	os.WriteFile(filepath.Join(root, "web", "package-lock.json"), []byte("{}"), 0o644)
	res, err := workspace.Parse([]byte(config))
	if err != nil {
		t.Fatal(err)
	}
	if res.Diagnostics.HasErrors() {
		t.Fatal(res.Diagnostics.Errors())
	}
	env := Env{
		ID: "abc123", Project: "shop", Slug: "main", Root: root,
		Network: "net1", Subnet: netip.MustParsePrefix("10.213.4.0/24"),
		URL:     "https://shop.localhost",
		Sources: map[string]string{"web": filepath.Join(root, "web"), "api": filepath.Join(root, "api")},
		Heads:   map[string]string{"api": "c0ffee"},
		Secrets: map[string]string{"STRIPE_KEY": "sk_test"},
		CA:      []byte("ca"), Bundle: []byte("bundle"),
		DNS:    netip.MustParseAddr("10.213.4.253"),
		Labels: map[string]string{runtime.LabelManaged: "true", runtime.LabelEnv: "abc123"},
	}
	p, err := Build(res.Workspace, env)
	if err != nil {
		t.Fatal(err)
	}
	return p, env
}

func unit(t *testing.T, p *Plan, name string) Unit {
	t.Helper()
	for _, u := range p.Units {
		if u.Name == name {
			return u
		}
	}
	t.Fatalf("no unit %s", name)
	return Unit{}
}

func envOf(spec runtime.ContainerSpec, key string) string {
	for _, kv := range spec.Env {
		if k, v, _ := strings.Cut(kv, "="); k == key {
			return v
		}
	}
	return ""
}

func TestPlanUnits(t *testing.T) {
	p, env := plan(t)
	var names []string
	for _, u := range p.Units {
		names = append(names, u.Name)
	}
	// The optional cache is used by api; the optional search and docs are not.
	for _, want := range []string{"db", "cache", "mail", "web", "api", "api-worker-1-1", "api-worker-1-2"} {
		if !slices.Contains(names, want) {
			t.Errorf("units %v lack %s", names, want)
		}
	}
	if slices.Contains(names, "search") || slices.Contains(names, "docs") {
		t.Errorf("optional units nobody uses run: %v", names)
	}
	pos := func(n string) int { return slices.Index(names, n) }
	if pos("db") > pos("api") || pos("cache") > pos("api") || pos("api") > pos("api-worker-1-1") {
		t.Errorf("order %v: dependencies must come first", names)
	}

	seen := map[netip.Addr]string{}
	for _, u := range p.Units {
		if u.Role == RoleWorker {
			if u.IP.IsValid() || len(u.Spec.Aliases) > 0 {
				t.Errorf("worker %s has a pinned address or an alias", u.Name)
			}
			continue
		}
		if !env.Subnet.Contains(u.IP) || u.IP.As4()[3] < 128 || u.IP.As4()[3] > 252 {
			t.Errorf("%s at %s: outside the pinned range", u.Name, u.IP)
		}
		if other, dup := seen[u.IP]; dup {
			t.Errorf("%s and %s share %s", u.Name, other, u.IP)
		}
		seen[u.IP] = u.Name
		if u.Spec.Aliases[0] != u.Name || u.Spec.Name != "shop-env-abc123-"+u.Name {
			t.Errorf("%s: alias %v name %s", u.Name, u.Spec.Aliases, u.Spec.Name)
		}
	}

	// Adding a module does not move the others, unless their slots collide.
	p2, _ := plan(t)
	if unit(t, p2, "api").IP != unit(t, p, "api").IP {
		t.Error("addresses are not stable")
	}
}

func TestPlanModuleSpecs(t *testing.T) {
	p, env := plan(t)
	web := unit(t, p, "web")
	if web.Spec.Image != "node:22" || web.Spec.WorkDir != "/app" || !web.Spec.Init {
		t.Errorf("web: image %s workdir %s init %v", web.Spec.Image, web.Spec.WorkDir, web.Spec.Init)
	}
	if web.Spec.Binds[0] != (runtime.Bind{Source: env.Sources["web"], Target: "/app"}) {
		t.Errorf("web binds %v", web.Spec.Binds)
	}
	if web.Spec.Volumes[0] != (runtime.VolumeMount{Name: "shop-abc123-node_modules", Target: "/app/node_modules"}) {
		t.Errorf("web volumes %v", web.Spec.Volumes)
	}
	if got := strings.Join(web.Spec.Cmd, "|"); got != "npm|run|dev|--|--host|0.0.0.0" {
		t.Errorf("web cmd %q", got)
	}
	if envOf(web.Spec, "API_URL") != "http://api:9000" || envOf(web.Spec, "PUBLIC_URL") != "https://shop.localhost" {
		t.Errorf("web env %v", web.Spec.Env)
	}
	if envOf(web.Spec, "SSL_CERT_FILE") != BundlePath || envOf(web.Spec, "NODE_EXTRA_CA_CERTS") != CAPath || len(web.Spec.Files) != 2 {
		t.Errorf("web does not get the CA: env %v files %d", web.Spec.Env, len(web.Spec.Files))
	}
	if web.Spec.DNS[0] != env.DNS {
		t.Errorf("web DNS %v", web.Spec.DNS)
	}

	api := unit(t, p, "api")
	if len(p.Builds) != 1 || api.Spec.Image != p.Builds[0].Tag || !strings.HasPrefix(api.Spec.Image, "devopus/shop-api:") {
		t.Fatalf("api image %s builds %+v", api.Spec.Image, p.Builds)
	}
	if b := p.Builds[0]; b.Context != filepath.Join(env.Root, "api") || b.Dockerfile != "Dockerfile.dev" || b.Args["PHP_VERSION"] != "8.3" {
		t.Errorf("build %+v", b)
	}
	if api.Spec.WorkDir != "/var/www" || api.Port != 9000 || api.Routes[0] != "/api" {
		t.Errorf("api workdir %s port %d routes %v", api.Spec.WorkDir, api.Port, api.Routes)
	}
	if got := envOf(api.Spec, "DB_URL"); got != "postgres://postgres:dev@db:5432/shop" {
		t.Errorf("DB_URL = %q", got)
	}
	if envOf(api.Spec, "STRIPE_KEY") != "sk_test" {
		t.Error("the secret is not substituted")
	}
	if slices.Contains(p.Images, api.Spec.Image) {
		t.Error("a built image is in the pull list")
	}
	for _, img := range []string{"node:22", "postgres:17.2", "redis:7.4", "axllent/mailpit:v1.21"} {
		if !slices.Contains(p.Images, img) {
			t.Errorf("images %v lack %s", p.Images, img)
		}
	}
	if w := unit(t, p, "api-worker-1-2"); strings.Join(w.Spec.Cmd, " ") != "php artisan queue:work" || w.Spec.Image != api.Spec.Image || w.Needs[0] != "api" {
		t.Errorf("worker %+v", w)
	}
}

func TestPlanServices(t *testing.T) {
	p, env := plan(t)
	db := unit(t, p, "db")
	if !db.Healthcheck || db.Spec.Health.Test[1] != "pg_isready" || db.Spec.Health.Interval.Seconds() != 2 {
		t.Errorf("db health %+v", db.Spec.Health)
	}
	if db.Spec.Init || len(db.Spec.Files) != 0 || envOf(db.Spec, "SSL_CERT_FILE") != "" {
		t.Error("services run as their image intends: no init, no CA files")
	}
	if db.Spec.Binds[0] != (runtime.Bind{Source: filepath.Join(env.Root, "db/init.sql"), Target: "/docker-entrypoint-initdb.d/init.sql", ReadOnly: true}) {
		t.Errorf("db binds %v", db.Spec.Binds)
	}
	if !slices.Equal(p.Volumes, []string{"shop-abc123-node_modules", "shop-abc123-pgdata"}) {
		t.Errorf("volumes %v", p.Volumes)
	}
	if mail := unit(t, p, "mail"); mail.Expose != "mail" || mail.Port != 8025 {
		t.Errorf("mail expose %q port %d", mail.Expose, mail.Port)
	}
}

func TestPlanHooks(t *testing.T) {
	p, env := plan(t)
	var got []string
	for _, h := range p.Hooks {
		got = append(got, h.Module+"."+h.Name)
	}
	// Every install comes before any migration: migrations may need dependencies.
	if len(got) != 3 || got[2] != "api.migrate" {
		t.Fatalf("hooks %v", got)
	}
	h := p.Hooks[2]
	if strings.Join(h.Spec.Cmd, "|") != "sh|-c|php artisan migrate --force" || h.Spec.Name != "shop-env-abc123-api-migrate" {
		t.Errorf("migrate spec %v %s", h.Spec.Cmd, h.Spec.Name)
	}
	if h.Spec.IP.IsValid() || h.Spec.Aliases != nil || h.Spec.Labels[runtime.LabelRole] != RoleHook {
		t.Error("a hook must not take the module's address or name")
	}

	// A new commit reruns migrate, a lock file change reruns install, nothing else does.
	keys := func(p *Plan) map[string]string {
		out := map[string]string{}
		for _, h := range p.Hooks {
			out[h.Module+"."+h.Name] = h.Key
		}
		return out
	}
	before := keys(p)
	res, _ := workspace.Parse([]byte(config))
	env.Heads = map[string]string{"api": "beef"}
	p2, err := Build(res.Workspace, env)
	if err != nil {
		t.Fatal(err)
	}
	after := keys(p2)
	if after["api.migrate"] == before["api.migrate"] || after["web.install"] != before["web.install"] {
		t.Errorf("after a commit: %v → %v", before, after)
	}
	os.WriteFile(filepath.Join(env.Sources["web"], "package-lock.json"), []byte(`{"v":2}`), 0o644)
	p3, _ := Build(res.Workspace, env)
	if keys(p3)["web.install"] == after["web.install"] {
		t.Error("a changed lock file does not rerun install")
	}
}

func TestMissingSecret(t *testing.T) {
	res, _ := workspace.Parse([]byte(config))
	_, env := plan(t)
	env.Secrets = nil
	if _, err := Build(res.Workspace, env); err == nil || !strings.Contains(err.Error(), "STRIPE_KEY is not in .env.secrets") {
		t.Errorf("err = %v", err)
	}
}

func TestCommand(t *testing.T) {
	for line, want := range map[string]string{
		`uvicorn app:main --host 0.0.0.0`:  "uvicorn|app:main|--host|0.0.0.0",
		`php artisan 'queue:work' "--a b"`: "php|artisan|queue:work|--a b",
		`npm ci && npm run dev`:            "sh|-c|npm ci && npm run dev",
		`echo $HOME`:                       "sh|-c|echo $HOME",
		`node server.js > /tmp/log`:        "sh|-c|node server.js > /tmp/log",
		`bundle exec rails s -b 0.0.0.0`:   "bundle|exec|rails|s|-b|0.0.0.0",
		`python -c "print(1)"`:             "python|-c|print(1)",
		`sh -c 'sleep 1; echo done'`:       "sh|-c|sleep 1; echo done",
	} {
		if got := strings.Join(Command(line), "|"); got != want {
			t.Errorf("%s → %q, want %q", line, got, want)
		}
	}
}
