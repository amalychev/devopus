package agent

import (
	"context"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/devopus/devopus/gen/devopus/agent/v1"
	"github.com/devopus/devopus/internal/agent/client"
	"github.com/devopus/devopus/internal/agent/paths"
	"github.com/devopus/devopus/internal/agent/proxy"
	"github.com/devopus/devopus/internal/agent/runtime"
	"github.com/devopus/devopus/internal/agent/store"
)

const featureConfig = `version: 1
project: %s
domain: %s.localhost
services:
  store:
    image: python:3.12-slim
    port: 9000
    env:
      ENV_ID: ${{ env.id }}
    command: sh -c 'mkdir -p /tmp/s && echo store-$ENV_ID > /tmp/s/i.html && exec python -m http.server 9000 --directory /tmp/s'
modules:
  web:
    path: web
    image: python:3.12-slim
    mount: /srv/web
    routes: [/]
    uses: [api]
    command: python -m http.server 8080 --directory /srv/web
  api:
    path: api
    image: python:3.12-slim
    mount: /srv/api
    routes: [/api]
    uses: [store]
    command: python -m http.server 8080 --directory /srv
features:
  database: %s
`

// TestFeatures runs two features of one project against the real runtime (US-4):
// each gets its worktrees, URL and containers, reaches what it shares with the base
// environment by logical names, and neither the features nor their creation and
// removal disturb the others.
func TestFeatures(t *testing.T) {
	rt, err := runtime.New(runtime.Detect())
	if err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	if _, err := rt.Info(ctx); err != nil {
		t.Skipf("no container runtime: %v", err)
	}
	// Under the home folder (Colima shares only it) and short (unix socket paths).
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.MkdirTemp(userHome, ".dk-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("DEVOPUS_HOME", home)
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"} {
		t.Setenv(k, v)
	}

	// The project is a git repository with its modules in folders.
	project := "ft-" + store.NewID()
	root := filepath.Join(home, "repo")
	os.MkdirAll(filepath.Join(root, "web"), 0o755)
	os.MkdirAll(filepath.Join(root, "api"), 0o755)
	os.WriteFile(filepath.Join(root, "web", "index.html"), []byte("web main"), 0o644)
	os.WriteFile(filepath.Join(root, "api", "index.html"), []byte("api main"), 0o644)
	config := fmt.Appendf(nil, featureConfig, project, project, "clone")
	os.WriteFile(filepath.Join(root, "workspace.yaml"), config, 0o644)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"-c", "commit.gpgsign=false", "commit", "-q", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	wsPath := filepath.Join(root, "workspace.yaml")

	ports := proxy.Ports{HTTP: freePort(t), HTTPS: freePort(t)}
	done := make(chan error, 1)
	// Not the test's deadline: the agent must outlive it to clean up after a failure.
	agentCtx, stop := context.WithCancel(context.Background())
	logPath := filepath.Join(t.TempDir(), "agent.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		logFile.Close()
		if t.Failed() {
			data, _ := os.ReadFile(logPath)
			t.Logf("agent log:\n%s", data)
		}
	})
	go func() {
		done <- Run(agentCtx, Options{Version: "test", Log: slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelDebug})), ProxyPorts: &ports})
	}()
	t.Cleanup(func() {
		stop()
		<-done
	})
	c := client.New(paths.Socket())
	for i := 0; client.Ping(ctx, c) != nil; i++ {
		if i > 100 {
			t.Fatal("agent did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Cleanup(func() {
		purge(t, rt, c, project)
	})

	up, err := c.Up(ctx, connect.NewRequest(&agentv1.UpRequest{WorkspacePath: wsPath, Config: config}))
	if err != nil {
		t.Fatal(err)
	}
	baseID := up.Msg.EnvironmentId
	waitFor(t, ctx, c, baseID, func(s *agentv1.EnvironmentStatus) bool { return s.Phase == agentv1.Phase_PHASE_RUNNING })
	status, err := c.Status(ctx, connect.NewRequest(&agentv1.StatusRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	pemData, _ := os.ReadFile(status.Msg.Ca.RootPath)
	roots.AppendCertsFromPEM(pemData)
	host := project + ".localhost"
	get := func(host, path string) string {
		_, body := httpsGetPath(t, ctx, "127.0.0.1", ports.HTTPS, host, path, roots)
		return strings.TrimSpace(body)
	}
	// A file just written on the host reaches the container a moment later: virtiofs on
	// Docker Desktop caches its size for a while.
	eventually := func(want string, fn func() string) string {
		got := fn()
		for i := 0; got != want && i < 30; i++ {
			time.Sleep(200 * time.Millisecond)
			got = fn()
		}
		return got
	}
	if b := get(host, "/api/"); b != "api main" {
		t.Fatalf("base /api/ = %q", b)
	}
	inside := func(env, unit, url string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", "exec", project+"-env-"+env+"-"+unit, "python", "-c",
			"import urllib.request;print(urllib.request.urlopen('"+url+"', timeout=5).read().decode())").CombinedOutput()
		if err != nil {
			return fmt.Sprintf("error: %v %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}

	// Requests to the base environment keep succeeding while features come and go.
	var failures, requests atomic.Int64
	var lastFailure atomic.Value
	pollCtx, stopPoll := context.WithCancel(ctx)
	var polling sync.WaitGroup
	polling.Add(1)
	go func() {
		defer polling.Done()
		for pollCtx.Err() == nil {
			_, body, err := tryGet(pollCtx, ports.HTTPS, host, "/api/", roots)
			if pollCtx.Err() != nil {
				return
			}
			requests.Add(1)
			if err != nil || strings.TrimSpace(body) != "api main" {
				failures.Add(1)
				lastFailure.Store(fmt.Sprintf("%q %v", body, err))
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	// Feature A forks api: web uses api, so it runs in the feature too; store is its own.
	a, err := c.CreateFeature(ctx, connect.NewRequest(&agentv1.CreateFeatureRequest{WorkspacePath: wsPath, Config: config, Name: "PAY-1", Modules: []string{"api"}}))
	if err != nil {
		t.Fatal(err)
	}
	aID := a.Msg.EnvironmentId
	if a.Msg.Slug != "pay-1" || a.Msg.Branch != "feature/PAY-1" || a.Msg.BaseEnvironmentId != baseID {
		t.Errorf("create = %v", a.Msg)
	}
	fa := waitFor(t, ctx, c, aID, func(s *agentv1.EnvironmentStatus) bool { return s.Phase == agentv1.Phase_PHASE_RUNNING })
	hostA := "pay-1." + host
	if want := fmt.Sprintf("https://%s:%d", hostA, ports.HTTPS); fa.Url != want {
		t.Errorf("url = %s, want %s", fa.Url, want)
	}
	if fa.CodeRoot == "" || len(fa.Sources) != 2 {
		t.Fatalf("sources = %v, code root %q", fa.Sources, fa.CodeRoot)
	}
	for _, s := range fa.Sources {
		if s.Module == "api" && s.Branch != "feature/PAY-1" || s.Module == "web" && s.Branch != "feature/PAY-1" {
			// One worktree of the repository: it is on the feature branch.
			t.Errorf("source %v", s)
		}
	}
	// The feature serves its own worktree: a change there shows only in the feature.
	os.WriteFile(filepath.Join(fa.CodeRoot, "api", "index.html"), []byte("api PAY-1"), 0o644)
	if b := eventually("api PAY-1", func() string { return get(hostA, "/api/") }); b != "api PAY-1" {
		t.Errorf("feature /api/ = %q", b)
	}
	if b := get(host, "/api/"); b != "api main" {
		t.Errorf("base /api/ after the feature's change = %q", b)
	}
	// Inside the feature the logical names are the feature's own units.
	if b := inside(aID, "web", "http://api:8080/api/"); b != "api PAY-1" {
		t.Errorf("web→api in PAY-1 = %q", b)
	}
	if b := inside(aID, "api", "http://store:9000/i.html"); b != "store-"+aID {
		t.Errorf("api→store in PAY-1 = %q", b)
	}
	if b := inside(baseID, "api", "http://store:9000/i.html"); b != "store-"+baseID {
		t.Errorf("api→store in the base = %q", b)
	}

	// Feature B forks only web: api and the paths it serves come from the base.
	b, err := c.CreateFeature(ctx, connect.NewRequest(&agentv1.CreateFeatureRequest{WorkspacePath: wsPath, Config: config, Name: "ui-2", Modules: []string{"web"}}))
	if err != nil {
		t.Fatal(err)
	}
	bID := b.Msg.EnvironmentId
	fb := waitFor(t, ctx, c, bID, func(s *agentv1.EnvironmentStatus) bool { return s.Phase == agentv1.Phase_PHASE_RUNNING })
	hostB := "ui-2." + host
	if len(fb.Shared) != 1 || fb.Shared[0] != "api" {
		t.Errorf("shared = %v", fb.Shared)
	}
	os.WriteFile(filepath.Join(fb.CodeRoot, "web", "index.html"), []byte("web ui-2"), 0o644)
	if b := eventually("web ui-2", func() string { return get(hostB, "/") }); b != "web ui-2" {
		t.Errorf("ui-2 / = %q", b)
	}
	if b := get(hostB, "/api/"); b != "api main" {
		t.Errorf("ui-2 /api/ = %q (the base's api)", b)
	}
	if b := inside(bID, "web", "http://api:8080/api/"); b != "api main" {
		t.Errorf("web→api in ui-2 = %q (through the proxy to the base)", b)
	}
	if b := get(hostA, "/"); b != "web main" {
		t.Errorf("PAY-1 / = %q: ui-2's change leaked", b)
	}
	// Each network holds only its environment's containers, the proxy and the DNS server.
	for _, env := range []string{baseID, aID, bID} {
		out, err := exec.CommandContext(ctx, "docker", "network", "inspect", "-f", "{{range .Containers}}{{.Name}} {{end}}", project+"-env-"+env).CombinedOutput()
		if err != nil {
			t.Fatal(err, string(out))
		}
		for _, name := range strings.Fields(string(out)) {
			if !strings.HasPrefix(name, project+"-env-"+env+"-") && !strings.HasPrefix(name, "devopus-proxy") {
				t.Errorf("network of %s has %s", env, name)
			}
		}
	}

	// database: shared — PAY-1 drops its store and reaches the base's over TCP.
	shared := fmt.Appendf(nil, featureConfig, project, project, "shared")
	add, err := c.AddFeatureModules(ctx, connect.NewRequest(&agentv1.AddFeatureModulesRequest{WorkspacePath: wsPath, Config: shared, Feature: "pay-1", Modules: []string{"api"}}))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, c, aID, func(s *agentv1.EnvironmentStatus) bool {
		return s.Phase == agentv1.Phase_PHASE_RUNNING && s.AppliedRevision >= add.Msg.Revision
	})
	if b := inside(aID, "api", "http://store:9000/i.html"); b != "store-"+baseID {
		t.Errorf("api→store in PAY-1 with a shared database = %q", b)
	}
	if running(t, ctx, rt, project+"-env-"+aID+"-store") {
		t.Error("PAY-1 still runs its own store")
	}

	// PAY-1 has uncommitted work: removal refuses without --force.
	_, err = c.RemoveFeature(ctx, connect.NewRequest(&agentv1.RemoveFeatureRequest{Project: project, Feature: "PAY-1"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("remove with work: %v", err)
	}
	rmB, err := c.RemoveFeature(ctx, connect.NewRequest(&agentv1.RemoveFeatureRequest{Project: project, Feature: "ui-2", Force: true}))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, c, bID, func(s *agentv1.EnvironmentStatus) bool { return s.Removed && s.AppliedRevision >= rmB.Msg.Revision })
	if _, err := os.Stat(fb.CodeRoot); !os.IsNotExist(err) {
		t.Errorf("ui-2's worktree stays: %v", err)
	}
	if hasNetwork(t, ctx, rt, bID) {
		t.Error("ui-2's network stays")
	}
	if out, _ := exec.Command("git", "-C", root, "branch", "--list", "feature/ui-2").CombinedOutput(); !strings.Contains(string(out), "feature/ui-2") {
		t.Error("removing a feature removed its branch")
	}
	stopPoll()
	polling.Wait()
	if n := requests.Load(); n < 20 {
		t.Errorf("only %d requests to the base while features came and went", n)
	}
	if n := failures.Load(); n > 0 {
		t.Errorf("%d of %d requests to the base failed while features came and went; last: %v", n, requests.Load(), lastFailure.Load())
	}
	if b := get(hostA, "/api/"); b != "api PAY-1" {
		t.Errorf("PAY-1 after ui-2 left: %q", b)
	}

	if _, err := c.Down(ctx, connect.NewRequest(&agentv1.DownRequest{Project: project, Purge: true})); err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		st, err := c.Status(ctx, connect.NewRequest(&agentv1.StatusRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Msg.Environments) == 0 {
			break
		}
		if i > 300 {
			t.Fatalf("environments left after purge: %v", st.Msg.Environments)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// The last environment is gone, so is the proxy.
	for i := 0; len(containers(t, ctx, rt, "")) > 0; i++ {
		if i > 100 {
			t.Fatalf("containers left behind: %v", containers(t, ctx, rt, ""))
		}
		time.Sleep(100 * time.Millisecond)
	}
	// PAY-1's worktree had uncommitted work: purging keeps it.
	if _, err := os.Stat(filepath.Join(fa.CodeRoot, "api", "index.html")); err != nil {
		t.Errorf("purge removed a worktree with uncommitted work: %v", err)
	}
}

func tryGet(ctx context.Context, port uint16, host, path string, roots *x509.CertPool) (int, string, error) {
	return httpsTry(ctx, "127.0.0.1", port, host, path, roots)
}
