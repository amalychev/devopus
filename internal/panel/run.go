package panel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/amalychev/devopus/internal/agent/paths"
	"github.com/amalychev/devopus/internal/daemon"
)

// ErrAlreadyRunning: another panel holds the lock of this data directory.
var ErrAlreadyRunning = daemon.ErrAlreadyRunning

// Options of Run.
type Options struct {
	Version string
	Log     *slog.Logger
	// Port to listen on; 0 keeps the port of the last run (open tabs keep working),
	// or picks a free one the first time.
	Port int
}

// Run serves the local panel until ctx is done.
func Run(ctx context.Context, opt Options) error {
	log := opt.Log
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(paths.Panel(), 0o700); err != nil {
		return err
	}
	unlock, err := daemon.Lock(paths.PanelLock())
	if err != nil {
		return err
	}
	defer unlock()
	token, err := Token(paths.PanelToken())
	if err != nil {
		return err
	}
	ln, err := listen(opt.Port)
	if err != nil {
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	h := New(AgentAPI(paths.Socket()), token, "devopus_panel_"+paths.Instance(), port)
	h.Version = opt.Version
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	log.Info("panel started", "version", opt.Version, "url", BaseURL(port), "agent", paths.Socket())

	select {
	case <-ctx.Done():
	case err = <-serveErr:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	log.Info("panel stopped")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// BaseURL is the panel's address, without the token.
func BaseURL(port int) string { return "http://127.0.0.1:" + strconv.Itoa(port) }

// listen takes the requested port, or the pinned one, or a free one; the port it got
// is written down for the next run and for `devopus panel open`.
func listen(port int) (net.Listener, error) {
	addrs := []string{}
	if port != 0 {
		addrs = append(addrs, "127.0.0.1:"+strconv.Itoa(port))
	} else {
		if b, err := os.ReadFile(paths.PanelPort()); err == nil {
			addrs = append(addrs, "127.0.0.1:"+strings.TrimSpace(string(b)))
		}
		addrs = append(addrs, "127.0.0.1:0")
	}
	var ln net.Listener
	var err error
	for _, a := range addrs {
		if ln, err = net.Listen("tcp", a); err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(paths.PanelPort(), []byte(strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)+"\n"), 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// AgentAPI forwards the app's ConnectRPC calls to the agent's unix socket, streams
// included. When the agent is not running, the app gets a Connect "unavailable" error
// and keeps retrying.
func AgentAPI(socket string) http.Handler {
	target := &url.URL{Scheme: "http", Host: "devopus-agent"}
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Header.Del("Cookie")
			r.Out.Header.Del(TokenHeader)
			r.Out.Header.Del("Origin")
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return // the browser went away
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, `{"code":"unavailable","message":"the agent is not running; start it with 'devopus agent start'"}`)
		},
	}
}
