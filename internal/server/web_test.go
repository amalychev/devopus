package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	workspacev1 "github.com/amalychev/devopus/gen/devopus/workspace/v1"
	"github.com/amalychev/devopus/gen/devopus/workspace/v1/workspacev1connect"
	"github.com/amalychev/devopus/internal/auth/authtest"
	"github.com/amalychev/devopus/internal/auth/token"
	"github.com/amalychev/devopus/internal/pgtest"
	"github.com/amalychev/devopus/internal/server/store"
)

// TestWebSignIn: the web interface signs in through the server, which keeps the
// refresh token in an HttpOnly cookie and hands the page access tokens.
func TestWebSignIn(t *testing.T) {
	a := authtest.Start(t)
	pool := pgtest.Pool(t)
	if err := store.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	keys := &token.Remote{URL: a.URL + "/.well-known/jwks.json"}
	svc, err := New(pool, Config{Keys: keys.Key, AuthIssuer: a.URL, AuthURL: a.URL})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}

	post := func(path string, body any, headers map[string]string) (int, map[string]any, *http.Response) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Devopus", "1")
		for k, v := range headers {
			if v == "" {
				req.Header.Del(k)
			} else {
				req.Header.Set(k, v)
			}
		}
		resp, err := browser.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		raw, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out, resp
	}

	// Another site cannot drive it: no custom header, or a foreign origin.
	if code, _, _ := post("/web/auth/start", map[string]any{"email": "a@acme.dev"}, map[string]string{"X-Devopus": ""}); code != http.StatusForbidden {
		t.Errorf("without the header: %d", code)
	}
	if code, _, _ := post("/web/auth/start", map[string]any{"email": "a@acme.dev"}, map[string]string{"Origin": "https://evil.example"}); code != http.StatusForbidden {
		t.Errorf("foreign origin: %d", code)
	}

	// Register through the page: the code from the email, the Free plan.
	code, st, _ := post("/web/auth/start", map[string]any{"email": "lead@acme.dev", "register": true, "deviceId": "browser-1"}, nil)
	if code != http.StatusOK || st["challengeId"] == "" {
		t.Fatalf("start: %d %v", code, st)
	}
	code, bad, _ := post("/web/auth/complete", map[string]any{"challengeId": st["challengeId"], "code": "000000x", "plan": 1}, nil)
	if code != http.StatusBadRequest || !strings.Contains(bad["error"].(string), "wrong") {
		t.Errorf("wrong code: %d %v", code, bad)
	}
	code, ses, resp := post("/web/auth/complete", map[string]any{"challengeId": st["challengeId"], "code": a.Inbox.Code(t, "lead@acme.dev", 1), "plan": 1}, nil)
	if code != http.StatusOK || ses["email"] != "lead@acme.dev" || ses["created"] != true || ses["accessToken"] == "" {
		t.Fatalf("complete: %d %v", code, ses)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == refreshCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/web/auth/" {
		t.Fatalf("refresh cookie: %+v", cookie)
	}
	if strings.Contains(strings.Join(mapValues(ses), " "), cookie.Value) {
		t.Error("the refresh token reached the page")
	}

	// The access token works with the workspace's calls, as the CLI's does.
	ws := workspacev1connect.NewWorkspaceServiceClient(http.DefaultClient, srv.URL)
	r := connect.NewRequest(&workspacev1.CreateWorkspaceRequest{Name: "web"})
	r.Header().Set("Authorization", "Bearer "+ses["accessToken"].(string))
	if _, err := ws.CreateWorkspace(context.Background(), r); err != nil {
		t.Fatal(err)
	}

	// A reload: the cookie gives a new access token and rotates itself.
	code, again, resp := post("/web/auth/refresh", nil, nil)
	if code != http.StatusOK || again["email"] != "lead@acme.dev" || again["accessToken"] == "" {
		t.Fatalf("refresh: %d %v", code, again)
	}
	if c := resp.Cookies(); len(c) != 1 || c[0].Value == cookie.Value {
		t.Errorf("the refresh cookie did not rotate")
	}

	// Signing out ends the session and the cookie.
	if code, _, _ := post("/web/auth/logout", nil, nil); code != http.StatusOK {
		t.Fatalf("logout: %d", code)
	}
	if code, out, _ := post("/web/auth/refresh", nil, nil); code != http.StatusOK || out["signedIn"] != false || out["accessToken"] != nil {
		t.Errorf("refresh after logout: %d %v", code, out)
	}

	// The app: any path is the page, which routes itself; strict headers.
	for _, p := range []string{"/", "/members", "/invites/accept"} {
		res, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 || !strings.Contains(string(body), "<title>") || !strings.Contains(res.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Errorf("%s: %d %q", p, res.StatusCode, res.Header.Get("Content-Security-Policy"))
		}
	}
}

func mapValues(m map[string]any) []string {
	var out []string
	for _, v := range m {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
