// Package ui serves the local panel: the web app from web/, built into dist, and the
// agent's API for it on 127.0.0.1.
//
// The CLI reaches the agent through a unix socket only the user can open; a browser
// cannot, and any web page can send requests to 127.0.0.1. So the panel's API needs a
// token: `devopus ui` reads it from the agent's folder and opens /auth with it, which
// turns it into a SameSite=Strict cookie. Requests whose Host is not the panel's own
// address are refused, so a rebinding DNS name cannot make the browser treat the
// panel as its site.
package ui

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

//go:embed all:dist
var dist embed.FS

// TokenHeader lets the Vite dev server (web/vite.config.ts) call the API without the cookie.
const TokenHeader = "Authorization"

// Token reads the panel's token, creating it on first use.
func Token(file string) (string, error) {
	if b, err := os.ReadFile(file); err == nil {
		if t := strings.TrimSpace(string(b)); len(t) >= 32 {
			return t, nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	t := hex.EncodeToString(buf)
	if err := os.WriteFile(file, []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	return t, nil
}

// Server is the panel's HTTP handler.
type Server struct {
	api    http.Handler
	token  string
	cookie string
	port   int
	static http.Handler
}

// New serves the panel on port: the agent's API (api, a ConnectRPC handler) behind the
// token, and the web app.
func New(api http.Handler, token, cookie string, port int) *Server {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return &Server{api: api, token: token, cookie: cookie, port: port, static: spa(sub)}
}

// AuthPath is where `devopus ui` sends the browser with the token.
const AuthPath = "/auth"

// URL is the address `devopus ui` opens.
func URL(port int, token string) string {
	return "http://127.0.0.1:" + strconv.Itoa(port) + AuthPath + "?token=" + token
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.ownHost(r.Host) {
		http.Error(w, "this address does not serve the Devopus panel; open it with 'devopus ui'", http.StatusMisdirectedRequest)
		return
	}
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	switch {
	case r.URL.Path == AuthPath:
		s.auth(w, r)
	case isAPI(r.URL.Path):
		if !s.authorized(r) {
			// The panel shows how to sign in on this code.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"unauthenticated","message":"open the panel with 'devopus ui'"}`))
			return
		}
		s.api.ServeHTTP(w, r)
	default:
		s.static.ServeHTTP(w, r)
	}
}

func isAPI(p string) bool { return strings.HasPrefix(p, "/devopus.agent.v1.") }

func (s *Server) ownHost(host string) bool {
	h, p, err := net.SplitHostPort(host)
	if err != nil || p != strconv.Itoa(s.port) {
		return false
	}
	return h == "127.0.0.1" || h == "localhost"
}

func (s *Server) auth(w http.ResponseWriter, r *http.Request) {
	if !s.valid(r.URL.Query().Get("token")) {
		http.Error(w, "the link is out of date; open the panel with 'devopus ui'", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.cookie, Value: s.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Expires: time.Now().AddDate(1, 0, 0),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) authorized(r *http.Request) bool {
	// A page of another site may send a simple request; Connect's JSON POSTs are not
	// simple, but checking the origin costs nothing.
	if o := r.Header.Get("Origin"); o != "" {
		if !s.ownOrigin(o) {
			return false
		}
	}
	if c, err := r.Cookie(s.cookie); err == nil && s.valid(c.Value) {
		return true
	}
	if t, ok := strings.CutPrefix(r.Header.Get(TokenHeader), "Bearer "); ok && s.valid(t) {
		return true
	}
	return false
}

func (s *Server) ownOrigin(o string) bool {
	h, ok := strings.CutPrefix(o, "http://")
	return ok && s.ownHost(h)
}

func (s *Server) valid(t string) bool {
	return t != "" && subtle.ConstantTimeCompare([]byte(t), []byte(s.token)) == 1
}

// spa serves the built files; every other path is the app, which routes itself.
func spa(files fs.FS) http.Handler {
	fileServer := http.FileServerFS(files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p != "" {
			if _, err := fs.Stat(files, p); err == nil {
				if strings.HasPrefix(p, "assets/") {
					// Vite puts a content hash in their names.
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				fileServer.ServeHTTP(w, r)
				return
			} else if !errors.Is(err, fs.ErrNotExist) {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if path.Ext(p) != "" {
				http.NotFound(w, r)
				return
			}
		}
		index, err := fs.ReadFile(files, "index.html")
		if err != nil {
			http.Error(w, "the panel is not built into this binary: run 'npm run build' in web/", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	})
}
