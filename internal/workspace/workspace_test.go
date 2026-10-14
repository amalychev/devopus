package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSpecExample(t *testing.T) {
	res, err := Load("testdata/shop.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Diagnostics) > 0 {
		t.Fatalf("expected no diagnostics, got:\n%s", res.Diagnostics.Error())
	}
	w := res.Workspace
	if got := w.ModuleNames(); strings.Join(got, ",") != "backend,frontend,payments" {
		t.Errorf("modules = %v", got)
	}
	if w.Modules["payments"].ModulePort() != DefaultModulePort {
		t.Errorf("default port not applied")
	}
	if w.Services["mail"].ExposePort() != 8025 {
		t.Errorf("expose port = %d", w.Services["mail"].ExposePort())
	}
	if pol := w.Policy(); pol.BranchPrefix != "feature/" || pol.Database != "clone" {
		t.Errorf("policy defaults = %+v", pol)
	}
}

// base is a minimal valid workspace; cases append to it or replace parts of it.
const base = `version: 1
project: shop
domain: shop.localhost
services:
  db:
    image: postgres:17.2
    port: 5432
modules:
  web:
    path: web
    runtime: node:22
    command: npm run dev
    routes: ["/"]
    uses: [db]
`

func TestValidateRules(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string // substring of an error; empty means valid
		path string // expected path of that diagnostic
		warn bool   // the finding is a warning, not an error
	}{
		{name: "base is valid", yaml: base},
		{name: "wrong version", yaml: strings.Replace(base, "version: 1", "version: 2", 1), want: "value must be 1", path: "version"},
		{name: "project name", yaml: strings.Replace(base, "project: shop", "project: Shop_1", 1), want: "lowercase letters", path: "project"},
		{name: "domain outside .localhost", yaml: strings.Replace(base, "shop.localhost", "shop.test", 1), want: ".localhost", path: "domain"},
		{name: "unknown field", yaml: base + "    colour: red\n", want: "colour", path: "modules.web"},
		{name: "no modules", yaml: "version: 1\nproject: shop\ndomain: shop.localhost\nmodules: {}\n", want: "minProperties", path: "modules"},
		{name: "unpinned latest", yaml: strings.Replace(base, "postgres:17.2", "postgres:latest", 1), want: "not pinned", path: "services.db.image"},
		{name: "unpinned floating tag", yaml: strings.Replace(base, "postgres:17.2", "postgres:alpine", 1), want: "not pinned", path: "services.db.image"},
		{name: "unpinned no tag", yaml: strings.Replace(base, "postgres:17.2", "postgres", 1), want: "not pinned", path: "services.db.image"},
		{name: "digest is pinned", yaml: strings.Replace(base, "postgres:17.2", "postgres@sha256:"+strings.Repeat("a", 64), 1)},
		{name: "unknown uses", yaml: strings.Replace(base, "uses: [db]", "uses: [db, cache]", 1), want: "unknown module or service \"cache\"", path: "modules.web.uses[1]"},
		{name: "service uses module", yaml: strings.Replace(base, "    port: 5432\n", "    port: 5432\n    uses: [web]\n", 1), want: "only use services", path: "services.db.uses[0]"},
		{name: "service cycle", yaml: strings.Replace(base, "    port: 5432\n", "    port: 5432\n    uses: [zk]\n  zk:\n    image: zookeeper:3.9\n    uses: [db]\n", 1), want: "cycle", path: "services.db.uses"},
		{name: "module cycle is a warning", yaml: strings.Replace(base, "uses: [db]", "uses: [db, api]", 1) + "  api:\n    path: api\n    runtime: node:22\n    command: node .\n    uses: [web]\n", want: "hooks will run in parallel", path: "modules.api.uses", warn: true},
		{name: "duplicate route", yaml: base + "  api:\n    path: api\n    runtime: node:22\n    command: node .\n    routes: [\"/\"]\n", want: "already served by api", path: "modules.web.routes[0]"},
		{name: "trailing slash is the same route", yaml: strings.Replace(base, `routes: ["/"]`, `routes: ["/api"]`, 1) + "  api:\n    path: api\n    runtime: node:22\n    command: node .\n    routes: [\"/api/\"]\n", want: "already served", path: "modules.web.routes[0]"},
		{name: "nested routes are fine", yaml: base + "  api:\n    path: api\n    runtime: node:22\n    command: node .\n    routes: [\"/api\"]\n"},
		{name: "route must start with slash", yaml: strings.Replace(base, `routes: ["/"]`, `routes: ["api"]`, 1), want: "does not match pattern", path: "modules.web.routes[0]"},
		{name: "repo and path", yaml: strings.Replace(base, "path: web", "path: web\n    repo: git@x:y.git", 1), want: "either repo or path", path: "modules.web"},
		{name: "runtime and image", yaml: strings.Replace(base, "runtime: node:22", "runtime: node:22\n    image: node:22", 1), want: "exactly one", path: "modules.web"},
		{name: "runtime needs command", yaml: strings.Replace(base, "    command: npm run dev\n", "", 1), want: "needs a command", path: "modules.web.command"},
		{name: "path outside repo", yaml: strings.Replace(base, "path: web", "path: ../web", 1), want: "inside the repository", path: "modules.web.path"},
		{name: "name clash", yaml: base + "  db:\n    path: db\n    runtime: node:22\n    command: x\n", want: "both a module and a service", path: "modules.db"},
		{name: "bad expression", yaml: base + "    env:\n      X: ${{ nope.url }}\n", want: "neither secrets", path: "modules.web.env.X"},
		{name: "bad attribute", yaml: base + "    env:\n      X: ${{ db.url }}\n", want: "host, port and database", path: "modules.web.env.X"},
		{name: "ref outside uses", yaml: strings.Replace(base, "uses: [db]", "uses: []", 1) + "    env:\n      X: ${{ db.host }}\n", want: "does not list it in uses", path: "modules.web.env.X", warn: true},
		{name: "plain secret", yaml: base + "    env:\n      STRIPE_SECRET: sk_test_123\n", want: "looks like a secret", path: "modules.web.env.STRIPE_SECRET", warn: true},
		{name: "dev password is not a secret", yaml: base + "    env:\n      DB_PASSWORD: postgres\n"},
		{name: "baked API URL", yaml: base + "    env:\n      VITE_API_URL: http://localhost:8000\n", want: "baked into the frontend", path: "modules.web.env.VITE_API_URL", warn: true},
		{name: "ports warning", yaml: base + "    ports: [\"3000:3000\"]\n", want: "ports are ignored", path: "modules.web.ports", warn: true},
		{name: "bind volume in module", yaml: base + "    volumes: [\"./src:/app\"]\n", want: "named volume", path: "modules.web.volumes[0]"},
		{name: "host bind in service", yaml: strings.Replace(base, "    port: 5432\n", "    port: 5432\n    volumes: [\"/var/lib/x:/data\"]\n", 1), want: "outside the repository", path: "services.db.volumes[0]", warn: true},
		{name: "expose without port", yaml: strings.Replace(base, "    port: 5432\n", "    expose: db\n", 1), want: "set ui_port", path: "services.db.expose"},
		{name: "bad cron", yaml: base + "    cron:\n      - schedule: every minute\n        command: x\n", want: "5 cron fields", path: "modules.web.cron[0].schedule"},
		{name: "bad duration", yaml: base + "features:\n  sleep_after: two hours\n", want: "duration", path: "features.sleep_after"},
		{name: "contextual isolation", yaml: base + "features:\n  isolation: contextual\n", want: "not available yet", path: "features.isolation", warn: true},
		{name: "snapshot of unknown service", yaml: base + "data:\n  snapshots:\n    anon:\n      source: mysql\n", want: "unknown service", path: "data.snapshots.anon.source"},
		{name: "dotted volume name", yaml: base + "    volumes: [\"cache.v2:/app/.cache\"]\n"},
		{name: "numbers in env", yaml: base + "    env:\n      WORKERS: 4\n      DEBUG: true\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Parse([]byte(tc.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if len(res.Diagnostics) > 0 {
					t.Fatalf("expected no diagnostics, got:\n%s", res.Diagnostics.Error())
				}
				return
			}
			for _, d := range res.Diagnostics {
				if strings.Contains(d.Message, tc.want) {
					if d.Path != tc.path {
						t.Errorf("path = %q, want %q", d.Path, tc.path)
					}
					if (d.Severity == Warning) != tc.warn {
						t.Errorf("severity = %s", d.Severity)
					}
					if d.Line == 0 {
						t.Errorf("no line for %s", d)
					}
					return
				}
			}
			t.Fatalf("no diagnostic containing %q; got:\n%s", tc.want, res.Diagnostics.Error())
		})
	}
}

func TestLoadChecksFolders(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, FileName)
	if err := os.WriteFile(file, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "does not exist") {
		t.Fatalf("got %v", res.Diagnostics)
	}
	if err := os.Mkdir(filepath.Join(dir, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if res, _ = Load(file); len(res.Diagnostics) != 0 {
		t.Fatalf("got %v", res.Diagnostics)
	}
}

func TestExpand(t *testing.T) {
	res, err := Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	w := res.Workspace
	got, err := w.Expand("postgres://${{ db.host }}:${{db.port}}/x?k=${{ secrets.KEY }}", func(r Ref) (string, error) {
		switch r.Kind {
		case RefService:
			if r.Attr == "host" {
				return "db", nil
			}
			return "5432", nil
		case RefSecret:
			return "<" + r.Name + ">", nil
		}
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "postgres://db:5432/x?k=<KEY>" {
		t.Errorf("got %q", got)
	}
	if _, err := w.Refs("${{ db.host"); err == nil {
		t.Error("unterminated expression accepted")
	}
}

func TestImagePinned(t *testing.T) {
	for img, want := range map[string]bool{
		"postgres:17.2":             true,
		"redis:7":                   true,
		"node:22-alpine":            true,
		"localhost:5000/app:1.0":    true,
		"localhost:5000/app":        false,
		"redis:alpine":              false,
		"debian:bookworm":           false,
		"nginx:latest":              false,
		"nginx":                     false,
		"app:${TAG}":                false,
		"ghcr.io/acme/app@sha256:1": true,
	} {
		if got := ImagePinned(img); got != want {
			t.Errorf("ImagePinned(%q) = %v", img, got)
		}
	}
}
