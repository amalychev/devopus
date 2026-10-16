package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/amalychev/devopus/gen/devopus/agent/v1"
	"github.com/amalychev/devopus/gen/devopus/agent/v1/agentv1connect"
	"github.com/amalychev/devopus/internal/agent/client"
	"github.com/amalychev/devopus/internal/agent/paths"
	"github.com/amalychev/devopus/internal/agent/proxy"
	"github.com/amalychev/devopus/internal/agent/runtime"
	"github.com/amalychev/devopus/internal/agent/store"
)

// The api module takes a second to start, like an app that loads before it listens.
const sleepConfig = `version: 1
project: %s
domain: %s.localhost
modules:
  web:
    path: web
    image: python:3.12-slim
    mount: /srv
    routes: [/]
    command: python -m http.server 8080 --directory /srv
  api:
    path: api
    image: python:3.12-slim
    mount: /srv
    routes: [/api]
    env:
      ENV_ID: ${{ env.id }}
    command: sh -c 'sleep 1; exec python /srv/app.py'
features:
  sleep_after: %s
%s`

const apiApp = `import http.server, os
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self): self.reply(("api " + os.environ["ENV_ID"]).encode())
    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        self.reply(b"got " + self.rfile.read(n))
    def reply(self, b):
        self.send_response(200)
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)
    def log_message(self, *a): pass
# The default listen backlog of 5 drops connections of a burst; real servers keep 511+.
http.server.ThreadingHTTPServer.request_queue_size = 128
http.server.ThreadingHTTPServer(("", 8080), H).serve_forever()
`

// TestFeatureSleep puts features to sleep and wakes them on requests (US-5, P-7).
func TestFeatureSleep(t *testing.T) {
	rt, err := runtime.New(runtime.Detect())
	if err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if _, err := rt.Info(ctx); err != nil {
		t.Skipf("no container runtime: %v", err)
	}
	userHome, _ := os.UserHomeDir()
	home, err := os.MkdirTemp(userHome, ".dk-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("DEVOPUS_HOME", home)

	project := "sl-" + store.NewID()
	root := filepath.Join(home, "repo")
	os.MkdirAll(filepath.Join(root, "web"), 0o755)
	os.MkdirAll(filepath.Join(root, "api"), 0o755)
	os.WriteFile(filepath.Join(root, "web", "index.html"), []byte("web"), 0o644)
	os.WriteFile(filepath.Join(root, "api", "app.py"), []byte(apiApp), 0o644)
	config := fmt.Appendf(nil, sleepConfig, project, project, "4s", "")
	os.WriteFile(filepath.Join(root, "workspace.yaml"), config, 0o644)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"-c", "commit.gpgsign=false", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	wsPath := filepath.Join(root, "workspace.yaml")

	logPath := filepath.Join(home, "agent.log")
	logFile, _ := os.Create(logPath)
	t.Cleanup(func() {
		logFile.Close()
		if t.Failed() {
			data, _ := os.ReadFile(logPath)
			t.Logf("agent log:\n%s", data)
		}
	})
	ports := proxy.Ports{HTTP: freePort(t), HTTPS: freePort(t)}
	done := make(chan error, 1)
	agentCtx, stop := context.WithCancel(context.Background())
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
	t.Cleanup(func() { purge(t, rt, c, project) })

	up, err := c.Up(ctx, connect.NewRequest(&agentv1.UpRequest{WorkspacePath: wsPath, Config: config}))
	if err != nil {
		t.Fatal(err)
	}
	baseID := up.Msg.EnvironmentId
	waitFor(t, ctx, c, baseID, func(s *agentv1.EnvironmentStatus) bool { return s.Phase == agentv1.Phase_PHASE_RUNNING })
	status, _ := c.Status(ctx, connect.NewRequest(&agentv1.StatusRequest{}))
	roots := x509.NewCertPool()
	pemData, _ := os.ReadFile(status.Msg.Ca.RootPath)
	roots.AppendCertsFromPEM(pemData)
	hc := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort("127.0.0.1", fmt.Sprint(ports.HTTPS)))
		},
		MaxIdleConnsPerHost: 32,
	}}
	call := func(method, host, path, accept, body string) (int, string, error) {
		req, _ := http.NewRequestWithContext(ctx, method, "https://"+host+path, strings.NewReader(body))
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		res, err := hc.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b), nil
	}

	f, err := c.CreateFeature(ctx, connect.NewRequest(&agentv1.CreateFeatureRequest{WorkspacePath: wsPath, Config: config, Name: "f1", Modules: []string{"api"}}))
	if err != nil {
		t.Fatal(err)
	}
	fID := f.Msg.EnvironmentId
	host := "f1." + project + ".localhost"
	waitFor(t, ctx, c, fID, func(s *agentv1.EnvironmentStatus) bool { return s.Phase == agentv1.Phase_PHASE_RUNNING })
	apiCtr := project + "-env-" + fID + "-api"

	// Idle for sleep_after: it sleeps, its container stops; the base never sleeps.
	start := time.Now()
	st := waitPhase(t, ctx, c, fID, agentv1.Phase_PHASE_SLEEPING)
	t.Logf("asleep %s after the last request (sleep_after 4s)", time.Since(start).Round(100*time.Millisecond))
	if st.SleepReason != "idle" || running(t, ctx, rt, apiCtr) {
		t.Errorf("asleep: reason %q, container running %v", st.SleepReason, running(t, ctx, rt, apiCtr))
	}

	// 20 requests at once wake it once; every one gets the module's answer.
	start = time.Now()
	var wg sync.WaitGroup
	var ok atomic.Int32
	var bad atomic.Value
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, body, err := call("GET", host, "/api/x", "", "")
			if err == nil && code == 200 && body == "api "+fID {
				ok.Add(1)
			} else {
				bad.Store(fmt.Sprintf("%d %q %v", code, body, err))
			}
		}()
	}
	wg.Wait()
	t.Logf("woken by 20 requests in %s", time.Since(start).Round(10*time.Millisecond))
	if ok.Load() != 20 {
		t.Errorf("%d of 20 requests to a sleeping feature succeeded; one failed with %v", ok.Load(), bad.Load())
	}
	waitPhase(t, ctx, c, fID, agentv1.Phase_PHASE_RUNNING)

	// A POST keeps its body; a browser sees the starting page of a slow wake.
	sleepNow(t, ctx, c, project, fID)
	if code, body, err := call("POST", host, "/api/form", "", "hello"); err != nil || code != 200 || body != "got hello" {
		t.Errorf("POST to a sleeping feature: %d %q %v", code, body, err)
	}
	sleepNow(t, ctx, c, project, fID)
	code, body, err := call("GET", host, "/api/", "text/html,*/*", "")
	if err != nil || code != 503 || !strings.Contains(body, "Starting f1") {
		t.Errorf("browser to a sleeping feature: %d %q %v", code, body, err)
		time.Sleep(11 * time.Second) // Envoy flushes its access log every 10 s
		out, _ := exec.Command("docker", "logs", "--tail", "8", paths.ProxyName()).CombinedOutput()
		t.Logf("proxy log:\n%s", out)
	}
	waitPhase(t, ctx, c, fID, agentv1.Phase_PHASE_RUNNING)
	if code, body, _ := call("GET", host, "/api/", "text/html,*/*", ""); code != 200 || body != "api "+fID {
		t.Errorf("browser after the wake: %d %q", code, body)
	}

	// No request is lost while the feature goes to sleep: the late ones wake it again.
	var sent, failed atomic.Int32
	var last atomic.Value
	pollCtx, stopPoll := context.WithCancel(ctx)
	var polling sync.WaitGroup
	polling.Add(1)
	go func() {
		defer polling.Done()
		for pollCtx.Err() == nil {
			code, body, err := call("GET", host, "/api/", "", "")
			if pollCtx.Err() != nil {
				return
			}
			sent.Add(1)
			if err != nil || code != 200 || body != "api "+fID {
				failed.Add(1)
				last.Store(fmt.Sprintf("%d %q %v", code, body, err))
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	for i := 0; i < 3; i++ {
		if _, err := c.SetSleep(ctx, connect.NewRequest(&agentv1.SetSleepRequest{Project: project, Feature: fID, Asleep: true})); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Second)
	}
	stopPoll()
	polling.Wait()
	if failed.Load() > 0 || sent.Load() < 20 {
		t.Errorf("%d of %d requests failed while the feature went to sleep; last: %v", failed.Load(), sent.Load(), last.Load())
	}

	// Budget: one feature of the project may be awake; the least recently used sleeps.
	budget := fmt.Appendf(nil, sleepConfig, project, project, "1h", "  max_running: 1\n")
	if _, err := c.AddFeatureModules(ctx, connect.NewRequest(&agentv1.AddFeatureModulesRequest{WorkspacePath: wsPath, Config: budget, Feature: "f1", Modules: []string{"api"}})); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, ctx, c, fID, agentv1.Phase_PHASE_RUNNING)
	// The proxy reports requests every few seconds: let f1's last ones arrive first.
	time.Sleep(proxy.StatsInterval + time.Second)
	g, err := c.CreateFeature(ctx, connect.NewRequest(&agentv1.CreateFeatureRequest{WorkspacePath: wsPath, Config: budget, Name: "f2", Modules: []string{"web"}}))
	if err != nil {
		t.Fatal(err)
	}
	gID := g.Msg.EnvironmentId
	waitFor(t, ctx, c, gID, func(s *agentv1.EnvironmentStatus) bool {
		return s.Phase == agentv1.Phase_PHASE_RUNNING || s.Phase == agentv1.Phase_PHASE_SLEEPING
	})
	if st := waitPhase(t, ctx, c, fID, agentv1.Phase_PHASE_SLEEPING); st.SleepReason != "budget" {
		t.Errorf("f1 sleeps for %q, want budget", st.SleepReason)
	}
	if _, body, _ := call("GET", host, "/api/", "", ""); body != "api "+fID {
		t.Errorf("f1 after waking over the budget: %q", body)
	}
	if st := waitPhase(t, ctx, c, gID, agentv1.Phase_PHASE_SLEEPING); st.SleepReason != "budget" {
		t.Errorf("f2 sleeps for %q, want budget", st.SleepReason)
	}
	if st := waitPhase(t, ctx, c, baseID, agentv1.Phase_PHASE_RUNNING); st.Phase != agentv1.Phase_PHASE_RUNNING {
		t.Error("the base environment must never sleep")
	}
}

func sleepNow(t *testing.T, ctx context.Context, c agentv1connect.AgentServiceClient, project, envID string) {
	t.Helper()
	if _, err := c.SetSleep(ctx, connect.NewRequest(&agentv1.SetSleepRequest{Project: project, Feature: envID, Asleep: true})); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, ctx, c, envID, agentv1.Phase_PHASE_SLEEPING)
}

// waitPhase polls the status: phases of sleep and wake change outside passes.
func waitPhase(t *testing.T, ctx context.Context, c agentv1connect.AgentServiceClient, envID string, phase agentv1.Phase) *agentv1.EnvironmentStatus {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		st, err := c.Status(ctx, connect.NewRequest(&agentv1.StatusRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range st.Msg.Environments {
			if e.Id == envID && e.Phase == phase {
				return e
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not reach %v: %v", envID, phase, st.Msg.Environments)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
