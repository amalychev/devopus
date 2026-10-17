package compose

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/amalychev/devopus/internal/workspace"
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
	if w.Modules["web"].Mount != "/app" || w.Modules["api"].Mount != "/var/www" {
		t.Errorf("mounts: web %q api %q", w.Modules["web"].Mount, w.Modules["api"].Mount)
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
