package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"

	syncv1 "github.com/amalychev/devopus/gen/devopus/sync/v1"
	"github.com/amalychev/devopus/gen/devopus/sync/v1/syncv1connect"
	workspacev1 "github.com/amalychev/devopus/gen/devopus/workspace/v1"
)

func TestServers(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	f := e.fixture(t)
	must[workspacev1.CreateProjectResponse](t)(e.client.CreateProject(ctx, as(e, f.admin, &workspacev1.CreateProjectRequest{WorkspaceId: f.ws, Name: "blog"})))

	// The teamlead adds a server for their project; not one of no project, not for
	// another's; the developer adds none.
	if _, err := e.client.CreateServer(ctx, as(e, f.lead, &workspacev1.CreateServerRequest{WorkspaceId: f.ws, Name: "stage"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("lead, no project: %v", err)
	}
	if _, err := e.client.CreateServer(ctx, as(e, f.lead, &workspacev1.CreateServerRequest{WorkspaceId: f.ws, Name: "stage", Projects: []string{"blog"}})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("lead, another's project: %v", err)
	}
	if _, err := e.client.CreateServer(ctx, as(e, f.dev, &workspacev1.CreateServerRequest{WorkspaceId: f.ws, Name: "stage", Projects: []string{"shop"}})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("developer: %v", err)
	}
	created := must[workspacev1.CreateServerResponse](t)(e.client.CreateServer(ctx, as(e, f.lead, &workspacev1.CreateServerRequest{WorkspaceId: f.ws, Name: "stage", Projects: []string{"shop"}})))
	if _, err := e.client.CreateServer(ctx, as(e, f.lead, &workspacev1.CreateServerRequest{WorkspaceId: f.ws, Name: "stage", Projects: []string{"shop"}})); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("same name: %v", err)
	}

	// The code works once.
	joined := must[workspacev1.JoinServerResponse](t)(e.client.JoinServer(ctx, connect.NewRequest(&workspacev1.JoinServerRequest{Code: created.JoinCode, DeviceId: "stage-1", DeviceName: "stage-1"})))
	if joined.WorkspaceId != f.ws || joined.Name != "stage" || len(joined.Token) < 30 {
		t.Fatalf("join: %v", joined)
	}
	if _, err := e.client.JoinServer(ctx, connect.NewRequest(&workspacev1.JoinServerRequest{Code: created.JoinCode})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("the code again: %v", err)
	}
	bearer := func(msg any, tok string) *connect.Request[syncv1.WatchRequest] {
		r := connect.NewRequest(msg.(*syncv1.WatchRequest))
		r.Header().Set("Authorization", "Bearer "+tok)
		return r
	}

	// With its token the server gets the settings of its projects, and nothing else.
	sc := syncv1connect.NewSyncServiceClient(http.DefaultClient, e.url)
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	st, err := sc.Watch(wctx, bearer(&syncv1.WatchRequest{WorkspaceId: f.ws, Device: &syncv1.Device{Id: "stage-1", Name: "stage-1"}}, joined.Token))
	if err != nil || !st.Receive() {
		t.Fatalf("watch: %v %v", err, st.Err())
	}
	if ps := st.Msg().Projects; len(ps) != 1 || ps[0].Name != "shop" {
		t.Errorf("the server's projects: %v", ps)
	}
	cancel()
	list := connect.NewRequest(&workspacev1.ListProjectsRequest{WorkspaceId: f.ws})
	list.Header().Set("Authorization", "Bearer "+joined.Token)
	if _, err := e.client.ListProjects(ctx, list); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("the server's token reads projects: %v", err)
	}
	other, err := sc.Watch(ctx, bearer(&syncv1.WatchRequest{WorkspaceId: "wsp_other", Device: &syncv1.Device{Id: "stage-1"}}, joined.Token))
	if err == nil && other.Receive() {
		t.Error("the server's token works in another workspace")
	} else if connect.CodeOf(other.Err()) != connect.CodePermissionDenied {
		t.Errorf("another workspace: %v", other.Err())
	}

	// People see it; it is not among the members.
	servers := must[workspacev1.ListServersResponse](t)(e.client.ListServers(ctx, as(e, f.dev, &workspacev1.ListServersRequest{WorkspaceId: f.ws}))).Servers
	if len(servers) != 1 || !servers[0].Joined || servers[0].LastSeen == nil || servers[0].Projects[0] != "shop" {
		t.Errorf("servers: %v", servers)
	}
	members := must[workspacev1.ListMembersResponse](t)(e.client.ListMembers(ctx, as(e, f.admin, &workspacev1.ListMembersRequest{WorkspaceId: f.ws}))).Members
	if len(members) != 3 {
		t.Errorf("members: %v", members)
	}

	// The admin gives it blog too; then removes it: its token stops working.
	if _, err := e.client.SetServerProjects(ctx, as(e, f.lead, &workspacev1.SetServerProjectsRequest{WorkspaceId: f.ws, Name: "stage", Projects: []string{"shop", "blog"}})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("lead adds another's project: %v", err)
	}
	must[workspacev1.SetServerProjectsResponse](t)(e.client.SetServerProjects(ctx, as(e, f.admin, &workspacev1.SetServerProjectsRequest{WorkspaceId: f.ws, Name: "stage", Projects: []string{"shop", "blog"}})))
	must[workspacev1.RemoveServerResponse](t)(e.client.RemoveServer(ctx, as(e, f.admin, &workspacev1.RemoveServerRequest{WorkspaceId: f.ws, Name: "stage"})))
	st, err = sc.Watch(ctx, bearer(&syncv1.WatchRequest{WorkspaceId: f.ws, Device: &syncv1.Device{Id: "stage-1"}}, joined.Token))
	if err == nil && st.Receive() {
		t.Error("a removed server still gets settings")
	}
	if connect.CodeOf(st.Err()) != connect.CodeUnauthenticated {
		t.Errorf("removed server: %v", st.Err())
	}
}
