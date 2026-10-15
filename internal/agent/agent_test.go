package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/devopus/devopus/gen/devopus/agent/v1"
	"github.com/devopus/devopus/gen/devopus/agent/v1/agentv1connect"
	"github.com/devopus/devopus/internal/agent/client"
	"github.com/devopus/devopus/internal/agent/paths"
	"github.com/devopus/devopus/internal/agent/proxy"
	"github.com/devopus/devopus/internal/agent/runtime"
	"github.com/devopus/devopus/internal/agent/store"
)

// TestAgentEndToEnd runs the agent against the real runtime: up brings a module and a
// service up behind the proxy, down stops them, purge removes everything. Skipped when
// no runtime answers.
func TestAgentEndToEnd(t *testing.T) {
	rt, err := runtime.New(runtime.Detect())
	if err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if _, err := rt.Info(ctx); err != nil {
		t.Skipf("no container runtime: %v", err)
	}

	// unix socket paths are limited to ~104 bytes: t.TempDir() on macOS is too long.
	home, err := os.MkdirTemp("/tmp", "dk-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("DEVOPUS_HOME", home)

	project := "e2e-" + store.NewID()
	// Free high ports: a real agent may hold 80/443 or the fallback ports.
	ports := proxy.Ports{HTTP: freePort(t), HTTPS: freePort(t)}
	done := make(chan error, 1)
	// Not the test's deadline: the agent must outlive it to clean up after a failure.
	agentCtx, stop := context.WithCancel(context.Background())
	go func() {
		done <- Run(agentCtx, Options{Version: "test", Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ProxyPorts: &ports})
	}()
	t.Cleanup(func() {
		stop()
		<-done
	})

	c := client.New(paths.Socket())
	for i := 0; ; i++ {
		if err := client.Ping(ctx, c); err == nil {
			break
		} else if i > 100 {
			t.Fatalf("agent did not start: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// A second agent on the same data directory refuses to start.
	if err := Run(ctx, Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("second agent: %v", err)
	}

	// A module that serves its folder, with an install hook that writes into it, and a
	// service with a web UI on its own host. The image is small and usually at hand.
	// Under the home folder: Colima shares only it with its VM.
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(userHome, ".devopus-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if err := os.MkdirAll(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "web", "index.html"), []byte("served by the module "+project), 0o644)
	// A module built from its Dockerfile, on a path of the same host.
	os.MkdirAll(filepath.Join(root, "api"), 0o755)
	os.WriteFile(filepath.Join(root, "api", "Dockerfile"), []byte("FROM python:3.12-slim\nRUN mkdir -p /srv/api && echo built by docker > /srv/api/index.html\nWORKDIR /srv\n"), 0o644)
	config := fmt.Appendf(nil, `version: 1
project: %s
domain: %s.localhost
services:
  ui:
    image: python:3.12-slim
    command: python -m http.server 8025
    port: 8025
    expose: ui
modules:
  web:
    path: web
    image: python:3.12-slim
    mount: /app
    routes: [/]
    command: python -m http.server 8080
    hooks:
      install: echo installed > .installed
  api:
    path: api
    build: {context: api}
    routes: [/api]
    command: python -m http.server 8080
`, project, project)
	invalid, err := c.Up(ctx, connect.NewRequest(&agentv1.UpRequest{WorkspacePath: "/src/x/workspace.yaml", Config: []byte("version: 1\n")}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("invalid config accepted: %v %v", invalid, err)
	}

	up, err := c.Up(ctx, connect.NewRequest(&agentv1.UpRequest{WorkspacePath: filepath.Join(root, "workspace.yaml"), Config: config}))
	if err != nil {
		t.Fatal(err)
	}
	envID := up.Msg.EnvironmentId
	t.Cleanup(func() {
		// leave nothing behind even if the test fails half-way
		purge(t, rt, c, project)
	})
	st := waitFor(t, ctx, c, envID, func(s *agentv1.EnvironmentStatus) bool { return s.Phase == agentv1.Phase_PHASE_RUNNING })
	if st.Network.GetSubnet() == "" {
		t.Fatalf("no network in %v", st)
	}
	if !hasNetwork(t, ctx, rt, envID) {
		t.Fatal("network not created")
	}
	host := project + ".localhost"
	if want := fmt.Sprintf("https://%s:%d", host, ports.HTTPS); st.Url != want {
		t.Errorf("url = %s, want %s", st.Url, want)
	}

	// The proxy serves the environment over HTTPS with the local CA, on both loopbacks.
	status, err := c.Status(ctx, connect.NewRequest(&agentv1.StatusRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if p := status.Msg.Proxy; p.State != "running" || !p.Connected || p.HttpsPort != uint32(ports.HTTPS) {
		t.Errorf("proxy = %v", p)
	}
	roots := x509.NewCertPool()
	pemData, err := os.ReadFile(status.Msg.Ca.RootPath)
	if err != nil {
		t.Fatal(err)
	}
	roots.AppendCertsFromPEM(pemData)
	for _, addr := range []string{"127.0.0.1", "::1"} {
		if code, body := httpsGet(t, ctx, addr, ports.HTTPS, host, roots); code != 200 || !strings.Contains(body, "served by the module "+project) {
			t.Errorf("%s: %d %s", addr, code, body)
		}
	}
	if code, body := httpsGet(t, ctx, "127.0.0.1", ports.HTTPS, "ui."+host, roots); code != 200 || !strings.Contains(body, "Directory listing") {
		t.Errorf("ui.%s: %d %s", host, code, body)
	}
	if code, body := httpsGetPath(t, ctx, "127.0.0.1", ports.HTTPS, host, "/api/", roots); code != 200 || !strings.Contains(body, "built by docker") {
		t.Errorf("%s/api/: %d %s", host, code, body)
	}
	routes, err := c.Routes(ctx, connect.NewRequest(&agentv1.RoutesRequest{EnvironmentId: envID}))
	if err != nil || len(routes.Msg.Routes) != 3 || routes.Msg.Routes[0].Host != host || !strings.HasPrefix(routes.Msg.Routes[0].Target, "module web") {
		t.Errorf("routes: %v %v", routes, err)
	}
	// The install hook ran in the module's container, in its code folder.
	if data, err := os.ReadFile(filepath.Join(root, "web", ".installed")); err != nil || strings.TrimSpace(string(data)) != "installed" {
		t.Errorf("install hook: %q %v", data, err)
	}
	if len(st.Units) != 3 || st.Units[0].Name != "ui" || st.Units[1].State != "running" {
		t.Errorf("units: %v", st.Units)
	}

	// A module calls its environment's public URL: DNS gives the proxy, the proxy listens
	// on the public port inside the network too, the module trusts the local CA.
	web := project + "-env-" + envID + "-web"
	out, err := exec.CommandContext(ctx, "docker", "exec", web, "python", "-c",
		"import urllib.request;print(urllib.request.urlopen('"+st.Url+"/').read().decode())").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "served by the module") {
		t.Errorf("the module calling %s: %s %v", st.Url, out, err)
	}

	// Inside the environment network every *.localhost name is the proxy (CoreDNS).
	proxyIP := strings.TrimSuffix(st.Network.Subnet, "0/24") + "254"
	dnsIP := strings.TrimSuffix(st.Network.Subnet, "0/24") + "253"
	out, err = exec.CommandContext(ctx, "docker", "run", "--rm", "--network", st.Network.Name, "--dns", dnsIP, "python:3.12-slim",
		"python", "-c", "import socket;print(socket.gethostbyname('api."+host+"'))").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != proxyIP {
		t.Errorf("api.%s inside the network: %s %v (want %s)", host, out, err, proxyIP)
	}

	// down stops the environment: the site says so, the DNS server goes, the network stays.
	down, err := c.Down(ctx, connect.NewRequest(&agentv1.DownRequest{Project: project}))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, c, envID, func(s *agentv1.EnvironmentStatus) bool {
		return s.Phase == agentv1.Phase_PHASE_STOPPED && s.AppliedRevision >= down.Msg.Revision
	})
	if code, _ := httpsGet(t, ctx, "127.0.0.1", ports.HTTPS, host, roots); code != 503 {
		t.Errorf("stopped environment answers %d", code)
	}
	if !hasNetwork(t, ctx, rt, envID) || len(containers(t, ctx, rt, "dns")) != 0 {
		t.Error("stop must keep the network and drop the DNS server")
	}
	if names := containers(t, ctx, rt, "module"); len(names) != 2 || running(t, ctx, rt, names[0]) {
		t.Errorf("stop must keep the module container, stopped: %v", names)
	}

	if _, err := c.Down(ctx, connect.NewRequest(&agentv1.DownRequest{Project: project, Purge: true})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, c, envID, func(s *agentv1.EnvironmentStatus) bool { return s.Removed })
	if hasNetwork(t, ctx, rt, envID) {
		t.Fatal("network left behind")
	}
	// So are the images built for the project.
	for i := 0; ; i++ {
		imgs, err := rt.Images(ctx, paths.Instance())
		if err != nil {
			t.Fatal(err)
		}
		if len(imgs) == 0 {
			break
		}
		if i > 50 {
			t.Fatalf("images left behind: %v", imgs)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// The last environment is gone, so is the proxy.
	for i := 0; len(containers(t, ctx, rt, "")) > 0; i++ {
		if i > 50 {
			t.Fatalf("containers left behind: %v", containers(t, ctx, rt, ""))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func httpsGet(t *testing.T, ctx context.Context, ip string, port uint16, host string, roots *x509.CertPool) (int, string) {
	t.Helper()
	return httpsGetPath(t, ctx, ip, port, host, "/", roots)
}

func httpsGetPath(t *testing.T, ctx context.Context, ip string, port uint16, host, path string, roots *x509.CertPool) (int, string) {
	t.Helper()
	code, body, err := httpsTry(ctx, ip, port, host, path, roots)
	if err != nil {
		t.Errorf("GET https://%s%s via %s: %v", host, path, ip, err)
	}
	return code, body
}

func httpsTry(ctx context.Context, ip string, port uint16, host, path string, roots *x509.CertPool) (int, string, error) {
	addr := net.JoinHostPort(ip, fmt.Sprint(port))
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: host},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+path, nil)
	res, err := c.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b), nil
}

func running(t *testing.T, ctx context.Context, rt *runtime.Docker, name string) bool {
	t.Helper()
	all, err := rt.Containers(ctx, paths.Instance())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		if c.Name == name {
			return c.Running
		}
	}
	return false
}

func containers(t *testing.T, ctx context.Context, rt *runtime.Docker, role string) []string {
	t.Helper()
	all, err := rt.Containers(ctx, paths.Instance())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range all {
		if role == "" || c.Labels[runtime.LabelRole] == role {
			out = append(out, c.Name)
		}
	}
	return out
}

// purge removes the project and waits until the agent's containers are gone.
func purge(t *testing.T, rt *runtime.Docker, c agentv1connect.AgentServiceClient, project string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, _ = c.Down(ctx, connect.NewRequest(&agentv1.DownRequest{Project: project, Purge: true}))
	for ctx.Err() == nil {
		if all, err := rt.Containers(ctx, paths.Instance()); err != nil || len(all) == 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("containers of %s are still there after a minute", project)
}

func freePort(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return uint16(ln.Addr().(*net.TCPAddr).Port)
}

func waitFor(t *testing.T, ctx context.Context, c agentv1connect.AgentServiceClient, envID string, cond func(*agentv1.EnvironmentStatus) bool) *agentv1.EnvironmentStatus {
	t.Helper()
	w, err := c.Watch(ctx, connect.NewRequest(&agentv1.WatchRequest{EnvironmentId: envID}))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for w.Receive() {
		if s := w.Msg().Status; s != nil {
			if s.Phase == agentv1.Phase_PHASE_ERROR {
				t.Fatalf("environment failed: %s", s.Error)
			}
			if cond(s) {
				return s
			}
		}
	}
	t.Fatalf("watch ended: %v", w.Err())
	return nil
}

func hasNetwork(t *testing.T, ctx context.Context, rt *runtime.Docker, envID string) bool {
	t.Helper()
	nets, err := rt.Networks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nets {
		if n.Labels[runtime.LabelEnv] == envID {
			return true
		}
	}
	return false
}
