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
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentv2 "github.com/amalychev/devopus/gen/devopus/agent/v2"
	syncv1 "github.com/amalychev/devopus/gen/devopus/sync/v1"
	"github.com/amalychev/devopus/gen/devopus/sync/v1/syncv1connect"
	workspacev1 "github.com/amalychev/devopus/gen/devopus/workspace/v1"
	"github.com/amalychev/devopus/gen/devopus/workspace/v1/workspacev1connect"
	"github.com/amalychev/devopus/internal/agent/client"
	"github.com/amalychev/devopus/internal/agent/link"
	"github.com/amalychev/devopus/internal/agent/paths"
	"github.com/amalychev/devopus/internal/agent/proxy"
	"github.com/amalychev/devopus/internal/agent/runtime"
	"github.com/amalychev/devopus/internal/agent/store"
	"github.com/amalychev/devopus/internal/auth/authtest"
	"github.com/amalychev/devopus/internal/auth/token"
	"github.com/amalychev/devopus/internal/credentials"
	"github.com/amalychev/devopus/internal/pgtest"
	"github.com/amalychev/devopus/internal/server"
	serverstore "github.com/amalychev/devopus/internal/server/store"
	"github.com/amalychev/devopus/internal/signing"
)

const serverProject = `apps:
  web:
    repo: %s
    routes: [/]
    env:
      VERSION: "%s"
    command: sh -c "mkdir -p /srv && echo server settings v$VERSION > /srv/index.html && cd /srv && exec python -m http.server 8080"
services:
  py:
    type: python
    version: 3.12-slim
    apps: [web]
`

// TestServerAgent is R13's scenario: the admin adds a server; its agent joins with
// the one-time code and runs the project — only the versions the admin signed.
func TestServerAgent(t *testing.T) {
	rt, err := runtime.New(runtime.Detect())
	if err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := rt.Info(ctx); err != nil {
		t.Skipf("no container runtime: %v", err)
	}
	home, err := os.MkdirTemp("/tmp", "ds-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("DEVOPUS_HOME", home)
	t.Setenv("DEVOPUS_CREDENTIAL_STORE", credentials.StoreFile)

	a := authtest.Start(t)
	pool := pgtest.Pool(t)
	if err := serverstore.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	keys := &token.Remote{URL: a.URL + "/.well-known/jwks.json"}
	srv := httptest.NewUnstartedServer(nil)
	srv.Start()
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	svc, err := server.New(pool, server.Config{Keys: keys.Key, AuthIssuer: a.URL, Mailer: a.Inbox, PublicURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = svc.Handler()
	ws := workspacev1connect.NewWorkspaceServiceClient(http.DefaultClient, srv.URL)

	userHome, _ := os.UserHomeDir()
	repo, err := os.MkdirTemp(userHome, ".devopus-e2e-repo-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(repo) })
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	config := func(version string) string { return fmt.Sprintf(serverProject, "file://"+repo, version) }

	// The admin: a workspace, a project, and their device as the root of trust.
	project := "s13-" + store.NewID()
	admin := forServer(t, a, signUp(t, a, "admin@acme.dev"), srv.URL)
	w, err := ws.CreateWorkspace(ctx, bearer(&workspacev1.CreateWorkspaceRequest{Name: "acme"}, admin))
	if err != nil {
		t.Fatal(err)
	}
	wsID := w.Msg.Workspace.Id
	key, _ := signing.NewKey()
	if _, err := ws.RegisterSigningKey(ctx, bearer(&workspacev1.RegisterSigningKeyRequest{WorkspaceId: wsID, DeviceId: "admin-mac", DeviceName: "mac", PublicKey: key.Public()}, admin)); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.SetTrustRoot(ctx, bearer(&workspacev1.SetTrustRootRequest{WorkspaceId: wsID, DeviceId: "admin-mac"}, admin)); err != nil {
		t.Fatal(err)
	}
	created, err := ws.CreateProject(ctx, bearer(&workspacev1.CreateProjectRequest{WorkspaceId: wsID, Name: project, ConfigYaml: config("1")}, admin))
	if err != nil {
		t.Fatal(err)
	}
	sign := func(conf *workspacev1.ProjectConfig) {
		t.Helper()
		if _, err := ws.SignVersion(ctx, bearer(&workspacev1.SignVersionRequest{WorkspaceId: wsID, Project: project, Version: conf.Version, DeviceId: "admin-mac",
			Signature: key.Sign(signing.SettingsMessage(wsID, project, conf.Version, []byte(conf.Yaml)))}, admin)); err != nil {
			t.Fatal(err)
		}
	}

	// The server joins with the code; its agent starts with the link it got.
	added, err := ws.CreateServer(ctx, bearer(&workspacev1.CreateServerRequest{WorkspaceId: wsID, Name: "stage", Projects: []string{project}}, admin))
	if err != nil {
		t.Fatal(err)
	}
	joined, err := ws.JoinServer(ctx, connect.NewRequest(&workspacev1.JoinServerRequest{Code: added.Msg.JoinCode, DeviceId: "stage-1", DeviceName: "stage-1"}))
	if err != nil {
		t.Fatal(err)
	}
	j := joined.Msg
	if err := link.SaveServerConfig(&link.ServerConfig{URL: srv.URL, WorkspaceID: j.WorkspaceId, WorkspaceName: j.WorkspaceName, ServerID: j.ServerId, Name: j.Name, Token: j.Token}); err != nil {
		t.Fatal(err)
	}
	ports := proxy.Ports{HTTP: freePort(t), HTTPS: freePort(t)}
	done := make(chan error, 1)
	agentCtx, stop := context.WithCancel(context.Background())
	acmeDir := pebble(t)
	go func() {
		done <- Run(agentCtx, Options{Version: "test", Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ProxyPorts: &ports,
			ACMEDirectory: acmeDir, ACMEClient: insecureClient()})
	}()
	t.Cleanup(func() {
		stop()
		<-done
	})
	c := client.New(paths.Socket())
	t.Cleanup(func() { purge(t, rt, c, project) })
	status := func() *agentv2.StatusResponse {
		t.Helper()
		for i := 0; ; i++ {
			st, err := c.Status(ctx, connect.NewRequest(&agentv2.StatusRequest{}))
			if err == nil {
				return st.Msg
			}
			if i > 100 {
				t.Fatalf("agent: %v", err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	// Unsigned, the first version is not taken at all.
	for deadline := time.Now().Add(30 * time.Second); !status().Link.Connected; time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the server's agent did not connect: %v", status().Link)
		}
	}
	time.Sleep(2 * time.Second)
	if ps := status().Projects; len(ps) != 0 {
		t.Fatalf("an unsigned version is taken: %v", ps)
	}

	// Signed by the admin: it runs.
	sign(created.Msg.Config)
	var url string
	for deadline := time.Now().Add(3 * time.Minute); url == ""; time.Sleep(300 * time.Millisecond) {
		for _, e := range status().Environments {
			if e.Project == project && e.Phase == agentv2.Phase_PHASE_RUNNING {
				url = e.Url
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("environment: %v", status().Environments)
		}
	}
	roots := x509.NewCertPool()
	pemData, err := os.ReadFile(status().Ca.RootPath)
	if err != nil {
		t.Fatal(err)
	}
	roots.AppendCertsFromPEM(pemData)
	host := strings.TrimPrefix(strings.Split(url, ":")[1], "//")
	serves := func(want string) {
		t.Helper()
		var body string
		for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
			if _, b, err := httpsTry(ctx, "127.0.0.1", ports.HTTPS, host, "/", roots); err == nil {
				if body = b; strings.Contains(b, want) {
					return
				}
			}
		}
		t.Fatalf("%s does not serve %q: %q", host, want, body)
	}
	serves("server settings v1")

	// An unsigned change — even an everyday one — waits; signed, it applies.
	put, err := ws.PutApp(ctx, bearer(&workspacev1.PutAppRequest{WorkspaceId: wsID, Project: project, Name: "web",
		AppYaml: strings.SplitN(strings.SplitN(config("2"), "web:\n", 2)[1], "services:", 2)[0]}, admin))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(300 * time.Millisecond) {
		if p := status().Projects; len(p) == 1 && p[0].Pending != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the unsigned change did not wait: %v", status().Projects)
		}
	}
	serves("server settings v1")
	sign(put.Msg.Config)
	serves("server settings v2")

	// People see what the server runs.
	sc := syncv1connect.NewSyncServiceClient(http.DefaultClient, srv.URL)
	for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		res, err := sc.ListDevices(ctx, bearer(&syncv1.ListDevicesRequest{WorkspaceId: wsID, Project: project}, admin))
		if err != nil {
			t.Fatal(err)
		}
		if d := res.Msg.Devices; len(d) == 1 && d[0].UserEmail == "stage (server)" && len(d[0].Environments) == 1 && d[0].Environments[0].ConfigVersion == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("devices: %v", res.Msg.Devices)
		}
	}

	// R14: the admin signs a staging environment at a public domain; the server's agent
	// moves the project there and gets its certificate from the CA.
	domain := strings.ToLower(project) + ".staging.example.com"
	if _, err := ws.CreateEnvironment(ctx, bearer(&workspacev1.CreateEnvironmentRequest{WorkspaceId: wsID, Project: project, Server: "stage", Kind: "staging",
		Domain: domain, DeviceId: "admin-mac", Signature: key.Sign(signing.EnvironmentMessage(wsID, j.ServerId, project, "staging", domain))}, admin)); err != nil {
		t.Fatal(err)
	}
	var issuer string
	for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(500 * time.Millisecond) {
		body, cert, err := publicGet(ctx, ports.HTTPS, domain)
		if err == nil && strings.Contains(body, "server settings v2") && cert != nil && !strings.Contains(cert.Issuer.String(), "Devopus") {
			issuer = cert.Issuer.String()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is not served with the CA's certificate: %v %v %q", domain, cert, err, body)
		}
	}
	if !strings.Contains(strings.ToLower(issuer), "pebble") {
		t.Errorf("issuer: %s", issuer)
	}
	for _, p := range status().Projects {
		if p.Name == project && (p.PublicDomain != domain || p.EnvironmentSignedBy != "admin@acme.dev") {
			t.Errorf("project on the server: %+v", p)
		}
	}

	// R14b: the server becomes the project's production. It runs nothing until a
	// deploy, then exactly the deployed version — v1 here, an older one than staging's.
	if _, err := ws.RemoveEnvironment(ctx, bearer(&workspacev1.RemoveEnvironmentRequest{WorkspaceId: wsID, Project: project, Server: "stage"}, admin)); err != nil {
		t.Fatal(err)
	}
	prod := "prod-" + domain
	if _, err := ws.CreateEnvironment(ctx, bearer(&workspacev1.CreateEnvironmentRequest{WorkspaceId: wsID, Project: project, Server: "stage", Kind: "production",
		Domain: prod, DeviceId: "admin-mac", Signature: key.Sign(signing.EnvironmentMessage(wsID, j.ServerId, project, "production", prod))}, admin)); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(300 * time.Millisecond) {
		if p := status().Projects; len(p) == 1 && p[0].Orphaned {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("production ran before a deploy: %v", status().Projects)
		}
	}
	deploy := func(conf *workspacev1.ProjectConfig) {
		t.Helper()
		if _, err := ws.DeployProduction(ctx, bearer(&workspacev1.DeployProductionRequest{WorkspaceId: wsID, Project: project, Server: "stage", Version: conf.Version,
			DeviceId: "admin-mac", Signature: key.Sign(signing.DeployMessage(wsID, j.ServerId, project, conf.Version, []byte(conf.Yaml)))}, admin)); err != nil {
			t.Fatal(err)
		}
	}
	servesAt := func(host, want string) {
		t.Helper()
		var body string
		for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(500 * time.Millisecond) {
			b, cert, err := publicGet(ctx, ports.HTTPS, host)
			if err == nil && strings.Contains(b, want) && cert != nil && !strings.Contains(cert.Issuer.String(), "Devopus") {
				return
			}
			body = b
			if time.Now().After(deadline) {
				t.Fatalf("%s does not serve %q: %q %v", host, want, body, err)
			}
		}
	}
	deploy(created.Msg.Config)
	servesAt(prod, "server settings v1")
	deploy(put.Msg.Config)
	servesAt(prod, "server settings v2")
	for _, p := range status().Projects {
		if p.Name == project && (p.DeployedBy != "admin@acme.dev" || p.ConfigVersion != put.Msg.Config.Version) {
			t.Errorf("production on the server: %+v", p)
		}
	}
}

// publicGet fetches a public host from the proxy on the loopback and returns the
// certificate it showed.
func publicGet(ctx context.Context, port uint16, host string) (string, *x509.Certificate, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{ServerName: host, InsecureSkipVerify: true},
		DialContext:     func(ctx context.Context, network, _ string) (net.Conn, error) { return (&net.Dialer{}).DialContext(ctx, network, addr) },
	}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/", nil)
	res, err := c.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	var cert *x509.Certificate
	if res.TLS != nil && len(res.TLS.PeerCertificates) > 0 {
		cert = res.TLS.PeerCertificates[0]
	}
	return string(b), cert, nil
}

func insecureClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
}

// pebble starts Let's Encrypt's test CA, which validates nothing: the challenge's way
// through the proxy is checked in the proxy's tests.
func pebble(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("docker", "run", "-d", "--rm", "-e", "PEBBLE_VA_ALWAYS_VALID=1", "-e", "PEBBLE_VA_NOSLEEP=1",
		"-p", "127.0.0.1::14000", "ghcr.io/letsencrypt/pebble:latest").CombinedOutput()
	if err != nil {
		t.Skipf("pebble: %s", out)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
	port, err := exec.Command("docker", "port", id, "14000").Output()
	if err != nil {
		t.Fatal(err)
	}
	dir := "https://" + strings.TrimSpace(strings.Split(string(port), "\n")[0]) + "/dir"
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if r, err := insecureClient().Get(dir); err == nil {
			r.Body.Close()
			return dir
		}
		if time.Now().After(deadline) {
			t.Fatal("pebble did not start")
		}
	}
}
