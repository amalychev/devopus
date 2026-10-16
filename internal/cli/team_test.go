package cli

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/amalychev/devopus/internal/auth/authtest"
	"github.com/amalychev/devopus/internal/auth/token"
	"github.com/amalychev/devopus/internal/pgtest"
	"github.com/amalychev/devopus/internal/server"
	"github.com/amalychev/devopus/internal/server/store"
	"github.com/amalychev/devopus/internal/spec"
)

var inviteCodeRe = regexp.MustCompile(`devopus invite accept (dvi_\S+)`)

// TestTeam is R5's scenario through the CLI against a real Auth and workspace server:
// the admin creates a workspace and invites a teamlead, the teamlead a developer; the
// developer without the permission is refused.
// startTeam runs a real Auth and workspace server for the CLI.
func startTeam(t *testing.T) *authtest.Server {
	t.Helper()
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
	return a
}

// signUp registers an email on a device of its own (a data directory).
func signUp(t *testing.T, a *authtest.Server, email string) {
	t.Helper()
	t.Setenv("DEVOPUS_HOME", t.TempDir())
	ty := newTyping()
	run := start(ty.r, nil, "register", "--email", email)
	ty.line(a.Inbox.Code(t, email, a.Inbox.Count(email)+1))
	if code, out, errOut := run.wait(t); code != ExitOK {
		t.Fatalf("register %s: %d\n%s\n%s", email, code, out, errOut)
	}
}

func ok(t *testing.T, args ...string) string {
	t.Helper()
	code, out, errOut := cli(t, "", args...)
	if code != ExitOK {
		t.Fatalf("devopus %s: exit %d\n%s\n%s", strings.Join(args, " "), code, out, errOut)
	}
	return out
}

func TestTeam(t *testing.T) {
	a := startTeam(t)

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

// TestProjects is R6's scenario: the demo workspace in one command, its settings
// shown, changed, and rolled back; a project imported from docker-compose.yml.
func TestProjects(t *testing.T) {
	a := startTeam(t)
	signUp(t, a, "admin@acme.dev")

	if out := ok(t, "workspace", "demo"); !strings.Contains(out, "Workspace demo with project demo: 3 applications, 5 services") {
		t.Fatalf("demo:\n%s", out)
	}
	out := ok(t, "project", "show", "demo")
	for _, want := range []string{"version 1 · https://demo.localhost", "admin ./admin main /admin php", "php php 8.3 runtime admin", "db postgres 17.6 container api"} {
		if !strings.Contains(squash(out), want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}

	ok(t, "service", "set", "demo", "php", "8.4")
	ok(t, "app", "set", "demo", "admin", "--route", "/admin", "--route", "/backoffice")
	ok(t, "service", "add", "demo", "redis", "7.4", "--name", "cache", "--apps", "api")
	ok(t, "service", "link", "demo", "cache", "web")
	if code, _, errOut := cli(t, "", "service", "add", "demo", "redis", "--name", "cache2"); code != ExitInvalid || !strings.Contains(errOut, "name the redis version") {
		t.Errorf("unpinned service: %d %s", code, errOut)
	}
	if code, _, errOut := cli(t, "", "app", "add", "demo", "worker"); code != ExitInvalid || !strings.Contains(errOut, "where is the code") {
		t.Errorf("app without code: %d %s", code, errOut)
	}
	yamlOut := ok(t, "project", "config", "show", "demo", "-o", "yaml")
	cfg, err := spec.ParseYAML([]byte(yamlOut))
	if err != nil {
		t.Fatalf("config -o yaml: %v\n%s", err, yamlOut)
	}
	if cfg.Services["php"].Version != "8.4" || len(cfg.Services["cache"].Apps) != 2 || len(cfg.Apps["admin"].Routes) != 2 {
		t.Fatalf("after the changes:\n%s", yamlOut)
	}
	hist := ok(t, "project", "config", "history", "demo")
	for _, want := range []string{"5  ", "service set cache (redis 7.4)", "service add cache (redis 7.4)", "app set admin", "service set php (php 8.4)", "1  ", "created from an import", "admin@acme.dev"} {
		if !strings.Contains(hist, want) {
			t.Errorf("history lacks %q:\n%s", want, hist)
		}
	}

	// Back to the demo as it was created: the rollback is version 6.
	if out := ok(t, "project", "config", "rollback", "demo", "1"); !strings.Contains(out, "back to version 1 (settings version 6)") {
		t.Errorf("rollback:\n%s", out)
	}
	if out := ok(t, "project", "config", "show", "demo"); !strings.Contains(out, "version 6") || strings.Contains(out, "cache") || !strings.Contains(squash(out), "php php 8.3") {
		t.Errorf("after rollback:\n%s", out)
	}
	if out := ok(t, "project", "config", "show", "demo", "--version", "5"); !strings.Contains(out, "cache") || !strings.Contains(out, "the current one is 6") {
		t.Errorf("version 5:\n%s", out)
	}
	ok(t, "app", "rm", "demo", "admin")
	if out := ok(t, "service", "list", "demo"); strings.Contains(out, "admin") {
		t.Errorf("services still attached to a removed app:\n%s", out)
	}

	// A project from docker-compose.yml: no secret values leave the machine.
	dir := t.TempDir()
	write(t, filepath.Join(dir, "docker-compose.yml"), composeFile)
	write(t, filepath.Join(dir, "web", "Dockerfile"), "FROM scratch\n")
	code, out, errOut := cli(t, "", "project", "create", "shop", "--from-compose", filepath.Join(dir, "docker-compose.yml"), "--no-pin")
	if code != ExitOK || !strings.Contains(out, "Project shop created") || !strings.Contains(errOut, "Secrets POSTGRES_PASSWORD, SESSION_SECRET are encrypted here and stored in shop") ||
		!strings.Contains(errOut, "AGE-SECRET-KEY-1") {
		t.Fatalf("import: %d\n%s\n%s", code, out, errOut)
	}
	if y := ok(t, "project", "config", "show", "shop", "-o", "yaml"); strings.Contains(y, "devpass") || strings.Contains(y, "s3cr3t") || !strings.Contains(y, "${{ secrets.POSTGRES_PASSWORD }}") {
		t.Errorf("imported settings:\n%s", y)
	}
	if out := ok(t, "secrets", "list", "shop"); !strings.Contains(out, "POSTGRES_PASSWORD") || !strings.Contains(out, "SESSION_SECRET") || strings.Contains(out, "devpass") {
		t.Errorf("secrets of the import:\n%s", out)
	}
	ok(t, "project", "rm", "shop", "--yes")
	if out := ok(t, "project", "list"); strings.Contains(out, "shop") {
		t.Errorf("deleted project listed:\n%s", out)
	}
}

// squash collapses the spaces of tables.
func squash(s string) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		lines = append(lines, strings.Join(strings.Fields(l), " "))
	}
	return strings.Join(lines, "\n")
}

var recoveryRe = regexp.MustCompile(`AGE-SECRET-KEY-1[0-9A-Z]+`)

func TestSecretsCommands(t *testing.T) {
	a := startTeam(t)
	signUp(t, a, "admin@acme.dev")
	laptop := os.Getenv("DEVOPUS_HOME")
	ok(t, "workspace", "create", "acme")
	ok(t, "project", "create", "shop")
	devCode := inviteCodeRe.FindStringSubmatch(ok(t, "invite", "create", "dev@acme.dev", "--role", "developer", "--project", "shop"))[1]

	// The first secret makes the workspace's key and shows its recovery key once.
	code, out, errOut := cli(t, "", "secrets", "set", "shop", "DB_PASSWORD", "hunter2")
	recovery := recoveryRe.FindString(errOut)
	if code != ExitOK || recovery == "" || !strings.Contains(out, "Secret DB_PASSWORD of shop is set") {
		t.Fatalf("first secret: %d\n%s\n%s", code, out, errOut)
	}
	if _, _, errOut := cli(t, "", "secrets", "set", "shop", "API_TOKEN", "t0k3n"); strings.Contains(errOut, "AGE-SECRET-KEY") {
		t.Error("the recovery key shown twice")
	}
	if code, _, errOut := cli(t, "piped-value\n", "secrets", "set", "shop", "FROM_STDIN"); code != ExitOK {
		t.Fatalf("value from stdin: %d %s", code, errOut)
	}
	if out := ok(t, "secrets", "list", "shop"); !strings.Contains(out, "API_TOKEN") || !strings.Contains(out, "FROM_STDIN") || strings.Contains(out, "hunter2") {
		t.Errorf("list:\n%s", out)
	}
	ok(t, "secrets", "rm", "shop", "FROM_STDIN")

	// A developer reads names but sets nothing.
	signUp(t, a, "dev@acme.dev")
	ok(t, "invite", "accept", devCode)
	if out := ok(t, "secrets", "list", "shop"); !strings.Contains(out, "DB_PASSWORD") {
		t.Errorf("developer's list:\n%s", out)
	}
	if code, _, errOut := cli(t, "", "secrets", "set", "shop", "X", "y"); code != ExitAuth || !strings.Contains(errOut, "secrets.manage") {
		t.Errorf("developer sets: %d %s", code, errOut)
	}

	// The admin on a new machine: the recovery key puts the workspace's key there.
	t.Setenv("DEVOPUS_HOME", t.TempDir())
	ty := newTyping()
	run := start(ty.r, nil, "login", "--email")
	ty.line("admin@acme.dev")
	ty.line(a.Inbox.Code(t, "admin@acme.dev", a.Inbox.Count("admin@acme.dev")+1))
	if code, out, errOut := run.wait(t); code != ExitOK {
		t.Fatalf("login on the new machine: %d %s %s", code, out, errOut)
	}
	ok(t, "workspace", "use", "acme")
	if code, _, errOut := cli(t, "", "secrets", "rotate"); code != ExitInvalid || !strings.Contains(errOut, "secrets recover") {
		t.Errorf("rotate without the key: %d %s", code, errOut)
	}
	if code, _, _ := cli(t, "nonsense\n", "secrets", "recover"); code != ExitInvalid {
		t.Errorf("a bad recovery key: %d", code)
	}
	code, out, errOut = cli(t, recovery+"\n", "secrets", "recover")
	if code != ExitOK || !strings.Contains(out, "is on this machine again") {
		t.Fatalf("recover: %d %s %s", code, out, errOut)
	}

	// Rotation from the laptop: every secret re-encrypted, a new recovery key; the old
	// one no longer recovers.
	t.Setenv("DEVOPUS_HOME", laptop)
	code, out, errOut = cli(t, "", "secrets", "rotate")
	if code != ExitOK || !strings.Contains(out, "2 secrets re-encrypted") || recoveryRe.FindString(errOut) == "" || recoveryRe.FindString(errOut) == recovery {
		t.Fatalf("rotate: %d\n%s\n%s", code, out, errOut)
	}
	if code, _, errOut := cli(t, recovery+"\n", "secrets", "recover"); code != ExitInvalid || !strings.Contains(errOut, "not the current key") {
		t.Errorf("old recovery key: %d %s", code, errOut)
	}
}
