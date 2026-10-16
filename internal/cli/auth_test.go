package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	authv1 "github.com/amalychev/devopus/gen/devopus/auth/v1"
	"github.com/amalychev/devopus/internal/auth/authtest"
	"github.com/amalychev/devopus/internal/credentials"
	"github.com/amalychev/devopus/internal/pgtest"
)

// TestMain signs the tests in: every command but a few needs a session. The session
// is made up; nothing in these tests calls Auth with it. Tests of signing in use a
// data directory of their own and a real Auth.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "dvp-cli-")
	if err != nil {
		panic(err)
	}
	os.Setenv("DEVOPUS_HOME", home)
	os.Setenv("DEVOPUS_CREDENTIAL_STORE", credentials.StoreFile)
	far := time.Now().Add(24 * time.Hour)
	if err := credentials.Save(credentials.Session{AuthURL: "http://127.0.0.1:1", Email: "test@devopus.test", SessionID: "ses_test", IdleExpiresAt: far, ExpiresAt: far}, "dvr_test"); err != nil {
		panic(err)
	}
	code := pgtest.Run(m)
	os.RemoveAll(home)
	os.Exit(code)
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type cliRun struct {
	stdout, stderr syncBuffer
	code           chan int
}

// start runs the CLI in the background with stdin and a browser opener.
func start(stdin io.Reader, open func(string) error, args ...string) *cliRun {
	r := &cliRun{code: make(chan int, 1)}
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	go func() {
		r.code <- execute(&app{stdin: stdin, stdout: &r.stdout, stderr: &r.stderr, openURL: open}, args)
	}()
	return r
}

func (r *cliRun) wait(t *testing.T) (int, string, string) {
	t.Helper()
	select {
	case c := <-r.code:
		return c, r.stdout.String(), r.stderr.String()
	case <-time.After(60 * time.Second):
		t.Fatalf("the command did not finish:\n%s\n%s", r.stdout.String(), r.stderr.String())
	}
	return 0, "", ""
}

func cli(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	return start(strings.NewReader(stdin), nil, args...).wait(t)
}

// typing is stdin a person types into: lines arrive in order as the CLI reads them.
type typing struct {
	r     *io.PipeReader
	lines chan string
}

func newTyping() *typing {
	r, w := io.Pipe()
	ty := &typing{r: r, lines: make(chan string, 16)}
	go func() {
		for l := range ty.lines {
			if _, err := io.WriteString(w, l+"\n"); err != nil {
				return
			}
		}
	}()
	return ty
}

func (ty *typing) line(s string) { ty.lines <- s }

var (
	challengeRe = regexp.MustCompile(`name="challenge_id" value="([^"]+)"`)
	userCodeRe  = regexp.MustCompile(`enter the code ([A-Z]{4}-[A-Z]{4})`)
)

// signInPage goes through the email and code steps of a sign-in page as a person
// would; it returns the last response.
func signInPage(t *testing.T, a *authtest.Server, page string, form url.Values, email string, nth int) *http.Response {
	t.Helper()
	noRedirects := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	form.Set("step", "email")
	form.Set("email", email)
	resp, err := noRedirects.PostForm(a.URL+page, form)
	if err != nil {
		t.Error(err)
		return nil
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := challengeRe.FindSubmatch(b)
	if m == nil {
		t.Errorf("no code step:\n%s", b)
		return nil
	}
	form.Set("step", "code")
	form.Set("challenge_id", string(m[1]))
	form.Set("code", a.Inbox.Code(t, email, nth))
	resp, err = noRedirects.PostForm(a.URL+page, form)
	if err != nil {
		t.Error(err)
		return nil
	}
	return resp
}

func TestSignInFlows(t *testing.T) {
	a := authtest.Start(t)
	home := t.TempDir()
	t.Setenv("DEVOPUS_HOME", home)
	t.Setenv("DEVOPUS_AUTH_URL", a.URL)
	const email = "dev@acme.dev"

	// Without a session commands refuse with exit code 5; version does not need one.
	if code, _, errOut := cli(t, "", "validate"); code != ExitAuth || !strings.Contains(errOut, "devopus login") {
		t.Fatalf("validate signed out: %d %s", code, errOut)
	}
	if code, _, errOut := cli(t, "", "auth", "sessions"); code != ExitAuth {
		t.Fatalf("auth sessions signed out: %d %s", code, errOut)
	}
	if code, _, _ := cli(t, "", "version"); code != ExitOK {
		t.Fatalf("version: %d", code)
	}
	if code, _, _ := cli(t, "", "login", "--email", "--device"); code != ExitInvalid {
		t.Fatalf("both modes: %d", code)
	}

	// register: email, the code from the email, the only plan.
	ty := newTyping()
	run := start(ty.r, nil, "register")
	ty.line(email)
	ty.line("000000x") // a typo first
	ty.line(a.Inbox.Code(t, email, 1))
	code, out, errOut := run.wait(t)
	if code != ExitOK || !strings.Contains(out, "Account created; signed in as "+email) {
		t.Fatalf("register: %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"Plan: Free (Self-hosted and Team plans are coming)", "Code sent to " + email + " (valid 10 minutes)", "The code is wrong"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("register stderr lacks %q:\n%s", want, errOut)
		}
	}
	if fi, err := os.Stat(filepath.Join(home, "refresh-token")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("refresh token file: %v %v", fi, err)
	}

	code, out, errOut = cli(t, "", "whoami")
	if code != ExitOK || !strings.Contains(out, email) || !strings.Contains(out, "free") {
		t.Fatalf("whoami: %d\n%s\n%s", code, out, errOut)
	}
	// The command that was refused works now.
	if code, _, errOut := cli(t, "", "schema"); code != ExitOK {
		t.Fatalf("schema signed in: %d %s", code, errOut)
	}

	// Browser sign-in: the page sends the code back to the CLI on 127.0.0.1.
	browserDone := make(chan struct{})
	openBrowser := func(link string) error {
		go func() {
			defer close(browserDone)
			u, _ := url.Parse(link)
			resp := signInPage(t, a, "/authorize", u.Query(), email, 2)
			if resp == nil {
				return
			}
			resp.Body.Close()
			cb, err := http.Get(resp.Header.Get("Location"))
			if err != nil {
				t.Error(err)
				return
			}
			cb.Body.Close()
		}()
		return nil
	}
	run = start(nil, openBrowser, "login")
	code, out, errOut = run.wait(t)
	<-browserDone
	if code != ExitOK || !strings.Contains(out, "Signed in as "+email) {
		t.Fatalf("browser login: %d\n%s\n%s", code, out, errOut)
	}
	// The new sign-in replaced the old session: one session, this device's.
	code, out, _ = cli(t, "", "auth", "sessions")
	if code != ExitOK || strings.Count(out, "ses_") != 1 || !strings.Contains(out, "(this device)") {
		t.Fatalf("sessions after browser login: %d\n%s", code, out)
	}

	// Another device signs in; this one revokes it.
	ctx := context.Background()
	c := credentials.Client(a.URL)
	st, err := c.StartEmailSignIn(ctx, connect.NewRequest(&authv1.StartEmailSignInRequest{Email: email, Purpose: authv1.Purpose_PURPOSE_SIGN_IN}))
	if err != nil {
		t.Fatal(err)
	}
	other, err := c.CompleteEmailSignIn(ctx, connect.NewRequest(&authv1.CompleteEmailSignInRequest{ChallengeId: st.Msg.ChallengeId, Code: a.Inbox.Code(t, email, 3), DeviceName: "other"}))
	if err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := cli(t, "", "auth", "revoke", other.Msg.Tokens.Session.Id); code != ExitOK || !strings.Contains(out, "revoked") {
		t.Fatalf("revoke: %d %s %s", code, out, errOut)
	}
	if _, err := c.Refresh(ctx, connect.NewRequest(&authv1.RefreshRequest{RefreshToken: other.Msg.Tokens.RefreshToken})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked session refreshes: %v", err)
	}

	// A copy of the refresh token used after the device rotated it: Auth closes the
	// session, and this device finds out on its next call.
	stolen, err := credentials.StoredRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := cli(t, "", "whoami"); code != ExitOK {
		t.Fatalf("whoami: %d %s", code, errOut)
	}
	if _, err := c.Refresh(ctx, connect.NewRequest(&authv1.RefreshRequest{RefreshToken: stolen})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("stolen token: %v", err)
	}
	code, _, errOut = cli(t, "", "whoami")
	if code != ExitAuth || !strings.Contains(errOut, "used from somewhere else") {
		t.Fatalf("whoami after reuse: %d %s", code, errOut)
	}
	if code, _, _ := cli(t, "", "schema"); code != ExitAuth {
		t.Fatalf("schema after the session was closed: %d", code)
	}

	// Email sign-in in the terminal, then sign-out.
	ty = newTyping()
	run = start(ty.r, nil, "login", "--email")
	ty.line(email)
	ty.line(a.Inbox.Code(t, email, 4))
	if code, out, errOut := run.wait(t); code != ExitOK || !strings.Contains(out, "Signed in as") {
		t.Fatalf("login --email: %d\n%s\n%s", code, out, errOut)
	}
	rt, _ := credentials.StoredRefreshToken()
	if code, out, _ := cli(t, "", "logout"); code != ExitOK || !strings.Contains(out, "Signed out "+email) {
		t.Fatalf("logout: %d %s", code, out)
	}
	if _, err := c.Refresh(ctx, connect.NewRequest(&authv1.RefreshRequest{RefreshToken: rt})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("token after logout: %v", err)
	}
	if code, out, _ := cli(t, "", "logout"); code != ExitOK || !strings.Contains(out, "Not signed in") {
		t.Fatalf("second logout: %d %s", code, out)
	}

	// Signing in to an email without an account points to register.
	ty = newTyping()
	run = start(ty.r, nil, "login", "--email")
	ty.line("nobody@acme.dev")
	ty.line(a.Inbox.Code(t, "nobody@acme.dev", 1))
	if code, _, errOut := run.wait(t); code != ExitInvalid || !strings.Contains(errOut, "devopus register") {
		t.Fatalf("login without an account: %d %s", code, errOut)
	}
}

func TestDeviceLogin(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the device flow's poll interval")
	}
	a := authtest.Start(t)
	t.Setenv("DEVOPUS_HOME", t.TempDir())
	t.Setenv("DEVOPUS_AUTH_URL", a.URL)
	const email = "ops@acme.dev"

	ty := newTyping()
	run := start(ty.r, nil, "register", "--plan", "free")
	ty.line(email)
	ty.line(a.Inbox.Code(t, email, 1))
	if code, _, errOut := run.wait(t); code != ExitOK {
		t.Fatalf("register: %d %s", code, errOut)
	}
	if code, _, errOut := cli(t, "", "register", "--plan", "team", "--email", email); code != ExitInvalid || !strings.Contains(errOut, "coming") {
		t.Fatalf("unavailable plan: %d %s", code, errOut)
	}

	run = start(nil, nil, "login", "--device")
	var userCode string
	for deadline := time.Now().Add(10 * time.Second); userCode == ""; {
		if m := userCodeRe.FindStringSubmatch(run.stderr.String()); m != nil {
			userCode = m[1]
		} else if time.Now().After(deadline) {
			t.Fatalf("no user code:\n%s", run.stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	resp := signInPage(t, a, "/device", url.Values{"user_code": {userCode}}, email, 2)
	if resp == nil {
		t.FailNow()
	}
	resp.Body.Close()
	code, out, errOut := run.wait(t)
	if code != ExitOK || !strings.Contains(out, "Signed in as "+email) {
		t.Fatalf("login --device: %d\n%s\n%s", code, out, errOut)
	}
}
