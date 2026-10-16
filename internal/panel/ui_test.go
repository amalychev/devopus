package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const token = "0123456789abcdef0123456789abcdef0123456789abcdef"

func newServer() *Server {
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("api")) })
	return New(api, token, "devopus_panel_test", 41234)
}

func do(s *Server, method, url string, mod func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, url, nil)
	r.Host = "127.0.0.1:41234"
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

const apiPath = "/devopus.agent.v2.AgentService/Status"

func TestAPINeedsToken(t *testing.T) {
	s := newServer()
	if w := do(s, "POST", apiPath, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("without a cookie: %d", w.Code)
	}
	if w := do(s, "POST", apiPath, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "devopus_panel_test", Value: "wrong"})
	}); w.Code != http.StatusUnauthorized {
		t.Fatalf("with a wrong cookie: %d", w.Code)
	}
	if w := do(s, "POST", apiPath, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "devopus_panel_test", Value: token})
	}); w.Code != http.StatusOK || w.Body.String() != "api" {
		t.Fatalf("with the cookie: %d %s", w.Code, w.Body)
	}
	if w := do(s, "POST", apiPath, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
	}); w.Code != http.StatusOK {
		t.Fatalf("with the bearer token: %d", w.Code)
	}
}

func TestAuthSetsCookie(t *testing.T) {
	s := newServer()
	if w := do(s, "GET", "/auth?token=nope", nil); w.Code != http.StatusForbidden {
		t.Fatalf("bad token: %d", w.Code)
	}
	w := do(s, "GET", "/auth?token="+token, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("auth: %d %q", w.Code, w.Header().Get("Location"))
	}
	c := w.Result().Cookies()
	if len(c) != 1 || c[0].Value != token || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie: %+v", c)
	}
}

// A page of another site that rebinds its DNS name to 127.0.0.1 sends its own Host.
func TestForeignHostRefused(t *testing.T) {
	s := newServer()
	for _, host := range []string{"evil.example:41234", "127.0.0.1:9999", "localhost.evil.example:41234"} {
		w := do(s, "GET", "/auth?token="+token, func(r *http.Request) { r.Host = host })
		if w.Code != http.StatusMisdirectedRequest {
			t.Errorf("host %s: %d", host, w.Code)
		}
	}
	if w := do(s, "GET", "/", func(r *http.Request) { r.Host = "localhost:41234" }); w.Code != http.StatusOK {
		t.Errorf("localhost: %d", w.Code)
	}
}

func TestForeignOriginRefused(t *testing.T) {
	s := newServer()
	w := do(s, "POST", apiPath, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "devopus_panel_test", Value: token})
		r.Header.Set("Origin", "https://evil.example")
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("foreign origin: %d", w.Code)
	}
	w = do(s, "POST", apiPath, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "devopus_panel_test", Value: token})
		r.Header.Set("Origin", "http://127.0.0.1:41234")
	})
	if w.Code != http.StatusOK {
		t.Fatalf("own origin: %d", w.Code)
	}
}

func TestAppRoutes(t *testing.T) {
	s := newServer()
	w := do(s, "GET", "/some/env", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `id="root"`) {
		t.Fatalf("app route: %d %.80s", w.Code, w.Body)
	}
	if w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("security headers missing")
	}
	if w := do(s, "GET", "/assets/missing.js", nil); w.Code != http.StatusNotFound {
		t.Fatalf("missing asset: %d", w.Code)
	}
}
