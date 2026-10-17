package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentv2 "github.com/amalychev/devopus/gen/devopus/agent/v2"
	"github.com/amalychev/devopus/gen/devopus/agent/v2/agentv2connect"
	workspacev1 "github.com/amalychev/devopus/gen/devopus/workspace/v1"
	"github.com/amalychev/devopus/gen/devopus/workspace/v1/workspacev1connect"
	"github.com/amalychev/devopus/internal/auth/authtest"
	"github.com/amalychev/devopus/internal/auth/token"
	"github.com/amalychev/devopus/internal/credentials"
	"github.com/amalychev/devopus/internal/importer"
	"github.com/amalychev/devopus/internal/pgtest"
	"github.com/amalychev/devopus/internal/server"
	serverstore "github.com/amalychev/devopus/internal/server/store"
	"gopkg.in/yaml.v3"
)

// team is a workspace on a real Auth and workspace server, with its admin signed in on
// this machine: the engine's end-to-end tests run their projects through it, as people
// do since projects live on the workspace server (R10). newTeam goes before the agent
// starts, so the agent links at once.
type team struct {
	ws    workspacev1connect.WorkspaceServiceClient
	id    string
	token string
}

func newTeam(t *testing.T, ctx context.Context) *team {
	t.Helper()
	t.Setenv("DEVOPUS_CREDENTIAL_STORE", credentials.StoreFile)
	a := authtest.Start(t)
	pool := pgtest.Pool(t)
	if err := serverstore.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	keys := &token.Remote{URL: a.URL + "/.well-known/jwks.json"}
	srv := httptest.NewUnstartedServer(nil)
	srv.Start()
	svc, err := server.New(pool, server.Config{Keys: keys.Key, AuthIssuer: a.URL, Mailer: a.Inbox, PublicURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = svc.Handler()
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	me := signUp(t, a, "admin@acme.dev")
	if err := credentials.Save(credentials.FromTokens(a.URL, me.User, me.Tokens), me.Tokens.RefreshToken); err != nil {
		t.Fatal(err)
	}
	// A token for this server: servers accept only tokens of their own address.
	acc, err := credentials.RefreshFor(ctx, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	tm := &team{ws: workspacev1connect.NewWorkspaceServiceClient(http.DefaultClient, srv.URL), token: acc.Token}
	w, err := tm.ws.CreateWorkspace(ctx, bearer(&workspacev1.CreateWorkspaceRequest{Name: "e2e"}, tm.token))
	if err != nil {
		t.Fatal(err)
	}
	tm.id = w.Msg.Workspace.Id
	if err := credentials.SetCurrentWorkspace(&credentials.Workspace{ID: tm.id, Name: "e2e", ServerURL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	return tm
}

// project creates a project on the server from the workspace.yaml v1 a test writes,
// the way the demo workspace is made, and returns its name.
func (tm *team) project(t *testing.T, ctx context.Context, v1 []byte) string {
	t.Helper()
	p, _, err := importer.Parse(v1)
	if err != nil {
		t.Fatal(err)
	}
	config, err := p.YAML()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tm.ws.CreateProject(ctx, bearer(&workspacev1.CreateProjectRequest{WorkspaceId: tm.id, Name: p.Name, Domain: p.Domain, ConfigYaml: string(config)}, tm.token)); err != nil {
		t.Fatal(err)
	}
	return p.Name
}

// setFeatures changes a project's feature policy on the server to the one of a v1
// config, and waits until the agent has the new version.
func (tm *team) setFeatures(t *testing.T, ctx context.Context, c agentv2connect.AgentServiceClient, v1 []byte) {
	t.Helper()
	p, _, err := importer.Parse(v1)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tm.ws.UpdateProjectSettings(ctx, bearer(&workspacev1.UpdateProjectSettingsRequest{WorkspaceId: tm.id, Project: p.Name,
		PolicyYaml: toYAML(t, map[string]any{"features": p.Features})}, tm.token))
	if err != nil {
		t.Fatal(err)
	}
	agentHas(t, ctx, c, p.Name, res.Msg.Config.Version)
}

// agentHas waits until the agent has a project of the workspace at a version (or any
// version when 0).
func agentHas(t *testing.T, ctx context.Context, c agentv2connect.AgentServiceClient, project string, version int32) {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(100 * time.Millisecond) {
		st, err := c.Status(ctx, connect.NewRequest(&agentv2.StatusRequest{}))
		if err == nil {
			for _, p := range st.Msg.Projects {
				if p.Name == project && p.WorkspaceId != "" && (version == 0 || p.ConfigVersion == version) {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent did not get %s v%d from the workspace server: %v", project, version, err)
		}
	}
}

// upProject brings a workspace project up on this machine with its folder, as
// 'devopus up <project> --dir <root>' does, and waits until it runs. The agent brought
// the project up on its own before the folder was linked, and 'up' interrupts that pass
// half-way: Docker may answer the next pass with a passing error, which the agent
// retries. So errors only fail the test when the environment never runs.
func upProject(t *testing.T, ctx context.Context, c agentv2connect.AgentServiceClient, project, root string) (*connect.Response[agentv2.UpResponse], *agentv2.EnvironmentStatus) {
	t.Helper()
	agentHas(t, ctx, c, project, 0)
	up, err := c.Up(ctx, connect.NewRequest(&agentv2.UpRequest{Project: project, Dir: root}))
	if err != nil {
		t.Fatal(err)
	}
	w, err := c.Watch(ctx, connect.NewRequest(&agentv2.WatchRequest{EnvironmentId: up.Msg.EnvironmentId}))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	last := ""
	for w.Receive() {
		s := w.Msg().Status
		if s == nil || s.AppliedRevision < up.Msg.Revision {
			continue
		}
		if s.Phase == agentv2.Phase_PHASE_ERROR {
			last = s.Error
		}
		if s.Phase == agentv2.Phase_PHASE_RUNNING {
			return up, s
		}
	}
	t.Fatalf("the environment never ran: %v; last error: %s", w.Err(), last)
	return nil, nil
}

func toYAML(t *testing.T, v any) string {
	t.Helper()
	b, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
