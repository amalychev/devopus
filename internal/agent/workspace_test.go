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
	authv1 "github.com/amalychev/devopus/gen/devopus/auth/v1"
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
	"github.com/amalychev/devopus/internal/secrets"
	"github.com/amalychev/devopus/internal/server"
	serverstore "github.com/amalychev/devopus/internal/server/store"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

// signUp registers an account on Auth and returns its first sign-in.
func signUp(t *testing.T, a *authtest.Server, email string) *authv1.CompleteEmailSignInResponse {
	t.Helper()
	ctx := context.Background()
	c := credentials.Client(a.URL)
	st, err := c.StartEmailSignIn(ctx, connect.NewRequest(&authv1.StartEmailSignInRequest{Email: email, Purpose: authv1.Purpose_PURPOSE_REGISTER}))
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.CompleteEmailSignIn(ctx, connect.NewRequest(&authv1.CompleteEmailSignInRequest{
		ChallengeId: st.Msg.ChallengeId, Code: a.Inbox.Code(t, email, a.Inbox.Count(email)), Plan: authv1.Plan_PLAN_FREE, DeviceName: "test",
	}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg
}

func bearer[T any](msg *T, access string) *connect.Request[T] {
	r := connect.NewRequest(msg)
	r.Header().Set("Authorization", "Bearer "+access)
	return r
}

const workspaceProject = `apps:
  web:
    repo: %s
    image: python:3.12-slim
    mount: /app
    routes: [/]
    env:
      VERSION: "%s"
      PW: ${{ secrets.DB_PASSWORD }}
    command: sh -c "mkdir -p /srv && echo settings v$VERSION pw $PW > /srv/index.html && cd /srv && exec python -m http.server 8080"
`

// TestAgentFromWorkspace is R7's scenario: a developer's agent gets the project of the
// workspace from its server and brings it up with nothing else to do; the teamlead's
// change reaches it on its own; without the server the environment keeps running.
func TestAgentFromWorkspace(t *testing.T) {
	rt, err := runtime.New(runtime.Detect())
	if err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := rt.Info(ctx); err != nil {
		t.Skipf("no container runtime: %v", err)
	}
	home, err := os.MkdirTemp("/tmp", "dw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("DEVOPUS_HOME", home)
	t.Setenv("DEVOPUS_CREDENTIAL_STORE", credentials.StoreFile)

	// Auth and the workspace server.
	a := authtest.Start(t)
	pool := pgtest.Pool(t)
	if err := serverstore.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	keys := &token.Remote{URL: a.URL + "/.well-known/jwks.json"}
	svc, err := server.New(pool, server.Config{Keys: keys.Key, AuthIssuer: a.URL, Mailer: a.Inbox})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(svc.Handler())
	serverUp := true
	t.Cleanup(func() {
		if serverUp {
			srv.CloseClientConnections()
			srv.Close()
		}
	})
	ws := workspacev1connect.NewWorkspaceServiceClient(http.DefaultClient, srv.URL)

	// The application's repository; the agent clones it with the developer's git.
	// Under the home folder: Colima shares only it with its VM.
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
	appYAML := func(version string) string { return fmt.Sprintf(workspaceProject, "file://"+repo, version) }

	// The teamlead sets up the workspace elsewhere and invites the developer.
	project := "w7-" + store.NewID()
	lead := signUp(t, a, "lead@acme.dev").Tokens.AccessToken
	w, err := ws.CreateWorkspace(ctx, bearer(&workspacev1.CreateWorkspaceRequest{Name: "acme"}, lead))
	if err != nil {
		t.Fatal(err)
	}
	wsID := w.Msg.Workspace.Id
	if _, err := ws.CreateProject(ctx, bearer(&workspacev1.CreateProjectRequest{WorkspaceId: wsID, Name: project, ConfigYaml: appYAML("1")}, lead)); err != nil {
		t.Fatal(err)
	}
	// The teamlead (the admin here) sets the database password: the key of the
	// workspace is made on their device, the value is encrypted there.
	leadDevice, _ := secrets.NewKey()
	if _, err := ws.RegisterDeviceKey(ctx, bearer(&workspacev1.RegisterDeviceKeyRequest{WorkspaceId: wsID, DeviceId: "lead-mac", DeviceName: "mac", PublicKey: leadDevice.Recipient().String()}, lead)); err != nil {
		t.Fatal(err)
	}
	wsKey, _ := secrets.NewKey()
	leadWrap, _ := secrets.Wrap(wsKey, leadDevice.Recipient().String())
	if _, err := ws.CreateSecretsKey(ctx, bearer(&workspacev1.CreateSecretsKeyRequest{WorkspaceId: wsID, PublicKey: wsKey.Recipient().String(), DeviceId: "lead-mac", Wrapped: leadWrap}, lead)); err != nil {
		t.Fatal(err)
	}
	const password = "correct-horse-battery"
	sealed, _ := secrets.Seal([]byte(password), wsKey.Recipient().String())
	if _, err := ws.SetSecret(ctx, bearer(&workspacev1.SetSecretRequest{WorkspaceId: wsID, Project: project, Name: "DB_PASSWORD", Generation: 1, Ciphertext: sealed}, lead)); err != nil {
		t.Fatal(err)
	}
	inv, err := ws.CreateInvite(ctx, bearer(&workspacev1.CreateInviteRequest{WorkspaceId: wsID, Email: "dev@acme.dev", Role: "developer", Projects: []string{project}}, lead))
	if err != nil {
		t.Fatal(err)
	}

	// The developer signs in on this machine and accepts: that is all they do.
	dev := signUp(t, a, "dev@acme.dev")
	if _, err := ws.AcceptInvite(ctx, bearer(&workspacev1.AcceptInviteRequest{Code: inv.Msg.Code}, dev.Tokens.AccessToken)); err != nil {
		t.Fatal(err)
	}
	if err := credentials.Save(credentials.FromTokens(a.URL, dev.User, dev.Tokens), dev.Tokens.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if err := credentials.SetCurrentWorkspace(&credentials.Workspace{ID: wsID, Name: "acme", ServerURL: srv.URL}); err != nil {
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

	// The agent gets the project and brings its base environment up by itself.
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
	envID := ""
	var spec *agentv2.ProjectSpec
	for deadline := time.Now().Add(time.Minute); envID == ""; time.Sleep(200 * time.Millisecond) {
		st := status()
		for _, p := range st.Projects {
			if p.Name == project {
				spec = p
			}
		}
		for _, e := range st.Environments {
			if e.Project == project && e.Kind == agentv2.EnvironmentKind_ENVIRONMENT_KIND_BASE {
				envID = e.Id
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent did not get the project: link %v", st.Link)
		}
	}
	if spec.WorkspaceId != wsID || spec.ConfigVersion != 1 || spec.AdminEmail != "lead@acme.dev" {
		t.Errorf("project on the device: %+v", spec)
	}
	// The developer's device has no workspace key yet: the teamlead's agent sees it
	// waiting and delivers the key; the environment goes on by itself.
	sc := syncv1connect.NewSyncServiceClient(http.DefaultClient, srv.URL)
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(300 * time.Millisecond) {
		wctx, wcancel := context.WithCancel(ctx)
		stream, err := sc.Watch(wctx, bearer(&syncv1.WatchRequest{WorkspaceId: wsID, Device: &syncv1.Device{Id: "lead-mac", PublicKey: leadDevice.Recipient().String()}}, lead))
		if err != nil || !stream.Receive() {
			t.Fatalf("lead's watch: %v", err)
		}
		m := stream.Msg()
		wcancel()
		if len(m.PendingDevices) > 0 {
			if n, err := link.DeliverKey(ctx, srv.URL, wsID, lead, store.ServerKey{Generation: m.Key.Generation, Wrapped: m.Key.Wrapped}, leadDevice, m.PendingDevices); err != nil || n != 1 {
				t.Fatalf("deliver the key: %d %v", n, err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the developer's device never waited for the key")
		}
	}
	var st *agentv2.EnvironmentStatus
	for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(300 * time.Millisecond) {
		for _, e := range status().Environments {
			if e.Id == envID {
				st = e
			}
		}
		if st != nil && st.Phase == agentv2.Phase_PHASE_RUNNING {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("environment: %v", st)
		}
	}
	roots := x509.NewCertPool()
	pemData, err := os.ReadFile(status().Ca.RootPath)
	if err != nil {
		t.Fatal(err)
	}
	roots.AppendCertsFromPEM(pemData)
	host := strings.TrimPrefix(strings.Split(st.Url, ":")[1], "//")
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
	serves("settings v1 pw " + password)

	// The teamlead changes the settings; the developer's environment follows.
	if _, err := ws.PutApp(ctx, bearer(&workspacev1.PutAppRequest{WorkspaceId: wsID, Project: project, Name: "web",
		AppYaml: strings.SplitN(appYAML("2"), "web:\n", 2)[1]}, lead)); err != nil {
		t.Fatal(err)
	}
	serves("settings v2 pw " + password)

	// The device told the server what it runs, with the new version.
	var devices []*syncv1.DeviceStatus
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		res, err := sc.ListDevices(ctx, bearer(&syncv1.ListDevicesRequest{WorkspaceId: wsID, Project: project}, lead))
		if err != nil {
			t.Fatal(err)
		}
		devices = res.Msg.Devices
		if len(devices) == 1 && len(devices[0].Environments) == 1 && devices[0].Environments[0].Phase == "running" && devices[0].Environments[0].ConfigVersion == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("devices: %v", devices)
		}
	}
	if devices[0].UserEmail != "dev@acme.dev" || !devices[0].Online {
		t.Errorf("device: %v", devices[0])
	}

	// The server goes away: the environment keeps running on what the agent has.
	srv.CloseClientConnections()
	srv.Close()
	serverUp = false
	time.Sleep(3 * time.Second)
	serves("settings v2 pw " + password)
	for _, e := range status().Environments {
		if e.Id == envID && e.Phase != agentv2.Phase_PHASE_RUNNING {
			t.Errorf("without the server: %v %s", e.Phase, e.Error)
		}
	}
	if l := status().Link; l.Connected {
		t.Errorf("link still connected: %v", l)
	}
}
