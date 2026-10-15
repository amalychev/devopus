package reconcile

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agentv1 "github.com/devopus/devopus/gen/devopus/agent/v1"
	"github.com/devopus/devopus/internal/agent/proxy"
	"github.com/devopus/devopus/internal/agent/stack"
)

const appYAML = `version: 1
project: app
domain: app.localhost
services:
  db:
    image: postgres:17.2
    port: 5432
    volumes: ["pgdata:/var/lib/postgresql/data"]
    healthcheck: {test: pg_isready}
  mail:
    image: axllent/mailpit:v1.21
    port: 1025
    ui_port: 8025
    expose: mail
modules:
  web:
    path: web
    runtime: node:22
    routes: ["/"]
    command: npm run dev
    hooks: {install: npm ci}
  api:
    path: api
    build: {context: api}
    mount: /srv
    port: 9000
    routes: ["/api"]
    uses: [db]
    env:
      KEY: ${{ secrets.KEY }}
    hooks: {install: composer install, migrate: php artisan migrate}
    workers: [{command: php artisan queue:work}]
`

// appProject is a repository with two modules and a secret.
func appProject(t *testing.T) (root string, state func(rev uint64, phase agentv1.DesiredPhase) *agentv1.DesiredState) {
	t.Helper()
	root = t.TempDir()
	for _, d := range []string{"web", "api"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(root, "api", "Dockerfile"), []byte("FROM php:8.3\n"), 0o644)
	os.WriteFile(filepath.Join(root, "web", "package-lock.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(root, SecretsFile), []byte("KEY=\"s3cr3t\"\n"), 0o600)
	return root, func(rev uint64, phase agentv1.DesiredPhase) *agentv1.DesiredState {
		env := &agentv1.EnvironmentSpec{Id: "bbbbbb", Project: "app", Kind: agentv1.EnvironmentKind_ENVIRONMENT_KIND_BASE, Phase: phase}
		var envs []*agentv1.EnvironmentSpec
		if phase != agentv1.DesiredPhase_DESIRED_PHASE_UNSPECIFIED {
			envs = append(envs, env)
		}
		return &agentv1.DesiredState{Revision: rev, Environments: envs,
			Projects: []*agentv1.ProjectSpec{{Name: "app", Root: root, Config: []byte(appYAML)}}}
	}
}

func TestModulesComeUp(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newFake())
	asked := map[string]int{}
	h.opt.Answers = func(_ context.Context, host, path string, _ proxy.Ports) error {
		asked[host+path]++
		return nil
	}
	r := h.new(nil)
	root, state := appProject(t)
	running := agentv1.DesiredPhase_DESIRED_PHASE_RUNNING
	if err := r.Reconcile(ctx, state(1, running)); err != nil {
		t.Fatal(err)
	}
	st := r.Statuses()[0]
	if st.Phase != agentv1.Phase_PHASE_RUNNING {
		t.Fatalf("phase %v: %s", st.Phase, st.Error)
	}

	// Install everywhere, then migrate; the image of api is built once.
	if got := strings.Join(h.rt.hooks, "; "); got != "sh -c composer install; sh -c npm ci; sh -c php artisan migrate" &&
		got != "sh -c npm ci; sh -c composer install; sh -c php artisan migrate" {
		t.Errorf("hooks: %s", got)
	}
	if h.rt.builds != 1 || len(h.rt.vols) != 1 {
		t.Errorf("%d builds, volumes %v", h.rt.builds, h.rt.vols)
	}
	if len(h.rt.byRole(stack.RoleHook)) != 0 {
		t.Error("hook containers are left behind")
	}
	api := h.rt.byRole(stack.RoleModule)
	if len(api) != 2 || len(h.rt.byRole(stack.RoleWorker)) != 1 || len(h.rt.byRole(stack.RoleService)) != 2 {
		t.Fatalf("modules %d workers %d services %d", len(api), len(h.rt.byRole(stack.RoleWorker)), len(h.rt.byRole(stack.RoleService)))
	}
	for _, c := range api {
		spec := h.rt.specs[c.ID]
		if c.Name == "app-env-bbbbbb-api" && (!slices.Contains(spec.Env, "KEY=s3cr3t") || spec.Binds[0].Target != "/srv" || spec.Binds[0].Source != filepath.Join(root, "api")) {
			t.Errorf("api spec: env %v binds %v", spec.Env, spec.Binds)
		}
	}

	// The proxy sends paths to modules and the mail host to the web UI.
	cfg := h.xds.last()
	if len(cfg.Sites) != 1 || len(cfg.Sites[0].VHosts) != 2 {
		t.Fatalf("sites %+v", cfg.Sites)
	}
	main, mail := cfg.Sites[0].VHosts[0], cfg.Sites[0].VHosts[1]
	if len(main.Routes) != 2 || mail.Hosts[0] != "mail.app.localhost" || mail.Routes[0].Upstream.Addr.Port() != 8025 {
		t.Errorf("vhosts %+v %+v", main, mail)
	}
	var routes []string
	for _, rt := range r.Routes() {
		routes = append(routes, rt.Host+rt.Path+" → "+strings.Fields(rt.Target)[0]+" "+strings.Fields(rt.Target)[1])
	}
	if strings.Join(routes, "; ") != "app.localhost/ → module web; app.localhost/api → module api; mail.app.localhost/ → service mail" {
		t.Errorf("routes %v", routes)
	}
	if asked["app.localhost/"] != 1 || asked["app.localhost/api"] != 1 {
		t.Errorf("readiness asked %v", asked)
	}
	var units []string
	for _, u := range st.Units {
		units = append(units, u.Name+":"+u.State)
	}
	if strings.Join(units, ",") != "db:running,mail:running,api:running,api-worker-1:running,web:running" {
		t.Errorf("units %v", units)
	}

	// A converged pass runs no hooks, builds nothing, restarts nothing, asks nobody.
	created := h.rt.created
	if err := r.Reconcile(ctx, state(1, running)); err != nil {
		t.Fatal(err)
	}
	if len(h.rt.hooks) != 3 || h.rt.builds != 1 || h.rt.created != created || h.rt.stopped != 0 || asked["app.localhost/"] != 1 {
		t.Errorf("second pass: hooks %v builds %d created %d stopped %d asked %v", h.rt.hooks, h.rt.builds, h.rt.created-created, h.rt.stopped, asked)
	}

	// A changed Dockerfile builds a new image and drops the old one.
	oldTag := h.rt.specs[api[0].ID].Image
	if api[0].Name != "app-env-bbbbbb-api" {
		oldTag = h.rt.specs[api[1].ID].Image
	}
	os.WriteFile(filepath.Join(root, "api", "Dockerfile"), []byte("FROM php:8.4\n"), 0o644)
	if err := r.Reconcile(ctx, state(1, running)); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.rt.built[oldTag]; ok || h.rt.builds != 2 || len(h.rt.built) != 1 {
		t.Errorf("after a Dockerfile change: builds %d images %v", h.rt.builds, h.rt.built)
	}
	// Another image means another environment for the hooks: they run again.
	if got := strings.Join(h.rt.hooks[3:], "; "); got != "sh -c composer install; sh -c php artisan migrate" {
		t.Errorf("hooks after a new image: %s", got)
	}
	stoppedBefore := h.rt.stopped

	// A changed lock file reruns install and restarts that module only.
	os.WriteFile(filepath.Join(root, "web", "package-lock.json"), []byte(`{"lockfileVersion":3}`), 0o644)
	if err := r.Reconcile(ctx, state(2, running)); err != nil {
		t.Fatal(err)
	}
	if len(h.rt.hooks) != 6 || h.rt.hooks[5] != "sh -c npm ci" || h.rt.stopped != stoppedBefore+1 || asked["app.localhost/"] != 3 {
		t.Errorf("after the lock file changed: hooks %v stopped %d asked %v", h.rt.hooks, h.rt.stopped, asked)
	}

	// Stopped: containers stop and stay, the volume stays, the site says so.
	if err := r.Reconcile(ctx, state(3, agentv1.DesiredPhase_DESIRED_PHASE_STOPPED)); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{stack.RoleModule, stack.RoleService, stack.RoleWorker} {
		for _, c := range h.rt.byRole(role) {
			if c.Running {
				t.Errorf("%s still runs", c.Name)
			}
		}
	}
	if st := r.Statuses()[0]; st.Phase != agentv1.Phase_PHASE_STOPPED || len(st.Units) != 5 || st.Units[0].State != "exited" {
		t.Errorf("stopped: %v units %v", st.Phase, st.Units)
	}
	if h.xds.last().Sites[0].Page.Status != 503 || len(h.rt.vols) != 1 {
		t.Error("a stopped environment must keep its data and answer 503")
	}

	// Removed with its data: containers, volumes and the environment's files go.
	if err := r.Reconcile(ctx, state(4, agentv1.DesiredPhase_DESIRED_PHASE_UNSPECIFIED)); err != nil {
		t.Fatal(err)
	}
	if len(h.rt.ctrs) != 0 || len(h.rt.vols) != 0 {
		t.Errorf("left: %d containers, volumes %v", len(h.rt.ctrs), h.rt.vols)
	}
	// The project stays (only its environment went): its image stays too.
	if len(h.rt.built) != 1 {
		t.Errorf("built images %v", h.rt.built)
	}
	gone := state(5, agentv1.DesiredPhase_DESIRED_PHASE_UNSPECIFIED)
	gone.Projects = nil
	if err := r.Reconcile(ctx, gone); err != nil {
		t.Fatal(err)
	}
	if len(h.rt.built) != 0 {
		t.Errorf("images of a removed project stay: %v", h.rt.built)
	}
	if _, err := os.Stat(filepath.Join(h.opt.EnvsDir, "bbbbbb")); !os.IsNotExist(err) {
		t.Errorf("hook marks stay: %v", err)
	}
}

func TestHookFailure(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newFake())
	h.rt.hookExit["sh -c php artisan migrate"] = 1
	r := h.new(nil)
	_, state := appProject(t)
	if err := r.Reconcile(ctx, state(1, agentv1.DesiredPhase_DESIRED_PHASE_RUNNING)); err == nil {
		t.Fatal("a failed migration must fail the pass")
	}
	st := r.Statuses()[0]
	if st.Phase != agentv1.Phase_PHASE_ERROR || !strings.Contains(st.Error, `migrate: api: "php artisan migrate" exited with 1`) {
		t.Errorf("status %v: %s", st.Phase, st.Error)
	}
	if !strings.Contains(st.Error, filepath.Join("bbbbbb", "logs", "api-migrate.log")) {
		t.Errorf("the error does not point at the log: %s", st.Error)
	}
	if len(h.rt.byRole(stack.RoleModule)) != 0 {
		t.Error("modules started although their migration failed")
	}
	// The services that did start stay reachable through the proxy.
	if len(h.xds.last().Sites[0].VHosts) == 0 {
		t.Error("the site has no routes after a failed hook")
	}

	// Fixed: the next pass migrates and starts the modules; install does not run again.
	delete(h.rt.hookExit, "sh -c php artisan migrate")
	if err := r.Reconcile(ctx, state(1, agentv1.DesiredPhase_DESIRED_PHASE_RUNNING)); err != nil {
		t.Fatal(err)
	}
	if n := len(h.rt.hooks); n != 4 || h.rt.hooks[3] != "sh -c php artisan migrate" {
		t.Errorf("hooks %v", h.rt.hooks)
	}
}

func TestModuleStillStarting(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newFake())
	h.opt.ReadyTimeout = 1
	h.opt.Answers = func(context.Context, string, string, proxy.Ports) error { return proxy.ErrNoAnswer }
	r := h.new(nil)
	_, state := appProject(t)
	if err := r.Reconcile(ctx, state(1, agentv1.DesiredPhase_DESIRED_PHASE_RUNNING)); err != ErrNotReady {
		t.Fatalf("err = %v", err)
	}
	st := r.Statuses()[0]
	ready := findStep(st, "ready")
	if st.Phase != agentv1.Phase_PHASE_CREATING || ready.State != agentv1.StepState_STEP_STATE_RUNNING || !strings.Contains(ready.Message, "waiting for") {
		t.Errorf("phase %v ready %v %q", st.Phase, ready.State, ready.Message)
	}

	// A module that exits fails the environment with its output.
	h.rt.crash[stack.RoleModule] = true
	for _, c := range h.rt.byRole(stack.RoleModule) {
		h.rt.StopContainer(ctx, c.ID)
	}
	err := r.Reconcile(ctx, state(2, agentv1.DesiredPhase_DESIRED_PHASE_RUNNING))
	st = r.Statuses()[0]
	if err == nil || st.Phase != agentv1.Phase_PHASE_ERROR || !strings.Contains(st.Error, "exited") {
		t.Errorf("err %v phase %v: %s", err, st.Phase, st.Error)
	}
}

func TestCloneRepoModule(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ctx := context.Background()
	// A repository with a commit on main, served over file://.
	repo := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(repo, "init", "-b", "main")
	os.WriteFile(filepath.Join(repo, "index.html"), []byte("hi"), 0o644)
	run(repo, "add", ".")
	run(repo, "commit", "-m", "init")

	root := t.TempDir()
	cfg := "version: 1\nproject: site\ndomain: site.localhost\nmodules:\n  web:\n    repo: file://" + repo + "\n    image: nginx:1.27.4\n    mount: /usr/share/nginx/html\n    port: 80\n    routes: [/]\n"
	h := newHarness(t, newFake())
	r := h.new(nil)
	state := func(phase agentv1.DesiredPhase) *agentv1.DesiredState {
		d := &agentv1.DesiredState{Revision: 1, Projects: []*agentv1.ProjectSpec{{Name: "site", Root: root, Config: []byte(cfg)}}}
		if phase != agentv1.DesiredPhase_DESIRED_PHASE_UNSPECIFIED {
			d.Environments = []*agentv1.EnvironmentSpec{{Id: "cccccc", Project: "site", Kind: agentv1.EnvironmentKind_ENVIRONMENT_KIND_BASE, Phase: phase}}
		}
		return d
	}
	if err := r.Reconcile(ctx, state(agentv1.DesiredPhase_DESIRED_PHASE_RUNNING)); err != nil {
		t.Fatal(err)
	}
	src := r.SourceDir("site", "cccccc", "web")
	if data, err := os.ReadFile(filepath.Join(src, "index.html")); err != nil || string(data) != "hi" {
		t.Fatalf("checkout: %v %q", err, data)
	}
	web := h.rt.byRole(stack.RoleModule)[0]
	if b := h.rt.specs[web.ID].Binds[0]; b.Source != src || b.Target != "/usr/share/nginx/html" {
		t.Errorf("bind %+v", b)
	}

	// Removal keeps a checkout with work that exists nowhere else.
	os.WriteFile(filepath.Join(src, "draft.html"), []byte("wip"), 0o644)
	if err := r.Reconcile(ctx, state(agentv1.DesiredPhase_DESIRED_PHASE_UNSPECIFIED)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(src, "draft.html")); err != nil {
		t.Errorf("unsaved work was removed: %v", err)
	}
}
