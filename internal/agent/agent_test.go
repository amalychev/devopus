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
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/docklet/docklet/gen/docklet/agent/v1"
	"github.com/docklet/docklet/gen/docklet/agent/v1/agentv1connect"
	"github.com/docklet/docklet/internal/agent/client"
	"github.com/docklet/docklet/internal/agent/paths"
	"github.com/docklet/docklet/internal/agent/proxy"
	"github.com/docklet/docklet/internal/agent/runtime"
	"github.com/docklet/docklet/internal/agent/store"
)

// TestAgentEndToEnd runs the agent against the real runtime: up creates the
// environment network, purge removes it. Skipped when no runtime answers.
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
	t.Setenv("DOCKLET_HOME", home)

	project := "e2e-" + store.NewID()
	// Free high ports: a real agent may hold 80/443 or the fallback ports.
	ports := proxy.Ports{HTTP: freePort(t), HTTPS: freePort(t)}
	done := make(chan error, 1)
	agentCtx, stop := context.WithCancel(ctx)
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

	config := fmt.Appendf(nil, "version: 1\nproject: %s\ndomain: %s.localhost\nmodules:\n  web:\n    repo: git@example.com:web.git\n    image: nginx:1.27\n    routes: [/]\n", project, project)
	invalid, err := c.Up(ctx, connect.NewRequest(&agentv1.UpRequest{WorkspacePath: "/src/x/workspace.yaml", Config: []byte("version: 1\n")}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("invalid config accepted: %v %v", invalid, err)
	}

	up, err := c.Up(ctx, connect.NewRequest(&agentv1.UpRequest{WorkspacePath: "/src/" + project + "/workspace.yaml", Config: config}))
	if err != nil {
		t.Fatal(err)
	}
	envID := up.Msg.EnvironmentId
	t.Cleanup(func() {
		// leave nothing behind even if the test fails half-way
		_, _ = c.Down(context.Background(), connect.NewRequest(&agentv1.DownRequest{Project: project, Purge: true}))
		time.Sleep(time.Second)
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
		if code, body := httpsGet(t, ctx, addr, ports.HTTPS, host, roots); code != 200 || !strings.Contains(body, project+" is up") {
			t.Errorf("%s: %d %s", addr, code, body)
		}
	}
	routes, err := c.Routes(ctx, connect.NewRequest(&agentv1.RoutesRequest{EnvironmentId: envID}))
	if err != nil || len(routes.Msg.Routes) != 1 || routes.Msg.Routes[0].Host != host {
		t.Errorf("routes: %v %v", routes, err)
	}

	// Inside the environment network every *.localhost name is the proxy (CoreDNS).
	proxyIP := strings.TrimSuffix(st.Network.Subnet, "0/24") + "254"
	dnsIP := strings.TrimSuffix(st.Network.Subnet, "0/24") + "253"
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", st.Network.Name, "--dns", dnsIP, "python:3.12-slim",
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

	if _, err := c.Down(ctx, connect.NewRequest(&agentv1.DownRequest{Project: project, Purge: true})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, c, envID, func(s *agentv1.EnvironmentStatus) bool { return s.Removed })
	if hasNetwork(t, ctx, rt, envID) {
		t.Fatal("network left behind")
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
	addr := net.JoinHostPort(ip, fmt.Sprint(port))
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: host},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/", nil)
	res, err := c.Do(req)
	if err != nil {
		t.Errorf("GET https://%s via %s: %v", host, addr, err)
		return 0, ""
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
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
