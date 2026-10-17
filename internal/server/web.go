package server

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"connectrpc.com/connect"

	authv1 "github.com/amalychev/devopus/gen/devopus/auth/v1"
	"github.com/amalychev/devopus/gen/devopus/auth/v1/authv1connect"
)

// The web interface of the workspace (R11): the same calls and permissions as the
// CLI. The server serves the app and signs people in for it as a backend for the
// frontend: it talks to Auth, keeps the refresh token in an HttpOnly cookie the page's
// scripts cannot read, and gives the page short-lived access tokens.

//go:embed all:webdist
var webdist embed.FS

// refreshCookie holds the web session's refresh token; only /web/auth/ sees it.
const refreshCookie = "devopus_refresh"

func (s *Service) webHandler(mux *http.ServeMux) {
	mux.HandleFunc("POST /web/auth/plans", s.webAuth(s.webPlans))
	mux.HandleFunc("POST /web/auth/start", s.webAuth(s.webStart))
	mux.HandleFunc("POST /web/auth/complete", s.webAuth(s.webComplete))
	mux.HandleFunc("POST /web/auth/refresh", s.webAuth(s.webRefresh))
	mux.HandleFunc("POST /web/auth/logout", s.webAuth(s.webLogout))
	dist, err := fs.Sub(webdist, "webdist")
	if err != nil {
		panic(err)
	}
	files := http.FileServerFS(dist)
	// "/" without a method: "GET /" would clash with the RPC paths, which take any.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		setSecurityHeaders(w)
		// A single-page app: unknown paths get the page, which routes by itself.
		if p := strings.TrimPrefix(path.Clean(r.URL.Path), "/"); p != "" {
			if _, err := fs.Stat(dist, p); err == nil {
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, dist, "index.html")
	})
}

func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
}

// webAuth guards the sign-in endpoints: JSON from this site only. The custom header
// cannot be sent by another site's form, and the Origin must be ours.
func (s *Service) webAuth(fn func(w http.ResponseWriter, r *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Header.Get("X-Devopus") != "1" || !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		out, err := fn(w, r)
		if err != nil {
			writeWebError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true // same-origin fetches may omit it; the custom header is the guard
	}
	return strings.TrimPrefix(strings.TrimPrefix(o, "https://"), "http://") == r.Host
}

func writeWebError(w http.ResponseWriter, err error) {
	status, msg := http.StatusBadGateway, err.Error()
	var ce *connect.Error
	if errors.As(err, &ce) {
		msg = ce.Message()
		switch ce.Code() {
		case connect.CodeInvalidArgument, connect.CodeFailedPrecondition, connect.CodeDeadlineExceeded:
			status = http.StatusBadRequest
		case connect.CodeNotFound:
			status = http.StatusNotFound
		case connect.CodeUnauthenticated:
			status = http.StatusUnauthorized
		case connect.CodeResourceExhausted:
			status = http.StatusTooManyRequests
		case connect.CodeUnavailable:
			status, msg = http.StatusBadGateway, "the sign-in service is not reachable; try again later"
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (s *Service) authClient() authv1connect.AuthServiceClient {
	return authv1connect.NewAuthServiceClient(&http.Client{Timeout: 30 * time.Second}, s.cfg.AuthURL)
}

// forward makes an Auth request on behalf of the browser: its address goes along, for
// Auth's limits behind a proxy it trusts.
func forward[T any](r *http.Request, msg *T) *connect.Request[T] {
	req := connect.NewRequest(msg)
	if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		req.Header().Set("X-Forwarded-For", ip)
	}
	return req
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("bad request"))
	}
	return nil
}

func (s *Service) webPlans(_ http.ResponseWriter, r *http.Request) (any, error) {
	res, err := s.authClient().ListPlans(r.Context(), forward(r, &authv1.ListPlansRequest{}))
	if err != nil {
		return nil, err
	}
	type plan struct {
		ID          int32  `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Available   bool   `json:"available"`
	}
	var out []plan
	for _, p := range res.Msg.Plans {
		out = append(out, plan{int32(p.Plan), p.Name, p.Description, p.Available})
	}
	return out, nil
}

func (s *Service) webStart(_ http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Email    string `json:"email"`
		Register bool   `json:"register"`
		DeviceID string `json:"deviceId"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	purpose := authv1.Purpose_PURPOSE_SIGN_IN
	if in.Register {
		purpose = authv1.Purpose_PURPOSE_REGISTER
	}
	res, err := s.authClient().StartEmailSignIn(r.Context(), forward(r, &authv1.StartEmailSignInRequest{Email: in.Email, Purpose: purpose, DeviceId: in.DeviceID}))
	if err != nil {
		return nil, err
	}
	return map[string]any{"challengeId": res.Msg.ChallengeId, "expiresAt": res.Msg.ExpiresAt.AsTime()}, nil
}

// webSession is what the page gets: an access token for the workspace calls, kept in
// memory, and who is signed in.
type webSession struct {
	AccessToken string    `json:"accessToken"`
	ExpiresAt   time.Time `json:"expiresAt"`
	Email       string    `json:"email"`
	UserID      string    `json:"userId"`
	Created     bool      `json:"created,omitempty"`
}

func (s *Service) setRefresh(w http.ResponseWriter, r *http.Request, t *authv1.Tokens) {
	c := &http.Cookie{Name: refreshCookie, Value: t.GetRefreshToken(), Path: "/web/auth/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"}
	if exp := t.GetSession().GetExpiresAt(); exp != nil {
		c.Expires = exp.AsTime()
	}
	http.SetCookie(w, c)
}

func clearRefresh(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: refreshCookie, Value: "", Path: "/web/auth/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
}

func (s *Service) webComplete(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		ChallengeID string `json:"challengeId"`
		Code        string `json:"code"`
		Plan        int32  `json:"plan"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	res, err := s.authClient().CompleteEmailSignIn(r.Context(), forward(r, &authv1.CompleteEmailSignInRequest{
		ChallengeId: in.ChallengeID, Code: in.Code, Plan: authv1.Plan(in.Plan), DeviceName: webDevice(r),
	}))
	if err != nil {
		return nil, err
	}
	// The first access token is Auth's own: trade it at once for one of this server.
	ref, err := s.authClient().Refresh(r.Context(), forward(r, &authv1.RefreshRequest{RefreshToken: res.Msg.Tokens.RefreshToken, Audience: s.cfg.PublicURL}))
	if err != nil {
		return nil, err
	}
	s.setRefresh(w, r, ref.Msg.Tokens)
	return webSession{AccessToken: ref.Msg.Tokens.AccessToken, ExpiresAt: ref.Msg.Tokens.AccessExpiresAt.AsTime(),
		Email: res.Msg.User.Email, UserID: res.Msg.User.Id, Created: res.Msg.Created}, nil
}

func (s *Service) webRefresh(w http.ResponseWriter, r *http.Request) (any, error) {
	c, err := r.Cookie(refreshCookie)
	if err != nil || c.Value == "" {
		// Not signed in on this browser: an answer, not an error, for the page's first look.
		return map[string]bool{"signedIn": false}, nil
	}
	res, err := s.authClient().Refresh(r.Context(), forward(r, &authv1.RefreshRequest{RefreshToken: c.Value, Audience: s.cfg.PublicURL}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeUnauthenticated {
			clearRefresh(w)
		}
		return nil, err
	}
	s.setRefresh(w, r, res.Msg.Tokens)
	return webSession{AccessToken: res.Msg.Tokens.AccessToken, ExpiresAt: res.Msg.Tokens.AccessExpiresAt.AsTime(),
		Email: res.Msg.User.Email, UserID: res.Msg.User.Id}, nil
}

func (s *Service) webLogout(w http.ResponseWriter, r *http.Request) (any, error) {
	if c, err := r.Cookie(refreshCookie); err == nil && c.Value != "" {
		if _, err := s.authClient().SignOut(r.Context(), forward(r, &authv1.SignOutRequest{RefreshToken: c.Value})); err != nil {
			s.cfg.Log.Warn("web sign-out", "err", err)
		}
	}
	clearRefresh(w)
	return map[string]bool{"signedOut": true}, nil
}

// webDevice names a browser session in 'devopus auth sessions'.
func webDevice(r *http.Request) string {
	ua := r.UserAgent()
	for _, b := range []string{"Firefox", "Edg", "Chrome", "Safari"} {
		if strings.Contains(ua, b+"/") {
			if b == "Edg" {
				b = "Edge"
			}
			return "web, " + b
		}
	}
	return "web browser"
}
