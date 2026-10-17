package agent

import (
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
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
	go func() {
		done <- Run(agentCtx, Options{Version: "test", Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ProxyPorts: &ports})
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
}
