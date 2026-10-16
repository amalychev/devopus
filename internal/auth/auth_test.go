package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	authv1 "github.com/amalychev/devopus/gen/devopus/auth/v1"
	"github.com/amalychev/devopus/gen/devopus/auth/v1/authv1connect"
	"github.com/amalychev/devopus/internal/auth/mail"
	"github.com/amalychev/devopus/internal/auth/store"
	"github.com/amalychev/devopus/internal/auth/token"
	"github.com/amalychev/devopus/internal/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// inbox keeps the emails the service sends.
type inbox struct {
	mu   sync.Mutex
	msgs []mail.Message
}

func (b *inbox) Send(_ context.Context, m mail.Message) error {
	b.mu.Lock()
	b.msgs = append(b.msgs, m)
	b.mu.Unlock()
	return nil
}

var codeRe = regexp.MustCompile(`\b\d{6}\b`)

// code is the code in the last email to an address.
func (b *inbox) code(t *testing.T, to string) string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.msgs) - 1; i >= 0; i-- {
		if b.msgs[i].To == to {
			return codeRe.FindString(b.msgs[i].Text)
		}
	}
	t.Fatalf("no email to %s", to)
	return ""
}

type env struct {
	svc    *Service
	srv    *httptest.Server
	client authv1connect.AuthServiceClient
	clock  *clock
	inbox  *inbox
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := pgtest.Pool(t)
	ctx := context.Background()
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	e := &env{clock: &clock{now: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}, inbox: &inbox{}}
	e.srv = httptest.NewUnstartedServer(nil)
	e.srv.Start()
	t.Cleanup(e.srv.Close)
	svc, err := New(ctx, pool, Config{Issuer: e.srv.URL, Mailer: e.inbox, Now: e.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	e.srv.Config.Handler = svc.Handler()
	e.client = authv1connect.NewAuthServiceClient(http.DefaultClient, e.srv.URL)
	return e
}

func (e *env) signIn(t *testing.T, email string, purpose authv1.Purpose) (*authv1.CompleteEmailSignInResponse, error) {
	t.Helper()
	ctx := context.Background()
	st, err := e.client.StartEmailSignIn(ctx, connect.NewRequest(&authv1.StartEmailSignInRequest{Email: email, Purpose: purpose, DeviceId: "dev-1"}))
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.client.CompleteEmailSignIn(ctx, connect.NewRequest(&authv1.CompleteEmailSignInRequest{
		ChallengeId: st.Msg.ChallengeId, Code: e.inbox.code(t, strings.ToLower(email)), Plan: authv1.Plan_PLAN_FREE, DeviceName: "laptop",
	}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func (e *env) register(t *testing.T, email string) *authv1.CompleteEmailSignInResponse {
	t.Helper()
	res, err := e.signIn(t, email, authv1.Purpose_PURPOSE_REGISTER)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (e *env) refresh(rt string) (*authv1.RefreshResponse, error) {
	res, err := e.client.Refresh(context.Background(), connect.NewRequest(&authv1.RefreshRequest{RefreshToken: rt}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func withToken[T any](msg *T, access string) *connect.Request[T] {
	r := connect.NewRequest(msg)
	r.Header().Set("Authorization", "Bearer "+access)
	return r
}

func wantCode(t *testing.T, err error, code connect.Code) {
	t.Helper()
	if connect.CodeOf(err) != code {
		t.Fatalf("want %v, got %v", code, err)
	}
}

func TestRegisterRefreshAndReuse(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	reg := e.register(t, "Admin@Acme.dev")
	if !reg.Created || reg.User.Email != "admin@acme.dev" || reg.User.Plan != authv1.Plan_PLAN_FREE {
		t.Fatalf("register = %+v", reg)
	}
	claims, err := e.svc.Verify(reg.Tokens.AccessToken)
	if err != nil || claims.Subject != reg.User.Id || claims.Session != reg.Tokens.Session.Id || claims.Email != "admin@acme.dev" || claims.Audience != token.DefaultAudience {
		t.Fatalf("access token: %+v %v", claims, err)
	}
	if got := reg.Tokens.AccessExpiresAt.AsTime().Sub(e.clock.Now()); got != AccessTTL {
		t.Errorf("access lifetime %v", got)
	}

	me, err := e.client.GetMe(ctx, withToken(&authv1.GetMeRequest{}, reg.Tokens.AccessToken))
	if err != nil || me.Msg.User.Email != "admin@acme.dev" || !me.Msg.Session.Current || me.Msg.Session.DeviceName != "laptop" {
		t.Fatalf("GetMe = %v %v", me, err)
	}

	// Registering again with the same email signs in.
	again := e.register(t, "admin@acme.dev")
	if again.Created || again.User.Id != reg.User.Id {
		t.Errorf("second register = %+v", again)
	}

	// Refresh rotates the refresh token and moves the idle window.
	e.clock.Add(time.Hour)
	r1, err := e.refresh(reg.Tokens.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Tokens.RefreshToken == reg.Tokens.RefreshToken || r1.Tokens.Session.Id != reg.Tokens.Session.Id {
		t.Fatalf("refresh did not rotate: %+v", r1.Tokens)
	}
	if got := r1.Tokens.Session.IdleExpiresAt.AsTime(); !got.Equal(e.clock.Now().Add(SessionIdle)) {
		t.Errorf("idle expiry %v", got)
	}

	// The old refresh token used again: the session is revoked, the new token dies too.
	_, err = e.refresh(reg.Tokens.RefreshToken)
	wantCode(t, err, connect.CodeUnauthenticated)
	if !strings.Contains(err.Error(), "used from somewhere else") {
		t.Errorf("reuse message: %v", err)
	}
	_, err = e.refresh(r1.Tokens.RefreshToken)
	wantCode(t, err, connect.CodeUnauthenticated)
	// Access tokens of the revoked session no longer work with Auth.
	_, err = e.client.GetMe(ctx, withToken(&authv1.GetMeRequest{}, r1.Tokens.AccessToken))
	wantCode(t, err, connect.CodeUnauthenticated)
	// The other session (the second register) lives on.
	if _, err := e.refresh(again.Tokens.RefreshToken); err != nil {
		t.Errorf("other session: %v", err)
	}

	events, err := e.svc.q.UserEvents(ctx, reg.User.Id)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
	}
	if got := strings.Join(kinds, ","); got != "registered,plan_chosen,signed_in,signed_in,refresh_token_reused" {
		t.Errorf("security log: %s", got)
	}
}

func TestSignInSessionsAndRevoke(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// Sign-in to an email without an account: the same answer to start, NOT_FOUND after the code.
	_, err := e.signIn(t, "nobody@acme.dev", authv1.Purpose_PURPOSE_SIGN_IN)
	wantCode(t, err, connect.CodeNotFound)
	if !strings.Contains(err.Error(), "devopus register") {
		t.Errorf("message: %v", err)
	}

	reg := e.register(t, "dev@acme.dev")
	other, err := e.signIn(t, "dev@acme.dev", authv1.Purpose_PURPOSE_SIGN_IN)
	if err != nil {
		t.Fatal(err)
	}
	list, err := e.client.ListSessions(ctx, withToken(&authv1.ListSessionsRequest{}, reg.Tokens.AccessToken))
	if err != nil || len(list.Msg.Sessions) != 2 {
		t.Fatalf("sessions = %v %v", list, err)
	}

	// Someone else's session is "not found".
	stranger := e.register(t, "stranger@acme.dev")
	_, err = e.client.RevokeSession(ctx, withToken(&authv1.RevokeSessionRequest{SessionId: other.Tokens.Session.Id}, stranger.Tokens.AccessToken))
	wantCode(t, err, connect.CodeNotFound)

	if _, err := e.client.RevokeSession(ctx, withToken(&authv1.RevokeSessionRequest{SessionId: other.Tokens.Session.Id}, reg.Tokens.AccessToken)); err != nil {
		t.Fatal(err)
	}
	_, err = e.refresh(other.Tokens.RefreshToken)
	wantCode(t, err, connect.CodeUnauthenticated)
	list, _ = e.client.ListSessions(ctx, withToken(&authv1.ListSessionsRequest{}, reg.Tokens.AccessToken))
	if len(list.Msg.Sessions) != 1 || !list.Msg.Sessions[0].Current {
		t.Errorf("after revoke: %v", list.Msg.Sessions)
	}

	// Sign-out ends the session of the refresh token.
	if _, err := e.client.SignOut(ctx, connect.NewRequest(&authv1.SignOutRequest{RefreshToken: reg.Tokens.RefreshToken})); err != nil {
		t.Fatal(err)
	}
	_, err = e.refresh(reg.Tokens.RefreshToken)
	wantCode(t, err, connect.CodeUnauthenticated)
	// Signing out twice is fine.
	if _, err := e.client.SignOut(ctx, connect.NewRequest(&authv1.SignOutRequest{RefreshToken: reg.Tokens.RefreshToken})); err != nil {
		t.Errorf("second sign-out: %v", err)
	}
}

func TestCodesAttemptsExpiryAndLimits(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	start := func(email, device string) (string, error) {
		r, err := e.client.StartEmailSignIn(ctx, connect.NewRequest(&authv1.StartEmailSignInRequest{Email: email, Purpose: authv1.Purpose_PURPOSE_REGISTER, DeviceId: device}))
		if err != nil {
			return "", err
		}
		return r.Msg.ChallengeId, nil
	}
	complete := func(id, code string) error {
		_, err := e.client.CompleteEmailSignIn(ctx, connect.NewRequest(&authv1.CompleteEmailSignInRequest{ChallengeId: id, Code: code, Plan: authv1.Plan_PLAN_FREE}))
		return err
	}

	_, err := start("not-an-email", "d")
	wantCode(t, err, connect.CodeInvalidArgument)

	// Five wrong codes kill the challenge; the right one no longer works.
	id, err := start("a@acme.dev", "d1")
	if err != nil {
		t.Fatal(err)
	}
	good := e.inbox.code(t, "a@acme.dev")
	bad := "000000"
	if bad == good {
		bad = "111111"
	}
	for i := 1; i <= CodeAttempts; i++ {
		err := complete(id, bad)
		if i < CodeAttempts {
			wantCode(t, err, connect.CodeInvalidArgument)
		} else {
			wantCode(t, err, connect.CodeResourceExhausted)
		}
	}
	wantCode(t, complete(id, good), connect.CodeResourceExhausted)

	// A code expires after CodeTTL.
	id, _ = start("b@acme.dev", "d2")
	code := e.inbox.code(t, "b@acme.dev")
	e.clock.Add(CodeTTL)
	wantCode(t, complete(id, code), connect.CodeDeadlineExceeded)

	// Registration needs an available plan.
	id, _ = start("c@acme.dev", "d3")
	_, err = e.client.CompleteEmailSignIn(ctx, connect.NewRequest(&authv1.CompleteEmailSignInRequest{ChallengeId: id, Code: e.inbox.code(t, "c@acme.dev"), Plan: authv1.Plan_PLAN_TEAM}))
	wantCode(t, err, connect.CodeInvalidArgument)

	// A code works once.
	id, _ = start("d@acme.dev", "d4")
	code = e.inbox.code(t, "d@acme.dev")
	if err := complete(id, code); err != nil {
		t.Fatal(err)
	}
	wantCode(t, complete(id, code), connect.CodeFailedPrecondition)

	// Limits per email and per device in an hour.
	for i := range LimitPerEmail {
		if _, err := start("e@acme.dev", "e"+string(rune('a'+i))); err != nil {
			t.Fatalf("code %d: %v", i, err)
		}
	}
	_, err = start("e@acme.dev", "fresh")
	wantCode(t, err, connect.CodeResourceExhausted)
	for i := range LimitPerDevice {
		if _, err := start("f"+string(rune('a'+i))+"@acme.dev", "same"); err != nil {
			t.Fatalf("device code %d: %v", i, err)
		}
	}
	_, err = start("g@acme.dev", "same")
	wantCode(t, err, connect.CodeResourceExhausted)
	// An hour later the limits are spent no more.
	e.clock.Add(time.Hour)
	if _, err := start("e@acme.dev", "same"); err != nil {
		t.Errorf("after an hour: %v", err)
	}
}

func TestSessionLifetime(t *testing.T) {
	e := newEnv(t)

	// Idle: a session not refreshed for SessionIdle ends.
	idle := e.register(t, "idle@acme.dev")
	// Active: refreshed every 20 days, it lives until SessionMax.
	active := e.register(t, "active@acme.dev")
	rt := active.Tokens.RefreshToken
	for elapsed := 20 * 24 * time.Hour; elapsed < SessionMax; elapsed += 20 * 24 * time.Hour {
		e.clock.Add(20 * 24 * time.Hour)
		r, err := e.refresh(rt)
		if err != nil {
			t.Fatalf("refresh after %v: %v", elapsed, err)
		}
		if r.Tokens.Session.IdleExpiresAt.AsTime().After(r.Tokens.Session.ExpiresAt.AsTime()) {
			t.Errorf("idle expiry beyond the session's end")
		}
		rt = r.Tokens.RefreshToken
	}
	_, err := e.refresh(idle.Tokens.RefreshToken)
	wantCode(t, err, connect.CodeUnauthenticated)

	e.clock.Add(20 * 24 * time.Hour) // 100 days
	_, err = e.refresh(rt)
	wantCode(t, err, connect.CodeUnauthenticated)
}

func pkce() (verifier, challenge string) {
	verifier = "a-very-long-random-verifier-0123456789abcdefghijklmnop"
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

var noRedirects = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func post(t *testing.T, u string, form url.Values) *http.Response {
	t.Helper()
	resp, err := noRedirects.PostForm(u, form)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func body(t *testing.T, r *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

var hiddenRe = regexp.MustCompile(`name="challenge_id" value="([^"]+)"`)

func TestBrowserSignIn(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.register(t, "dev@acme.dev")
	verifier, challenge := pkce()
	redirect := "http://127.0.0.1:53682/callback"
	params := url.Values{
		"client_id": {ClientID}, "response_type": {"code"}, "redirect_uri": {redirect}, "state": {"st-1"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"},
	}

	// A redirect anywhere but the loopback is refused without redirecting.
	bad := url.Values{}
	for k, v := range params {
		bad[k] = v
	}
	bad.Set("redirect_uri", "http://evil.example:80/cb")
	resp, err := noRedirects.Get(e.srv.URL + "/authorize?" + bad.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("evil redirect: %d", resp.StatusCode)
	}

	resp, err = noRedirects.Get(e.srv.URL + "/authorize?" + params.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if page := body(t, resp); resp.StatusCode != 200 || !strings.Contains(page, `name="email"`) {
		t.Fatalf("authorize page %d:\n%s", resp.StatusCode, page)
	}
	resp.Body.Close()

	form := url.Values{"step": {"email"}, "email": {"dev@acme.dev"}}
	for k, v := range params {
		form[k] = v
	}
	page := body(t, post(t, e.srv.URL+"/authorize", form))
	m := hiddenRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no code step:\n%s", page)
	}
	form.Set("step", "code")
	form.Set("challenge_id", m[1])
	form.Set("code", "999999x")
	if page := body(t, post(t, e.srv.URL+"/authorize", form)); !strings.Contains(page, "wrong or expired") {
		t.Fatalf("wrong code page:\n%s", page)
	}
	form.Set("code", e.inbox.code(t, "dev@acme.dev"))
	resp = post(t, e.srv.URL+"/authorize", form)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("code step %d:\n%s", resp.StatusCode, body(t, resp))
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if !strings.HasPrefix(loc.String(), redirect+"?") || loc.Query().Get("state") != "st-1" {
		t.Fatalf("redirect to %s", loc)
	}
	code := loc.Query().Get("code")

	exchange := func(verifier, redirect string) error {
		_, err := e.client.ExchangeAuthorizationCode(ctx, connect.NewRequest(&authv1.ExchangeAuthorizationCodeRequest{Code: code, CodeVerifier: verifier, RedirectUri: redirect, DeviceName: "mac"}))
		return err
	}
	wantCode(t, exchange("wrong-verifier", redirect), connect.CodeInvalidArgument)
	wantCode(t, exchange(verifier, "http://127.0.0.1:1/other"), connect.CodeInvalidArgument)
	if err := exchange(verifier, redirect); err != nil {
		t.Fatal(err)
	}
	wantCode(t, exchange(verifier, redirect), connect.CodeInvalidArgument) // used
}

var userCodeRe = regexp.MustCompile(`name="user_code" value="([A-Z-]+)"`)

func TestDeviceSignIn(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	reg := e.register(t, "ops@acme.dev")

	st, err := e.client.StartDeviceSignIn(ctx, connect.NewRequest(&authv1.StartDeviceSignInRequest{DeviceName: "build-server"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(st.Msg.VerificationUriComplete, e.srv.URL+"/device?user_code=") || len(st.Msg.UserCode) != 9 {
		t.Fatalf("start = %+v", st.Msg)
	}
	poll := func() (*authv1.PollDeviceSignInResponse, error) {
		r, err := e.client.PollDeviceSignIn(ctx, connect.NewRequest(&authv1.PollDeviceSignInRequest{DeviceCode: st.Msg.DeviceCode}))
		if err != nil {
			return nil, err
		}
		return r.Msg, nil
	}
	if r, err := poll(); err != nil || r.Status != authv1.DeviceSignInStatus_DEVICE_SIGN_IN_STATUS_PENDING {
		t.Fatalf("first poll = %v %v", r, err)
	}
	if r, _ := poll(); r.Status != authv1.DeviceSignInStatus_DEVICE_SIGN_IN_STATUS_SLOW_DOWN {
		t.Fatalf("fast poll = %v", r)
	}

	// The person types the code in lower case without the dash.
	typed := strings.ToLower(strings.ReplaceAll(st.Msg.UserCode, "-", ""))
	page := body(t, post(t, e.srv.URL+"/device", url.Values{"step": {"user_code"}, "user_code": {typed}}))
	if m := userCodeRe.FindStringSubmatch(page); m == nil || m[1] != st.Msg.UserCode || !strings.Contains(page, `name="email"`) {
		t.Fatalf("user code step:\n%s", page)
	}
	page = body(t, post(t, e.srv.URL+"/device", url.Values{"step": {"email"}, "user_code": {st.Msg.UserCode}, "email": {"ops@acme.dev"}}))
	m := hiddenRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no code step:\n%s", page)
	}
	page = body(t, post(t, e.srv.URL+"/device", url.Values{"step": {"code"}, "user_code": {st.Msg.UserCode}, "email": {"ops@acme.dev"},
		"challenge_id": {m[1]}, "code": {e.inbox.code(t, "ops@acme.dev")}}))
	if !strings.Contains(page, "signed in as ops@acme.dev") {
		t.Fatalf("done page:\n%s", page)
	}

	e.clock.Add(DeviceInterval)
	r, err := poll()
	if err != nil || r.Status != authv1.DeviceSignInStatus_DEVICE_SIGN_IN_STATUS_DONE || r.User.Id != reg.User.Id || r.Tokens.Session.DeviceName != "build-server" {
		t.Fatalf("done poll = %v %v", r, err)
	}
	_, err = poll()
	wantCode(t, err, connect.CodeFailedPrecondition)

	// An expired device code.
	st2, _ := e.client.StartDeviceSignIn(ctx, connect.NewRequest(&authv1.StartDeviceSignInRequest{DeviceName: "x"}))
	e.clock.Add(DeviceCodeTTL)
	_, err = e.client.PollDeviceSignIn(ctx, connect.NewRequest(&authv1.PollDeviceSignInRequest{DeviceCode: st2.Msg.DeviceCode}))
	wantCode(t, err, connect.CodeDeadlineExceeded)
}

func TestJWKSAndKeyRotation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	reg := e.register(t, "keys@acme.dev")
	remote := &token.Remote{URL: e.srv.URL + "/.well-known/jwks.json"}
	want := token.Expect{Issuer: e.srv.URL, Audience: token.DefaultAudience, Now: e.clock.Now()}
	if _, err := token.Verify(reg.Tokens.AccessToken, remote.Key, want); err != nil {
		t.Fatalf("verify with JWKS: %v", err)
	}

	// After a rotation the old key stays published while its tokens can live.
	if err := e.svc.RotateKeys(ctx, true); err != nil {
		t.Fatal(err)
	}
	if n := len(e.svc.JWKS().Keys); n != 2 {
		t.Fatalf("%d keys right after rotation", n)
	}
	r, err := e.refresh(reg.Tokens.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	remote = &token.Remote{URL: remote.URL}
	if _, err := token.Verify(r.Tokens.AccessToken, remote.Key, want); err != nil {
		t.Fatalf("token of the new key: %v", err)
	}
	e.clock.Add(2*AccessTTL + time.Second)
	if n := len(e.svc.JWKS().Keys); n != 1 {
		t.Errorf("%d keys long after rotation", n)
	}
	// A key older than KeyRotation is replaced on the next check.
	e.clock.Add(KeyRotation)
	if err := e.svc.RotateKeys(ctx, false); err != nil {
		t.Fatal(err)
	}
	if n := len(e.svc.JWKS().Keys); n != 2 {
		t.Errorf("%d keys after scheduled rotation", n)
	}
}

func TestWorkspaceRegistry(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	reg := e.register(t, "admin@acme.dev")
	other := e.register(t, "other@acme.dev")
	add := func(access, id, name, server string) error {
		_, err := e.client.RegisterWorkspace(ctx, withToken(&authv1.RegisterWorkspaceRequest{Id: id, Name: name, ServerUrl: server}, access))
		return err
	}
	wantCode(t, add(reg.Tokens.AccessToken, "wsp_1", "acme", "ftp://x"), connect.CodeInvalidArgument)
	wantCode(t, add(reg.Tokens.AccessToken, "", "acme", "https://x"), connect.CodeInvalidArgument)
	for _, err := range []error{
		add(reg.Tokens.AccessToken, "wsp_1", "acme", "https://old.example"),
		add(reg.Tokens.AccessToken, "wsp_1", "Acme", "https://acme.example"), // an update
		add(reg.Tokens.AccessToken, "wsp_2", "beta", "https://beta.example"),
		add(other.Tokens.AccessToken, "wsp_1", "acme", "https://acme.example"),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	list := func(access string) []*authv1.Workspace {
		t.Helper()
		res, err := e.client.ListWorkspaces(ctx, withToken(&authv1.ListWorkspacesRequest{}, access))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.Workspaces
	}
	if ws := list(reg.Tokens.AccessToken); len(ws) != 2 || ws[0].Name != "Acme" || ws[0].ServerUrl != "https://acme.example" {
		t.Fatalf("registry = %v", ws)
	}
	if _, err := e.client.ForgetWorkspace(ctx, withToken(&authv1.ForgetWorkspaceRequest{Id: "wsp_1"}, reg.Tokens.AccessToken)); err != nil {
		t.Fatal(err)
	}
	if ws := list(reg.Tokens.AccessToken); len(ws) != 1 || ws[0].Id != "wsp_2" {
		t.Errorf("after forget: %v", ws)
	}
	// Each user has a registry of their own.
	if ws := list(other.Tokens.AccessToken); len(ws) != 1 || ws[0].Id != "wsp_1" {
		t.Errorf("other's registry: %v", ws)
	}
	_, err := e.client.ListWorkspaces(ctx, connect.NewRequest(&authv1.ListWorkspacesRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)
}
