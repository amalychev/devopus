package lifecycle

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/devopus/devopus/internal/agent/proxy"
)

// browserWait is how long a browser waits before it gets the "Starting…" page instead
// (P-7: a fast feature answers without the page).
const browserWait = 300 * time.Millisecond

// Waker serves the requests the proxy sends while a feature sleeps: it wakes the
// feature, then replays each request straight to its unit through the proxy.
type Waker struct {
	m      *Manager
	replay *httputil.ReverseProxy
}

// NewWaker replays through the proxy's HTTPS port on the host's loopback; roots trust
// the local CA. ports returns the proxy's current ports.
func NewWaker(m *Manager, roots *x509.CertPool, ports func() proxy.Ports) *Waker {
	transport := &http.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(addr)
			raw, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(ports().HTTPS)))
			if err != nil {
				return nil, err
			}
			c := tls.Client(raw, &tls.Config{RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}})
			if err := c.HandshakeContext(ctx); err != nil {
				raw.Close()
				return nil, err
			}
			return c, nil
		},
		ForceAttemptHTTP2: false,
		MaxIdleConns:      16,
		IdleConnTimeout:   30 * time.Second,
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			host := r.In.Host
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			r.Out.URL.Scheme, r.Out.URL.Host = "https", host
			r.Out.Host = r.In.Host
			r.Out.Header.Del(proxy.HeaderEnv)
			r.Out.Header.Del(proxy.HeaderUnit)
			r.Out.Header.Set(proxy.HeaderDirect, "1")
			// The client's own forwarding headers, as Envoy passed them.
			r.Out.Header["X-Forwarded-For"] = r.In.Header["X-Forwarded-For"]
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			page(w, http.StatusBadGateway, "The feature woke up but did not answer", html.EscapeString(err.Error()))
		},
	}
	return &Waker{m: m, replay: rp}
}

func (wk *Waker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	envID := r.Header.Get(proxy.HeaderEnv)
	if envID == "" {
		page(w, http.StatusBadGateway, "Not a request for a feature", "The Devopus waker got a request without an environment.")
		return
	}
	wk.m.opt.Log.Debug("waker", "env", envID, "method", r.Method, "path", r.URL.Path, "asleep", wk.m.Asleep(envID))
	done, result := wk.m.Wake(envID)
	browser := r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html")
	if browser {
		select {
		case <-done:
		case <-time.After(browserWait):
			w.Header().Set("Retry-After", "1")
			w.Header().Set("Refresh", "1")
			page(w, http.StatusServiceUnavailable, "Starting "+wk.m.name(envID)+"…",
				"The feature was asleep. It is starting; this page reloads by itself.")
			return
		case <-r.Context().Done():
			return
		}
	} else {
		select {
		case <-done:
		case <-r.Context().Done():
			return
		}
	}
	if err := result(); err != nil {
		page(w, http.StatusBadGateway, "The feature did not wake up", html.EscapeString(err.Error())+"</p><p>Run <code>devopus status</code> for details.")
		return
	}
	wk.replay.ServeHTTP(w, r)
}

func (m *Manager) name(envID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.envs[envID]; ok && e.Name != "" {
		return e.Name
	}
	return envID
}

func page(w http.ResponseWriter, status int, title, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<!doctype html><html lang=en><meta charset=utf-8><meta name=viewport content='width=device-width,initial-scale=1'>"+
		"<title>%s · Devopus</title><style>body{font:16px/1.5 system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem;color:#222;background:#fff}"+
		"@media(prefers-color-scheme:dark){body{color:#ddd;background:#16171a}}</style><h1>%s</h1><p>%s</p></html>",
		html.EscapeString(title), html.EscapeString(title), text)
}

// ListenWaker opens the waker on the host's loopback; the proxy reaches it through
// host.docker.internal, as it does the xDS server.
func ListenWaker(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return ln, nil
}

// Serve serves the waker until ctx ends.
func (wk *Waker) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: wk, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
