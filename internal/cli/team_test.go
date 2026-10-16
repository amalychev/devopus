package cli

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/amalychev/devopus/internal/auth/authtest"
	"github.com/amalychev/devopus/internal/auth/token"
	"github.com/amalychev/devopus/internal/pgtest"
	"github.com/amalychev/devopus/internal/server"
	"github.com/amalychev/devopus/internal/server/store"
)

var inviteCodeRe = regexp.MustCompile(`devopus invite accept (dvi_\S+)`)

// TestTeam is R5's scenario through the CLI against a real Auth and workspace server:
// the admin creates a workspace and invites a teamlead, the teamlead a developer; the
// developer without the permission is refused.
func TestTeam(t *testing.T) {
	a := authtest.Start(t)
	pool := pgtest.Pool(t)
	if err := store.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	keys := &token.Remote{URL: a.URL + "/.well-known/jwks.json"}
	svc, err := server.New(pool, server.Config{Keys: keys.Key, AuthIssuer: a.URL, Mailer: a.Inbox})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(svc.Handler())
	t.Cleanup(srv.Close)
	t.Setenv("DEVOPUS_AUTH_URL", a.URL)
	t.Setenv("DEVOPUS_SERVER_URL", srv.URL)

	// Each person has a device of their own: a data directory.
	homes := map[string]string{}
	as := func(email string) {
		if homes[email] == "" {
			homes[email] = t.TempDir()
		}
		t.Setenv("DEVOPUS_HOME", homes[email])
	}
	ok := func(args ...string) string {
		t.Helper()
		code, out, errOut := cli(t, "", args...)
		if code != ExitOK {
			t.Fatalf("devopus %s: exit %d\n%s\n%s", strings.Join(args, " "), code, out, errOut)
		}
		return out
	}
	register := func(email string) {
		t.Helper()
		as(email)
		ty := newTyping()
		run := start(ty.r, nil, "register", "--email", email)
		ty.line(a.Inbox.Code(t, email, a.Inbox.Count(email)+1))
		if code, out, errOut := run.wait(t); code != ExitOK {
			t.Fatalf("register %s: %d\n%s\n%s", email, code, out, errOut)
		}
	}
	codeIn := func(out string) string {
		t.Helper()
		m := inviteCodeRe.FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("no invite code in:\n%s", out)
		}
		return m[1]
	}
	const admin, lead, dev = "admin@acme.dev", "lead@acme.dev", "dev@acme.dev"

	as(admin)
	if code, _, _ := cli(t, "", "workspace", "list"); code != ExitAuth {
		t.Fatalf("workspace list signed out: %d", code)
	}
	register(admin)
	if out := ok("workspace", "list"); !strings.Contains(out, "No workspaces") {
		t.Errorf("empty list:\n%s", out)
	}
	ok("workspace", "create", "Acme")
	ok("project", "create", "shop")
	leadCode := codeIn(ok("invite", "create", lead, "--role", "teamlead", "--project", "shop"))

	register(lead)
	if code, _, errOut := cli(t, "", "project", "list"); code != ExitInvalid || !strings.Contains(errOut, "no workspace") {
		t.Fatalf("project list before joining: %d %s", code, errOut)
	}
	if out := ok("invite", "accept", leadCode); !strings.Contains(out, "joined Acme as teamlead") {
		t.Errorf("accept:\n%s", out)
	}
	if out := ok("workspace", "list"); !strings.Contains(out, "* ") || !strings.Contains(out, "teamlead") || !strings.Contains(out, admin) {
		t.Errorf("teamlead's workspaces:\n%s", out)
	}
	code, _, errOut := cli(t, "", "invite", "create", "x@acme.dev", "--role", "teamlead", "--project", "shop")
	if code != ExitAuth || !strings.Contains(errOut, "teamleads are invited by the admin") {
		t.Fatalf("teamlead invites a teamlead: %d %s", code, errOut)
	}
	devCode := codeIn(ok("invite", "create", dev, "--role", "developer", "--project", "shop"))

	register(dev)
	ok("invite", "accept", devCode)
	code, _, errOut = cli(t, "", "invite", "create", "y@acme.dev", "--role", "developer", "--project", "shop")
	if code != ExitAuth || !strings.Contains(errOut, "members.invite") {
		t.Fatalf("developer invites: %d %s", code, errOut)
	}
	if out := ok("member", "list"); !strings.Contains(out, "shop (teamlead)") || !strings.Contains(out, "shop (developer)") {
		t.Errorf("members:\n%s", out)
	}

	as(lead)
	ok("member", "grant", dev, "services.manage", "--project", "shop")
	if code, _, errOut := cli(t, "", "member", "grant", dev, "members.manage", "--project", "shop"); code != ExitAuth {
		t.Errorf("grant members.manage: %d %s", code, errOut)
	}

	as(dev)
	if out := ok("workspace", "show"); !strings.Contains(out, "services.manage") {
		t.Errorf("developer's permissions:\n%s", out)
	}

	as(admin)
	if out := ok("workspace", "audit"); !strings.Contains(out, "permission.granted") || !strings.Contains(out, "member.joined") {
		t.Errorf("audit:\n%s", out)
	}
	ok("member", "remove", dev)

	as(dev)
	if code, _, errOut := cli(t, "", "workspace", "show"); code != ExitAuth || !strings.Contains(errOut, "not a member") {
		t.Fatalf("removed developer: %d %s", code, errOut)
	}
	// The list drops the workspace from the developer's registry.
	if out := ok("workspace", "list"); !strings.Contains(out, "No workspaces") {
		t.Errorf("removed developer's list:\n%s", out)
	}
	if code, _, errOut := cli(t, "", "workspace", "show"); code != ExitInvalid || !strings.Contains(errOut, "no workspace") {
		t.Errorf("current workspace after removal: %d %s", code, errOut)
	}
}
