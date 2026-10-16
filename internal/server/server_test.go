package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"

	syncv1 "github.com/amalychev/devopus/gen/devopus/sync/v1"
	"github.com/amalychev/devopus/gen/devopus/sync/v1/syncv1connect"

	workspacev1 "github.com/amalychev/devopus/gen/devopus/workspace/v1"
	"github.com/amalychev/devopus/gen/devopus/workspace/v1/workspacev1connect"
	"github.com/amalychev/devopus/internal/agent/link"
	agentstore "github.com/amalychev/devopus/internal/agent/store"
	"github.com/amalychev/devopus/internal/auth/mail"
	"github.com/amalychev/devopus/internal/auth/token"
	"github.com/amalychev/devopus/internal/authz"
	"github.com/amalychev/devopus/internal/pgtest"
	"github.com/amalychev/devopus/internal/secrets"
	"github.com/amalychev/devopus/internal/server/store"
	"github.com/amalychev/devopus/internal/spec"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

const issuer = "https://auth.test"

type inbox struct {
	mu   sync.Mutex
	msgs []mail.Message
}

func (b *inbox) Send(_ context.Context, m mail.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs = append(b.msgs, m)
	return nil
}

type env struct {
	svc    *Service
	url    string
	key    token.Key
	client workspacev1connect.WorkspaceServiceClient
	inbox  *inbox
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := pgtest.Pool(t)
	if err := store.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := token.Key{ID: "k1", Private: priv}
	set := token.JWKS{Keys: []token.JWK{token.PublicJWK(key)}}
	in := &inbox{}
	svc, err := New(pool, Config{Keys: set.Find, AuthIssuer: issuer, Mailer: in})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(svc.Handler())
	t.Cleanup(srv.Close)
	return &env{svc: svc, url: srv.URL, key: key, client: workspacev1connect.NewWorkspaceServiceClient(http.DefaultClient, srv.URL), inbox: in}
}

// token is an access token Auth would issue to the user with this email.
func (e *env) token(email string) string {
	id := "usr_" + strings.Split(email, "@")[0]
	tok, err := token.Sign(e.key, token.Claims{Issuer: issuer, Subject: id, Session: "ses_" + id, Email: email,
		Audience: token.DefaultAudience, IssuedAt: time.Now().Unix(), Expiry: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		panic(err)
	}
	return tok
}

func as[T any](e *env, email string, msg *T) *connect.Request[T] {
	r := connect.NewRequest(msg)
	r.Header().Set("Authorization", "Bearer "+e.token(email))
	return r
}

func wantCode(t *testing.T, err error, code connect.Code) {
	t.Helper()
	if connect.CodeOf(err) != code {
		t.Fatalf("want %v, got %v", code, err)
	}
}

func must[T any](t *testing.T) func(*connect.Response[T], error) *T {
	return func(r *connect.Response[T], err error) *T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return r.Msg
	}
}

func service() protoreflect.ServiceDescriptor {
	return workspacev1.File_devopus_workspace_v1_workspace_proto.Services().ByName("WorkspaceService")
}

// methods are the calls of every service the server serves.
func methods() []protoreflect.MethodDescriptor {
	var out []protoreflect.MethodDescriptor
	for _, sd := range []protoreflect.ServiceDescriptor{service(), syncv1.File_devopus_sync_v1_sync_proto.Services().ByName("SyncService")} {
		for i := range sd.Methods().Len() {
			out = append(out, sd.Methods().Get(i))
		}
	}
	return out
}

// TestEveryCallDeclaresAccess: a call without a declared permission would be refused
// by the interceptor, and must not reach production either.
func TestEveryCallDeclaresAccess(t *testing.T) {
	for _, m := range methods() {
		a := AccessOf(m)
		if a == nil {
			t.Errorf("%s declares no access", m.Name())
			continue
		}
		if a.Scope == workspacev1.Scope_SCOPE_SIGNED_IN {
			if a.Permission != "" {
				t.Errorf("%s: a signed-in call with a permission", m.Name())
			}
			continue
		}
		if !authz.Valid(authz.Permission(a.Permission)) {
			t.Errorf("%s: unknown permission %q", m.Name(), a.Permission)
		}
		if a.Scope == workspacev1.Scope_SCOPE_PROJECT && m.Input().Fields().ByName("project") == nil {
			t.Errorf("%s is checked in a project but has no project field", m.Name())
		}
		if m.Input().Fields().ByName("workspace_id") == nil {
			t.Errorf("%s is checked in a workspace but has no workspace_id", m.Name())
		}
	}
}

// fixture is a workspace with an admin, a teamlead and a developer of project shop,
// and an outsider who is a member elsewhere. Every user gets a unique email.
type fixture struct {
	ws                         string
	admin, lead, dev, outsider string
	subjects                   map[string]authz.Subject
}

var fixtures int

func (e *env) fixture(t *testing.T) fixture {
	t.Helper()
	fixtures++
	n := fixtures
	f := fixture{
		admin: fmt.Sprintf("admin%d@acme.dev", n), lead: fmt.Sprintf("lead%d@acme.dev", n),
		dev: fmt.Sprintf("dev%d@acme.dev", n), outsider: fmt.Sprintf("out%d@acme.dev", n),
	}
	ctx := context.Background()
	f.ws = must[workspacev1.CreateWorkspaceResponse](t)(e.client.CreateWorkspace(ctx, as(e, f.admin, &workspacev1.CreateWorkspaceRequest{Name: fmt.Sprintf("acme %d", n)}))).Workspace.Id
	must[workspacev1.CreateWorkspaceResponse](t)(e.client.CreateWorkspace(ctx, as(e, f.outsider, &workspacev1.CreateWorkspaceRequest{Name: "elsewhere"})))
	must[workspacev1.CreateProjectResponse](t)(e.client.CreateProject(ctx, as(e, f.admin, &workspacev1.CreateProjectRequest{WorkspaceId: f.ws, Name: "shop"})))
	for _, m := range []struct{ email, role string }{{f.lead, "teamlead"}, {f.dev, "developer"}} {
		inv := must[workspacev1.CreateInviteResponse](t)(e.client.CreateInvite(ctx, as(e, f.admin, &workspacev1.CreateInviteRequest{WorkspaceId: f.ws, Email: m.email, Role: m.role, Projects: []string{"shop"}})))
		must[workspacev1.AcceptInviteResponse](t)(e.client.AcceptInvite(ctx, as(e, m.email, &workspacev1.AcceptInviteRequest{Code: inv.Code})))
	}
	f.subjects = map[string]authz.Subject{
		f.admin:    {Role: authz.Admin},
		f.lead:     {Role: authz.Teamlead, Projects: map[string]authz.Role{"shop": authz.Teamlead}},
		f.dev:      {Role: authz.Developer, Projects: map[string]authz.Role{"shop": authz.Developer}},
		f.outsider: {},
	}
	return f
}

// call calls a method over HTTP as a client of any language would, with only the
// fields that locate it: the workspace and the project.
func (e *env) call(t *testing.T, method protoreflect.MethodDescriptor, bearer, ws string) string {
	t.Helper()
	body := map[string]string{}
	in := method.Input().Fields()
	if in.ByName("workspace_id") != nil {
		body["workspaceId"] = ws
	}
	if in.ByName("project") != nil {
		body["project"] = "shop"
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, e.url+"/"+string(method.Parent().FullName())+"/"+string(method.Name()), bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return "ok"
	}
	var out struct{ Code, Message string }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Code + ": " + out.Message
}

// TestRoleMatrix calls every method as every role, straight over HTTP: the roles
// without the declared permission are refused, the others get past the check.
func TestRoleMatrix(t *testing.T) {
	e := newEnv(t)
	for _, m := range methods() {
		if m.IsStreamingServer() {
			continue // TestWatch checks the stream's access
		}
		a := AccessOf(m)
		for _, role := range []string{"admin", "teamlead", "developer", "outsider", "anonymous"} {
			// A fresh workspace for every call: the admin's DeleteWorkspace must not
			// spoil the next one.
			f := e.fixture(t)
			email := map[string]string{"admin": f.admin, "teamlead": f.lead, "developer": f.dev, "outsider": f.outsider}[role]
			bearer := ""
			if email != "" {
				bearer = e.token(email)
			}
			got := e.call(t, m, bearer, f.ws)
			var allowed bool
			switch {
			case role == "anonymous":
				if !strings.HasPrefix(got, "unauthenticated") {
					t.Errorf("%s as anonymous: %s", m.Name(), got)
				}
				continue
			case a.Scope == workspacev1.Scope_SCOPE_SIGNED_IN:
				allowed = true
			case a.Scope == workspacev1.Scope_SCOPE_PROJECT:
				allowed = f.subjects[email].Can(authz.Permission(a.Permission), "shop")
			default:
				allowed = f.subjects[email].Can(authz.Permission(a.Permission), "")
			}
			deniedNow := strings.HasPrefix(got, "permission_denied")
			if allowed == deniedNow || strings.HasPrefix(got, "unauthenticated") || strings.HasPrefix(got, "internal") {
				t.Errorf("%s as %s (allowed %v): %s", m.Name(), role, allowed, got)
			}
			if deniedNow && !strings.Contains(got, a.Permission) && !strings.Contains(got, "not a member") {
				t.Errorf("%s as %s: the refusal does not name %s: %s", m.Name(), role, a.Permission, got)
			}
		}
	}
	// A token Auth did not sign.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	forged, _ := token.Sign(token.Key{ID: "k1", Private: priv}, token.Claims{Issuer: issuer, Subject: "usr_x", Session: "s", Email: "x@acme.dev", Audience: token.DefaultAudience, Expiry: time.Now().Add(time.Hour).Unix()})
	if got := e.call(t, service().Methods().ByName("ListWorkspaces"), forged, ""); !strings.HasPrefix(got, "unauthenticated") {
		t.Errorf("forged token: %s", got)
	}
}

func TestAdminTeamleadDeveloper(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	const admin, lead, dev, stranger = "admin@acme.dev", "lead@acme.dev", "dev@acme.dev", "stranger@acme.dev"

	ws := must[workspacev1.CreateWorkspaceResponse](t)(e.client.CreateWorkspace(ctx, as(e, admin, &workspacev1.CreateWorkspaceRequest{Name: "Acme"}))).Workspace
	if ws.Role != "admin" || ws.AdminEmail != admin {
		t.Fatalf("workspace = %+v", ws)
	}
	must[workspacev1.CreateProjectResponse](t)(e.client.CreateProject(ctx, as(e, admin, &workspacev1.CreateProjectRequest{WorkspaceId: ws.Id, Name: "shop"})))
	_, err := e.client.CreateProject(ctx, as(e, admin, &workspacev1.CreateProjectRequest{WorkspaceId: ws.Id, Name: "Bad Name"}))
	wantCode(t, err, connect.CodeInvalidArgument)

	// The admin invites a teamlead into shop; the invite email carries the code.
	inv := must[workspacev1.CreateInviteResponse](t)(e.client.CreateInvite(ctx, as(e, admin, &workspacev1.CreateInviteRequest{WorkspaceId: ws.Id, Email: "Lead@Acme.dev", Role: "teamlead", Projects: []string{"shop"}})))
	if len(e.inbox.msgs) != 1 || !strings.Contains(e.inbox.msgs[0].Text, "devopus invite accept "+inv.Code) || e.inbox.msgs[0].To != lead {
		t.Fatalf("invite email: %+v", e.inbox.msgs)
	}
	_, err = e.client.AcceptInvite(ctx, as(e, stranger, &workspacev1.AcceptInviteRequest{Code: inv.Code}))
	wantCode(t, err, connect.CodePermissionDenied)
	must[workspacev1.AcceptInviteResponse](t)(e.client.AcceptInvite(ctx, as(e, lead, &workspacev1.AcceptInviteRequest{Code: inv.Code})))
	_, err = e.client.AcceptInvite(ctx, as(e, lead, &workspacev1.AcceptInviteRequest{Code: inv.Code}))
	wantCode(t, err, connect.CodeFailedPrecondition)

	// The teamlead invites developers into the projects they lead, and only developers.
	_, err = e.client.CreateInvite(ctx, as(e, lead, &workspacev1.CreateInviteRequest{WorkspaceId: ws.Id, Email: "x@acme.dev", Role: "teamlead", Projects: []string{"shop"}}))
	wantCode(t, err, connect.CodePermissionDenied)
	must[workspacev1.CreateProjectResponse](t)(e.client.CreateProject(ctx, as(e, lead, &workspacev1.CreateProjectRequest{WorkspaceId: ws.Id, Name: "blog"})))
	must[workspacev1.CreateProjectResponse](t)(e.client.CreateProject(ctx, as(e, admin, &workspacev1.CreateProjectRequest{WorkspaceId: ws.Id, Name: "crm"})))
	_, err = e.client.CreateInvite(ctx, as(e, lead, &workspacev1.CreateInviteRequest{WorkspaceId: ws.Id, Email: dev, Role: "developer", Projects: []string{"crm"}}))
	wantCode(t, err, connect.CodePermissionDenied)
	devInv := must[workspacev1.CreateInviteResponse](t)(e.client.CreateInvite(ctx, as(e, lead, &workspacev1.CreateInviteRequest{WorkspaceId: ws.Id, Email: dev, Role: "developer", Projects: []string{"shop", "blog"}})))
	must[workspacev1.AcceptInviteResponse](t)(e.client.AcceptInvite(ctx, as(e, dev, &workspacev1.AcceptInviteRequest{Code: devInv.Code})))

	// The developer may not invite: refused by the permission, which the message names.
	_, err = e.client.CreateInvite(ctx, as(e, dev, &workspacev1.CreateInviteRequest{WorkspaceId: ws.Id, Email: "y@acme.dev", Role: "developer", Projects: []string{"shop"}}))
	wantCode(t, err, connect.CodePermissionDenied)
	if !strings.Contains(err.Error(), "members.invite") {
		t.Errorf("refusal: %v", err)
	}
	projects := must[workspacev1.ListProjectsResponse](t)(e.client.ListProjects(ctx, as(e, dev, &workspacev1.ListProjectsRequest{WorkspaceId: ws.Id}))).Projects
	if len(projects) != 2 {
		t.Errorf("the developer sees %d projects", len(projects))
	}

	// Grants: the teamlead gives the developer services.manage in shop, not more.
	must[workspacev1.GrantPermissionResponse](t)(e.client.GrantPermission(ctx, as(e, lead, &workspacev1.GrantPermissionRequest{WorkspaceId: ws.Id, Project: "shop", Email: dev, Permission: "services.manage"})))
	_, err = e.client.GrantPermission(ctx, as(e, lead, &workspacev1.GrantPermissionRequest{WorkspaceId: ws.Id, Project: "shop", Email: dev, Permission: "members.manage"}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = e.client.GrantPermission(ctx, as(e, lead, &workspacev1.GrantPermissionRequest{WorkspaceId: ws.Id, Project: "crm", Email: dev, Permission: "services.manage"}))
	wantCode(t, err, connect.CodePermissionDenied)
	me := must[workspacev1.GetWorkspaceResponse](t)(e.client.GetWorkspace(ctx, as(e, dev, &workspacev1.GetWorkspaceRequest{WorkspaceId: ws.Id})))
	if len(me.Projects) != 2 || me.Projects[1].Project != "shop" || !strings.Contains(strings.Join(me.Projects[1].Permissions, ","), "services.manage") {
		t.Fatalf("developer's permissions: %v", me.Projects)
	}
	members := must[workspacev1.ListMembersResponse](t)(e.client.ListMembers(ctx, as(e, dev, &workspacev1.ListMembersRequest{WorkspaceId: ws.Id}))).Members
	if len(members) != 3 || members[0].Email != admin || members[2].Email != dev || len(members[2].Projects) != 2 || members[2].Projects[1].Grants[0] != "services.manage" {
		t.Fatalf("members: %v", members)
	}

	// A teamlead removes developers from a project, not other teamleads.
	_, err = e.client.RemoveFromProject(ctx, as(e, lead, &workspacev1.RemoveFromProjectRequest{WorkspaceId: ws.Id, Project: "shop", Email: lead}))
	wantCode(t, err, connect.CodePermissionDenied)
	must[workspacev1.RemoveFromProjectResponse](t)(e.client.RemoveFromProject(ctx, as(e, lead, &workspacev1.RemoveFromProjectRequest{WorkspaceId: ws.Id, Project: "shop", Email: dev})))
	me = must[workspacev1.GetWorkspaceResponse](t)(e.client.GetWorkspace(ctx, as(e, dev, &workspacev1.GetWorkspaceRequest{WorkspaceId: ws.Id})))
	if len(me.Projects) != 1 || me.Projects[0].Project != "blog" {
		t.Fatalf("after removal from shop: %v", me.Projects)
	}

	// Invites: the teamlead sees their own; a revoked one cannot be accepted.
	late := must[workspacev1.CreateInviteResponse](t)(e.client.CreateInvite(ctx, as(e, admin, &workspacev1.CreateInviteRequest{WorkspaceId: ws.Id, Email: "late@acme.dev", Role: "developer", Projects: []string{"crm"}})))
	if n := len(must[workspacev1.ListInvitesResponse](t)(e.client.ListInvites(ctx, as(e, lead, &workspacev1.ListInvitesRequest{WorkspaceId: ws.Id}))).Invites); n != 0 {
		t.Errorf("the teamlead sees %d invites of others", n)
	}
	_, err = e.client.RevokeInvite(ctx, as(e, lead, &workspacev1.RevokeInviteRequest{WorkspaceId: ws.Id, InviteId: late.Invite.Id}))
	wantCode(t, err, connect.CodePermissionDenied)
	must[workspacev1.RevokeInviteResponse](t)(e.client.RevokeInvite(ctx, as(e, admin, &workspacev1.RevokeInviteRequest{WorkspaceId: ws.Id, InviteId: late.Invite.Id})))
	_, err = e.client.AcceptInvite(ctx, as(e, "late@acme.dev", &workspacev1.AcceptInviteRequest{Code: late.Code}))
	wantCode(t, err, connect.CodeFailedPrecondition)

	// Transfer: the teamlead becomes the admin, the old admin a teamlead.
	_, err = e.client.TransferWorkspace(ctx, as(e, admin, &workspacev1.TransferWorkspaceRequest{WorkspaceId: ws.Id, Email: stranger}))
	wantCode(t, err, connect.CodeNotFound)
	must[workspacev1.TransferWorkspaceResponse](t)(e.client.TransferWorkspace(ctx, as(e, admin, &workspacev1.TransferWorkspaceRequest{WorkspaceId: ws.Id, Email: lead})))
	_, err = e.client.ListAudit(ctx, as(e, admin, &workspacev1.ListAuditRequest{WorkspaceId: ws.Id}))
	wantCode(t, err, connect.CodePermissionDenied)
	audit := must[workspacev1.ListAuditResponse](t)(e.client.ListAudit(ctx, as(e, lead, &workspacev1.ListAuditRequest{WorkspaceId: ws.Id}))).Events
	var actions []string
	for _, ev := range audit {
		actions = append(actions, ev.Action)
	}
	if got := strings.Join(actions, ","); !strings.HasPrefix(got, "workspace.transferred,invite.revoked,member.invited,member.removed_from_project,permission.granted,member.joined") {
		t.Errorf("audit: %s", got)
	}

	// The admin cannot be removed; a removed member loses access at once.
	_, err = e.client.RemoveMember(ctx, as(e, lead, &workspacev1.RemoveMemberRequest{WorkspaceId: ws.Id, Email: lead}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	must[workspacev1.RemoveMemberResponse](t)(e.client.RemoveMember(ctx, as(e, lead, &workspacev1.RemoveMemberRequest{WorkspaceId: ws.Id, Email: dev})))
	_, err = e.client.GetWorkspace(ctx, as(e, dev, &workspacev1.GetWorkspaceRequest{WorkspaceId: ws.Id}))
	wantCode(t, err, connect.CodePermissionDenied)

	// Deleting takes the name again.
	_, err = e.client.DeleteWorkspace(ctx, as(e, lead, &workspacev1.DeleteWorkspaceRequest{WorkspaceId: ws.Id, ConfirmName: "acme"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	must[workspacev1.DeleteWorkspaceResponse](t)(e.client.DeleteWorkspace(ctx, as(e, lead, &workspacev1.DeleteWorkspaceRequest{WorkspaceId: ws.Id, ConfirmName: "Acme"})))
	if n := len(must[workspacev1.ListWorkspacesResponse](t)(e.client.ListWorkspaces(ctx, as(e, admin, &workspacev1.ListWorkspacesRequest{}))).Workspaces); n != 0 {
		t.Errorf("%d workspaces after delete", n)
	}
}

func TestProjectConfig(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	f := e.fixture(t) // shop with a teamlead and a developer
	cfg := func(r *workspacev1.ProjectConfig) *spec.Project {
		t.Helper()
		p, err := spec.ParseYAML([]byte(r.Yaml))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	get := func() *workspacev1.ProjectConfig {
		t.Helper()
		return must[workspacev1.GetProjectConfigResponse](t)(e.client.GetProjectConfig(ctx, as(e, f.dev, &workspacev1.GetProjectConfigRequest{WorkspaceId: f.ws, Project: "shop"}))).Config
	}
	v1 := get()
	if v1.Version != 1 || cfg(v1).Domain != "shop.localhost" || v1.Message != "created" {
		t.Fatalf("first version: %+v", v1)
	}

	// The teamlead adds an application and its services.
	putApp := func(who, yaml string, create bool, base int32) (*workspacev1.ProjectConfig, error) {
		r, err := e.client.PutApp(ctx, as(e, who, &workspacev1.PutAppRequest{WorkspaceId: f.ws, Project: "shop", Name: "api", AppYaml: yaml, Create: create, BaseVersion: base}))
		if err != nil {
			return nil, err
		}
		return r.Msg.Config, nil
	}
	putSvc := func(who, name, yaml string, create bool) (*workspacev1.ProjectConfig, error) {
		r, err := e.client.PutService(ctx, as(e, who, &workspacev1.PutServiceRequest{WorkspaceId: f.ws, Project: "shop", Name: name, ServiceYaml: yaml, Create: create}))
		if err != nil {
			return nil, err
		}
		return r.Msg.Config, nil
	}
	_, err := putApp(f.lead, "routes: [/api]\n", true, 0)
	wantCode(t, err, connect.CodeInvalidArgument) // no code to run
	_, err = putApp(f.lead, "repo: x\nnope: 1\n", true, 0)
	wantCode(t, err, connect.CodeInvalidArgument) // unknown key
	v2, err := putApp(f.lead, "repo: git@github.com:acme/api.git\nroutes: [/api]\n", true, 1)
	if err != nil || v2.Version != 2 || v2.Author != f.lead || v2.Message != "app add api" {
		t.Fatalf("app add: %+v %v", v2, err)
	}
	_, err = putApp(f.lead, "repo: git@github.com:acme/api.git\n", false, 1)
	wantCode(t, err, connect.CodeAborted) // made against version 1
	if _, err := putSvc(f.lead, "php", "type: php\nversion: \"8.3\"\napps: [api]\n", true); err != nil {
		t.Fatal(err)
	}
	_, err = putSvc(f.lead, "db", "type: postgresql\napps: [api]\n", true)
	wantCode(t, err, connect.CodeInvalidArgument) // no version
	v4, err := putSvc(f.lead, "db", "type: postgresql\nversion: \"17\"\napps: [api]\n", true)
	if err != nil || cfg(v4).Services["db"].Type != "postgres" {
		t.Fatalf("service add db: %+v %v", v4, err)
	}

	// The developer reads but does not change; with services.manage granted, changes
	// services but not applications.
	_, err = putSvc(f.dev, "php", "type: php\nversion: \"8.4\"\napps: [api]\n", false)
	wantCode(t, err, connect.CodePermissionDenied)
	must[workspacev1.GrantPermissionResponse](t)(e.client.GrantPermission(ctx, as(e, f.lead, &workspacev1.GrantPermissionRequest{WorkspaceId: f.ws, Project: "shop", Email: f.dev, Permission: "services.manage"})))
	v5, err := putSvc(f.dev, "php", "type: php\nversion: \"8.4\"\napps: [api]\n", false)
	if err != nil || cfg(v5).Services["php"].Version != "8.4" || v5.Message != "service set php (php 8.4)" {
		t.Fatalf("developer's service set: %+v %v", v5, err)
	}
	_, err = putApp(f.dev, "repo: elsewhere\n", false, 0)
	wantCode(t, err, connect.CodePermissionDenied)
	// The same settings again add no version.
	if again, _ := putSvc(f.dev, "php", "type: php\nversion: \"8.4\"\napps: [api]\n", false); again.Version != 5 {
		t.Errorf("an empty change made version %d", again.Version)
	}

	// Rollback: version 4 again, as version 6; the history keeps everything.
	r := must[workspacev1.RollbackProjectResponse](t)(e.client.RollbackProject(ctx, as(e, f.lead, &workspacev1.RollbackProjectRequest{WorkspaceId: f.ws, Project: "shop", Version: 4}))).Config
	if r.Version != 6 || r.Message != "rollback to 4" || cfg(r).Services["php"].Version != "8.3" {
		t.Fatalf("rollback: %+v", r)
	}
	hist := must[workspacev1.ListProjectVersionsResponse](t)(e.client.ListProjectVersions(ctx, as(e, f.dev, &workspacev1.ListProjectVersionsRequest{WorkspaceId: f.ws, Project: "shop"}))).Versions
	if len(hist) != 6 || hist[0].Version != 6 || hist[5].Message != "created" {
		t.Fatalf("history: %v", hist)
	}
	old := must[workspacev1.GetProjectConfigResponse](t)(e.client.GetProjectConfig(ctx, as(e, f.dev, &workspacev1.GetProjectConfigRequest{WorkspaceId: f.ws, Project: "shop", Version: 5})))
	if old.CurrentVersion != 6 || cfg(old.Config).Services["php"].Version != "8.4" {
		t.Fatalf("version 5: %+v", old)
	}

	// Removing an application with attached services needs services.manage too: the
	// teamlead has it; the services are detached.
	e.client.RemoveService(ctx, as(e, f.lead, &workspacev1.RemoveServiceRequest{WorkspaceId: f.ws, Project: "shop", Name: "db"}))
	rm := must[workspacev1.RemoveAppResponse](t)(e.client.RemoveApp(ctx, as(e, f.lead, &workspacev1.RemoveAppRequest{WorkspaceId: f.ws, Project: "shop", Name: "api"}))).Config
	if p := cfg(rm); len(p.Apps) != 0 || len(p.Services["php"].Apps) != 0 {
		t.Fatalf("after app rm: %s", rm.Yaml)
	}

	// A project from an import, and its deletion by the teamlead, not the developer.
	imported := "apps:\n  web:\n    path: web\n    routes: [/]\nservices:\n  node:\n    type: nodejs\n    version: \"22\"\n    apps: [web]\n"
	cr := must[workspacev1.CreateProjectResponse](t)(e.client.CreateProject(ctx, as(e, f.lead, &workspacev1.CreateProjectRequest{WorkspaceId: f.ws, Name: "blog", Domain: "blog.test", ConfigYaml: imported})))
	if p := cfg(cr.Config); p.Name != "blog" || p.Domain != "blog.test" || p.Apps["web"] == nil || cr.Config.Message != "created from an import" {
		t.Fatalf("import: %s", cr.Config.Yaml)
	}
	_, err = e.client.DeleteProject(ctx, as(e, f.dev, &workspacev1.DeleteProjectRequest{WorkspaceId: f.ws, Project: "shop", ConfirmName: "shop"}))
	wantCode(t, err, connect.CodePermissionDenied)
	must[workspacev1.DeleteProjectResponse](t)(e.client.DeleteProject(ctx, as(e, f.lead, &workspacev1.DeleteProjectRequest{WorkspaceId: f.ws, Project: "blog", ConfirmName: "blog"})))
}

func TestWatch(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	f := e.fixture(t)
	sc := syncv1connect.NewSyncServiceClient(http.DefaultClient, e.url)
	device := &syncv1.Device{Id: "dev-laptop", Name: "laptop", AgentVersion: "test"}
	must[workspacev1.PutAppResponse](t)(e.client.PutApp(ctx, as(e, f.lead, &workspacev1.PutAppRequest{WorkspaceId: f.ws, Project: "shop", Name: "api", AppYaml: "repo: git@github.com:acme/api.git\n", Create: true})))

	type got struct {
		msg *syncv1.WatchResponse
		err error
	}
	watch := func(email string) chan got {
		out := make(chan got, 10)
		go func() {
			st, err := sc.Watch(ctx, as(e, email, &syncv1.WatchRequest{WorkspaceId: f.ws, Device: device}))
			if err != nil {
				out <- got{err: err}
				return
			}
			for st.Receive() {
				out <- got{msg: st.Msg()}
			}
			out <- got{err: st.Err()}
			close(out)
		}()
		return out
	}
	next := func(ch chan got) got {
		t.Helper()
		select {
		case g := <-ch:
			return g
		case <-time.After(5 * time.Second):
			t.Fatal("no message from the stream")
		}
		return got{}
	}

	dev := watch(f.dev)
	first := next(dev)
	if first.err != nil || len(first.msg.Projects) != 1 || first.msg.Projects[0].Version != 2 || first.msg.AdminEmail != f.admin {
		t.Fatalf("first state: %+v %v", first.msg, first.err)
	}
	// The teamlead's change reaches the developer's agent at once.
	start := time.Now()
	must[workspacev1.PutServiceResponse](t)(e.client.PutService(ctx, as(e, f.lead, &workspacev1.PutServiceRequest{WorkspaceId: f.ws, Project: "shop", Name: "php", ServiceYaml: "type: php\nversion: \"8.4\"\napps: [api]\n", Create: true})))
	second := next(dev)
	if second.err != nil || second.msg.Projects[0].Version != 3 || !strings.Contains(second.msg.Projects[0].ConfigYaml, "8.4") {
		t.Fatalf("after the change: %+v %v", second.msg, second.err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the change took %v", d)
	}

	// An outsider gets nothing.
	if g := next(watch(f.outsider)); connect.CodeOf(g.err) != connect.CodePermissionDenied {
		t.Errorf("outsider: %+v", g)
	}

	// The device reports; the teamlead sees what it runs, the outsider's view is empty.
	must[syncv1.ReportStatusResponse](t)(sc.ReportStatus(ctx, as(e, f.dev, &syncv1.ReportStatusRequest{WorkspaceId: f.ws, Device: device, Environments: []*syncv1.EnvironmentReport{
		{Project: "shop", Id: "e1", Kind: "base", Phase: "running", Url: "https://shop.localhost", ConfigVersion: 3},
		{Project: "shop", Id: "e2", Kind: "feature", Name: "PAY-1", Branch: "feature/PAY-1", Phase: "running", ConfigVersion: 3},
	}})))
	devices := must[syncv1.ListDevicesResponse](t)(sc.ListDevices(ctx, as(e, f.lead, &syncv1.ListDevicesRequest{WorkspaceId: f.ws, Project: "shop"}))).Devices
	if len(devices) != 1 || devices[0].UserEmail != f.dev || !devices[0].Online || len(devices[0].Environments) != 2 || devices[0].Environments[1].Name != "PAY-1" {
		t.Fatalf("devices: %v", devices)
	}

	// Removing the developer cuts the stream off.
	must[workspacev1.RemoveMemberResponse](t)(e.client.RemoveMember(ctx, as(e, f.admin, &workspacev1.RemoveMemberRequest{WorkspaceId: f.ws, Email: f.dev})))
	if g := next(dev); connect.CodeOf(g.err) != connect.CodePermissionDenied {
		t.Fatalf("removed member's stream: %+v", g)
	}

	// The stream ends with the access token.
	short, _ := token.Sign(e.key, token.Claims{Issuer: issuer, Subject: "usr_" + strings.Split(f.lead, "@")[0], Session: "s", Email: f.lead,
		Audience: token.DefaultAudience, Expiry: time.Now().Add(2 * time.Second).Unix()})
	r := connect.NewRequest(&syncv1.WatchRequest{WorkspaceId: f.ws, Device: device})
	r.Header().Set("Authorization", "Bearer "+short)
	st, err := sc.Watch(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for st.Receive() {
		n++
	}
	if n != 1 || st.Err() != nil {
		t.Errorf("short token: %d messages, %v", n, st.Err())
	}
}

func TestSecrets(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	f := e.fixture(t)
	sc := syncv1connect.NewSyncServiceClient(http.DefaultClient, e.url)
	adminDev, _ := secrets.NewKey()
	devDev, _ := secrets.NewKey()
	const value = "pa55-w0rd-of-the-db"

	// No key yet: setting a secret is refused until the admin makes one.
	sealedNowhere, _ := secrets.Seal([]byte(value), adminDev.Recipient().String())
	_, err := e.client.SetSecret(ctx, as(e, f.lead, &workspacev1.SetSecretRequest{WorkspaceId: f.ws, Project: "shop", Name: "DB_PASSWORD", Generation: 1, Ciphertext: sealedNowhere}))
	wantCode(t, err, connect.CodeAborted)

	// The admin's device registers its key and makes the workspace's.
	must[workspacev1.RegisterDeviceKeyResponse](t)(e.client.RegisterDeviceKey(ctx, as(e, f.admin, &workspacev1.RegisterDeviceKeyRequest{WorkspaceId: f.ws, DeviceId: "admin-mac", DeviceName: "mac", PublicKey: adminDev.Recipient().String()})))
	wsKey, _ := secrets.NewKey()
	wrapped, _ := secrets.Wrap(wsKey, adminDev.Recipient().String())
	_, err = e.client.CreateSecretsKey(ctx, as(e, f.lead, &workspacev1.CreateSecretsKeyRequest{WorkspaceId: f.ws, PublicKey: wsKey.Recipient().String(), DeviceId: "x", Wrapped: wrapped}))
	wantCode(t, err, connect.CodePermissionDenied)
	must[workspacev1.CreateSecretsKeyResponse](t)(e.client.CreateSecretsKey(ctx, as(e, f.admin, &workspacev1.CreateSecretsKeyRequest{WorkspaceId: f.ws, PublicKey: wsKey.Recipient().String(), DeviceId: "admin-mac", Wrapped: wrapped})))
	_, err = e.client.CreateSecretsKey(ctx, as(e, f.admin, &workspacev1.CreateSecretsKeyRequest{WorkspaceId: f.ws, PublicKey: wsKey.Recipient().String(), DeviceId: "admin-mac", Wrapped: wrapped}))
	wantCode(t, err, connect.CodeAlreadyExists)

	// The teamlead sets a secret with the public key alone; the developer may not.
	key := must[workspacev1.GetSecretsKeyResponse](t)(e.client.GetSecretsKey(ctx, as(e, f.lead, &workspacev1.GetSecretsKeyRequest{WorkspaceId: f.ws})))
	sealed, _ := secrets.Seal([]byte(value), key.PublicKey)
	must[workspacev1.SetSecretResponse](t)(e.client.SetSecret(ctx, as(e, f.lead, &workspacev1.SetSecretRequest{WorkspaceId: f.ws, Project: "shop", Name: "DB_PASSWORD", Generation: key.Generation, Ciphertext: sealed})))
	_, err = e.client.SetSecret(ctx, as(e, f.dev, &workspacev1.SetSecretRequest{WorkspaceId: f.ws, Project: "shop", Name: "X", Generation: key.Generation, Ciphertext: sealed}))
	wantCode(t, err, connect.CodePermissionDenied)
	names := must[workspacev1.ListSecretsResponse](t)(e.client.ListSecrets(ctx, as(e, f.dev, &workspacev1.ListSecretsRequest{WorkspaceId: f.ws, Project: "shop"}))).Secrets
	if len(names) != 1 || names[0].Name != "DB_PASSWORD" || names[0].UpdatedBy != f.lead {
		t.Fatalf("names: %v", names)
	}

	// The server never had the value: not in any table.
	var leaks int
	if err := e.svc.pool.QueryRow(ctx, `SELECT count(*) FROM secrets WHERE position($1::bytea in ciphertext) > 0`, []byte(value)).Scan(&leaks); err != nil || leaks != 0 {
		t.Fatalf("plaintext in the database: %d %v", leaks, err)
	}

	// The developer's device connects: it gets the ciphertext but no key yet; the
	// admin's device sees it waiting and wraps the key for it (variant A).
	first := func(email, device string, pub string) *syncv1.WatchResponse {
		t.Helper()
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		st, err := sc.Watch(ctx, as(e, email, &syncv1.WatchRequest{WorkspaceId: f.ws, Device: &syncv1.Device{Id: device, Name: device, PublicKey: pub}}))
		if err != nil || !st.Receive() {
			t.Fatalf("watch as %s: %v %v", email, err, st.Err())
		}
		return st.Msg()
	}
	dev := first(f.dev, "dev-laptop", devDev.Recipient().String())
	if dev.Key.GetGeneration() != 1 || len(dev.Key.Wrapped) != 0 || len(dev.Projects[0].Secrets["DB_PASSWORD"]) == 0 || len(dev.PendingDevices) != 0 {
		t.Fatalf("developer before the key: %+v", dev)
	}
	adm := first(f.admin, "admin-mac", adminDev.Recipient().String())
	if len(adm.PendingDevices) != 1 || adm.PendingDevices[0].UserEmail != f.dev {
		t.Fatalf("pending for the admin: %v", adm.PendingDevices)
	}
	n, err := link.DeliverKey(ctx, e.url, f.ws, e.token(f.admin), agentstore.ServerKey{Generation: adm.Key.Generation, Wrapped: adm.Key.Wrapped}, adminDev, adm.PendingDevices)
	if err != nil || n != 1 {
		t.Fatalf("deliver: %d %v", n, err)
	}
	dev = first(f.dev, "dev-laptop", devDev.Recipient().String())
	got, err := secrets.Unwrap(dev.Key.Wrapped, devDev)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := secrets.Open(dev.Projects[0].Secrets["DB_PASSWORD"], got); err != nil || string(v) != value {
		t.Fatalf("the developer's device opens %q %v", v, err)
	}
	// A developer may not deliver keys.
	_, err = e.client.ProvideKeyWraps(ctx, as(e, f.dev, &workspacev1.ProvideKeyWrapsRequest{WorkspaceId: f.ws, Generation: 1}))
	wantCode(t, err, connect.CodePermissionDenied)

	// The developer is removed, the admin rotates: the secret opens with the new key
	// on the admin's device, and the removed member's device keys are gone.
	must[workspacev1.RemoveMemberResponse](t)(e.client.RemoveMember(ctx, as(e, f.admin, &workspacev1.RemoveMemberRequest{WorkspaceId: f.ws, Email: f.dev})))
	prep := must[workspacev1.PrepareRotationResponse](t)(e.client.PrepareRotation(ctx, as(e, f.admin, &workspacev1.PrepareRotationRequest{WorkspaceId: f.ws})))
	if len(prep.Secrets) != 1 || len(prep.Devices) != 1 || prep.Devices[0].DeviceId != "admin-mac" {
		t.Fatalf("rotation: %d secrets, devices %v", len(prep.Secrets), prep.Devices)
	}
	next, _ := secrets.NewKey()
	v, _ := secrets.Open(prep.Secrets[0].Ciphertext, wsKey)
	resealed, _ := secrets.Seal(v, next.Recipient().String())
	rewrapped, _ := secrets.Wrap(next, adminDev.Recipient().String())
	_, err = e.client.RotateSecretsKey(ctx, as(e, f.admin, &workspacev1.RotateSecretsKeyRequest{WorkspaceId: f.ws, FromGeneration: 1, PublicKey: next.Recipient().String(),
		Wraps: []*workspacev1.KeyWrap{{DeviceId: "admin-mac", UserId: prep.Devices[0].UserId, Wrapped: rewrapped}}}))
	wantCode(t, err, connect.CodeFailedPrecondition) // a secret is missing
	must[workspacev1.RotateSecretsKeyResponse](t)(e.client.RotateSecretsKey(ctx, as(e, f.admin, &workspacev1.RotateSecretsKeyRequest{WorkspaceId: f.ws, FromGeneration: 1, PublicKey: next.Recipient().String(),
		Wraps:   []*workspacev1.KeyWrap{{DeviceId: "admin-mac", UserId: prep.Devices[0].UserId, Wrapped: rewrapped}},
		Secrets: []*workspacev1.SealedSecret{{Project: "shop", Name: "DB_PASSWORD", Ciphertext: resealed}}})))
	adm = first(f.admin, "admin-mac", adminDev.Recipient().String())
	k2, err := secrets.Unwrap(adm.Key.Wrapped, adminDev)
	if err != nil || adm.Key.Generation != 2 {
		t.Fatalf("after rotation: %v %v", adm.Key, err)
	}
	if v, err := secrets.Open(adm.Projects[0].Secrets["DB_PASSWORD"], k2); err != nil || string(v) != value {
		t.Fatalf("after rotation: %q %v", v, err)
	}
	if _, err := secrets.Open(adm.Projects[0].Secrets["DB_PASSWORD"], wsKey); err == nil {
		t.Error("the old key still opens the secret")
	}
	// A value encrypted to the old key is refused.
	_, err = e.client.SetSecret(ctx, as(e, f.lead, &workspacev1.SetSecretRequest{WorkspaceId: f.ws, Project: "shop", Name: "DB_PASSWORD", Generation: 1, Ciphertext: sealed}))
	wantCode(t, err, connect.CodeAborted)
}
