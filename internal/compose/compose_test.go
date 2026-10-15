package compose

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"

	"github.com/devcorb/devcorb/internal/workspace"
)

type fakePinner map[string]string

func (f fakePinner) Digest(_ context.Context, image string) (string, error) {
	if d, ok := f[image]; ok {
		return d, nil
	}
	if image == "redis:alpine" {
		return "", ErrRateLimited
	}
	return "", os.ErrNotExist
}

const mailpitDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func importShop(t *testing.T, pinner Pinner) *Result {
	t.Helper()
	res, err := Import(context.Background(), []string{"testdata/shop/compose.yaml"}, Options{Pinner: pinner})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestImport(t *testing.T) {
	res := importShop(t, fakePinner{"axllent/mailpit": mailpitDigest})
	w, rep := res.Workspace, res.Report

	if w.Project != "shop" || w.Domain != "shop.localhost" {
		t.Errorf("project %q domain %q", w.Project, w.Domain)
	}
	if got := strings.Join(w.ModuleNames(), ","); got != "api,queue-worker,web" {
		t.Errorf("modules = %s", got)
	}
	if got := strings.Join(w.ServiceNames(), ","); got != "cache,db,mailpit,nginx" {
		t.Errorf("services = %s", got)
	}

	api := w.Modules["api"]
	if api.Path != "api" || api.Build.Dockerfile != "Dockerfile.dev" || api.Build.Target != "dev" || api.Build.Args["PHP_VERSION"] != "8.3" {
		t.Errorf("api build = %+v path %q", api.Build, api.Path)
	}
	if api.Port != 9000 || len(api.Routes) != 0 {
		t.Errorf("api port %d routes %v", api.Port, api.Routes)
	}
	if strings.Join(api.Uses, ",") != "cache,db" {
		t.Errorf("api uses = %v", api.Uses)
	}
	if api.Env["JWT_SECRET"] != "${{ secrets.JWT_SECRET }}" || res.Secrets["JWT_SECRET"] != "dev-jwt-secret" {
		t.Errorf("secret not moved: %q / %v", api.Env["JWT_SECRET"], res.Secrets)
	}
	if !strings.Contains(api.Env["DATABASE_URL"], "devpass@db") {
		t.Errorf("interpolation lost: %q", api.Env["DATABASE_URL"])
	}

	web := w.Modules["web"]
	if strings.Join(web.Routes, ",") != "/" || web.Port != 3000 {
		t.Errorf("web routes %v port %d", web.Routes, web.Port)
	}
	if strings.Join(web.Volumes, ",") != "web-node-modules:/app/node_modules" {
		t.Errorf("web volumes = %v (code mount must be dropped)", web.Volumes)
	}
	if w.Modules["queue-worker"].Command != "php artisan queue:work --tries=3" {
		t.Errorf("worker command = %q", w.Modules["queue-worker"].Command)
	}

	db := w.Services["db"]
	if db.Port != 0 || db.Healthcheck == nil || db.Healthcheck.Interval != "5s" || db.Healthcheck.Retries != 10 {
		t.Errorf("db = %+v hc %+v", db, db.Healthcheck)
	}
	if db.Ulimits["nofile"].Soft != 1024 {
		t.Errorf("ulimits = %+v", db.Ulimits)
	}
	if strings.Join(db.Volumes, ",") != "pgdata:/var/lib/postgresql/data" {
		t.Errorf("db volumes = %v (host bind must be dropped)", db.Volumes)
	}
	if res.Secrets["POSTGRES_PASSWORD"] != "devpass" {
		t.Errorf("secrets = %v", res.Secrets)
	}

	nginx := w.Services["nginx"]
	if len(nginx.Uses) != 0 || nginx.Expose != "nginx" {
		t.Errorf("nginx uses %v expose %q", nginx.Uses, nginx.Expose)
	}
	if strings.Join(nginx.Volumes, ",") != "./api/nginx.conf:/etc/nginx/conf.d/default.conf" {
		t.Errorf("nginx volumes = %v", nginx.Volumes)
	}

	mail := w.Services["mailpit"]
	if !mail.Optional || mail.Image != "axllent/mailpit@"+mailpitDigest {
		t.Errorf("mailpit = %+v", mail)
	}

	for _, want := range []string{"queue_worker → queue-worker"} {
		if !slices.Contains(rep.Renamed, want) {
			t.Errorf("renamed %v lacks %q", rep.Renamed, want)
		}
	}
	if !slices.Contains(rep.ServiceOnModule, "nginx → api") {
		t.Errorf("service on module = %v", rep.ServiceOnModule)
	}
	if !slices.Contains(rep.MissingEnvFiles, "api/.env") {
		t.Errorf("missing env files = %v", rep.MissingEnvFiles)
	}
	if !slices.Contains(rep.HostMounts, "db: /tmp/dumps → /dumps") {
		t.Errorf("host mounts = %v", rep.HostMounts)
	}
	if !slices.Contains(rep.PinLater, "cache: redis:alpine") {
		t.Errorf("pin later = %v", rep.PinLater)
	}
	if rep.CodeMounts != 2 || rep.DroppedPorts != 4 {
		t.Errorf("code mounts %d dropped ports %d", rep.CodeMounts, rep.DroppedPorts)
	}
}

// TestImportValidates: the draft, written and read back, passes validation once the
// one image the registry would not resolve is pinned.
func TestImportValidates(t *testing.T) {
	res := importShop(t, fakePinner{"axllent/mailpit": mailpitDigest, "redis:alpine": "sha256:" + strings.Repeat("2", 64)})
	if lvl := res.Report.Level(false); lvl != "B" {
		t.Errorf("level = %s", lvl)
	}
	data, err := yaml.Marshal(res.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := workspace.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if errs := parsed.Diagnostics.Errors(); len(errs) > 0 {
		t.Fatalf("draft does not validate:\n%s\n---\n%s", errs.Error(), data)
	}
}

func TestImportWithoutPinnerIsBlocked(t *testing.T) {
	res := importShop(t, nil)
	if res.Report.Level(false) != "C" || len(res.Report.Unpinned) != 2 {
		t.Errorf("level %s unpinned %v", res.Report.Level(false), res.Report.Unpinned)
	}
}

func TestExportRoundTrip(t *testing.T) {
	ws, err := workspace.Load("../workspace/testdata/shop.yaml")
	if err != nil {
		t.Fatal(err)
	}
	out, err := Export(ws.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(file, out, 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := loader.LoadWithContext(context.Background(), types.ConfigDetails{
		WorkingDir:  dir,
		ConfigFiles: []types.ConfigFile{{Filename: file}},
		Environment: map[string]string{"STRIPE_TEST_KEY": "sk_test"},
	}, func(o *loader.Options) { o.SetProjectName("shop", true) })
	if err != nil {
		t.Fatalf("exported file does not load: %v\n%s", err, out)
	}

	be := p.Services["backend"]
	if be.Image != "php:8.3" || be.WorkingDir != "/app" {
		t.Errorf("backend = %s %s", be.Image, be.WorkingDir)
	}
	if got := strings.Join(be.Command, " "); got != "sh -c composer install && php artisan migrate --force && php artisan serve --host 0.0.0.0 --port 8080" {
		t.Errorf("backend command = %q", got)
	}
	env := func(s, k string) string {
		if v := p.Services[s].Environment[k]; v != nil {
			return *v
		}
		return ""
	}
	for k, want := range map[string]string{
		"DB_HOST":     "mysql",
		"DB_DATABASE": "shop",
		"APP_URL":     "http://localhost:8080",
		"STRIPE_KEY":  "sk_test",
	} {
		if got := env("backend", k); got != want {
			t.Errorf("backend %s = %q, want %q", k, got, want)
		}
	}
	if fe := p.Services["frontend"]; len(fe.Ports) != 1 || fe.Ports[0].Published != "8080" || fe.Ports[0].HostIP != "127.0.0.1" || fe.Ports[0].Target != 5173 {
		t.Errorf("frontend ports = %+v", fe.Ports)
	}
	if mail := p.Services["mail"]; len(mail.Ports) != 1 || mail.Ports[0].Target != 8025 {
		t.Errorf("mail ports = %+v", mail.Ports)
	}
	if _, ok := p.Services["backend-worker-1"]; !ok {
		t.Errorf("worker service missing")
	}
	if _, ok := p.Volumes["node-modules"]; !ok {
		t.Errorf("named volume not declared")
	}
	if !bytes.Contains(out, []byte("clone git@github.com:acme/shop-api.git into ./backend")) {
		t.Errorf("header lacks clone note:\n%s", out)
	}

	// docker compose itself is the final judge when it is installed.
	if _, err := exec.LookPath("docker"); err == nil {
		cmd := exec.Command("docker", "compose", "-f", file, "config", "-q")
		cmd.Env = append(os.Environ(), "STRIPE_TEST_KEY=x")
		if o, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("docker compose config: %v\n%s", err, o)
		}
	}
}

// TestImportExportImport: exporting an imported project and importing it again keeps
// modules, services and routes.
func TestImportExportImport(t *testing.T) {
	res := importShop(t, fakePinner{"axllent/mailpit": mailpitDigest, "redis:alpine": "sha256:" + strings.Repeat("2", 64)})
	out, err := Export(res.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join("testdata/shop", "exported.compose.yaml")
	if err := os.WriteFile(file, out, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(file) })
	again, err := Import(context.Background(), []string{file}, Options{Project: "shop"})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := strings.Join(again.Workspace.ModuleNames(), ","); got != "api,queue-worker,web" {
		t.Errorf("modules = %s", got)
	}
	if got := strings.Join(again.Workspace.ServiceNames(), ","); got != "cache,db,mailpit,nginx" {
		t.Errorf("services = %s", got)
	}
}
