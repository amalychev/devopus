// Package agent runs the local Devopus agent: the desired-state store, the
// reconcile loop and the local API on a unix socket (docs/architecture/agent.md).
package agent

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	fallback "golang.org/x/crypto/x509roots/fallback/bundle"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/devopus/devopus/gen/devopus/agent/v1"
	"github.com/devopus/devopus/gen/devopus/agent/v1/agentv1connect"
	"github.com/devopus/devopus/internal/agent/api"
	"github.com/devopus/devopus/internal/agent/lifecycle"
	"github.com/devopus/devopus/internal/agent/paths"
	"github.com/devopus/devopus/internal/agent/pki"
	"github.com/devopus/devopus/internal/agent/proxy"
	"github.com/devopus/devopus/internal/agent/reconcile"
	"github.com/devopus/devopus/internal/agent/runtime"
	"github.com/devopus/devopus/internal/agent/store"
	"github.com/devopus/devopus/internal/agent/ui"
	"github.com/devopus/devopus/internal/workspace"
)

// Intervals of the reconcile loop: a periodic pass catches changes made behind the
// agent's back (a network removed by hand); after a failure it retries sooner.
const (
	resyncInterval = 30 * time.Second
	retryInterval  = 5 * time.Second
	// waitInterval: modules are still starting; look again soon.
	waitInterval = 2 * time.Second
	// passTimeout bounds one pass: clones, builds and installs can take minutes.
	passTimeout = 30 * time.Minute
)

type Options struct {
	Version string
	Log     *slog.Logger
	// ProxyPorts pins the proxy's host ports instead of probing 80/443; for tests that
	// run next to a real agent.
	ProxyPorts *proxy.Ports
}

type Agent struct {
	opt   Options
	log   *slog.Logger
	store *store.Store
	rt    *runtime.Docker
	rec   *reconcile.Reconciler
	hub   *api.Hub
	ca    *pki.CA

	life *lifecycle.Manager
	// kick asks the loop for a pass now.
	kick chan struct{}

	panelURL string

	mu      sync.Mutex
	rtInfo  *agentv1.RuntimeInfo
	lastErr error
}

// Sleep puts a feature to sleep in the background.
func (a *Agent) Sleep(envID string) error {
	if !a.life.Known(envID) {
		return errors.New("the feature is not running yet")
	}
	go func() {
		if err := a.life.Sleep(context.Background(), envID, lifecycle.ReasonManual); err != nil {
			a.log.Warn("sleep", "env", envID, "error", err)
		}
	}()
	return nil
}

// Wake wakes a feature in the background.
func (a *Agent) Wake(envID string) error {
	if !a.life.Known(envID) {
		return errors.New("the feature is not running yet")
	}
	a.life.Wake(envID)
	return nil
}

func (a *Agent) kickLoop() {
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

func (a *Agent) Containers() api.Containers             { return a.rt }
func (a *Agent) EnvsDir() string                        { return paths.Envs() }
func (a *Agent) PanelURL() string                       { return a.panelURL }
func (a *Agent) Store() *store.Store                    { return a.store }
func (a *Agent) Statuses() []*agentv1.EnvironmentStatus { return a.rec.Statuses() }
func (a *Agent) Version() string                        { return a.opt.Version }
func (a *Agent) Force(envID string)                     { a.rec.Force(envID) }

func (a *Agent) Unsaved(ctx context.Context, env *agentv1.EnvironmentSpec, ws *workspace.Workspace) (map[string]string, error) {
	return a.rec.Unsaved(ctx, env, ws)
}
func (a *Agent) Proxy() *agentv1.ProxyStatus { return a.rec.Proxy() }
func (a *Agent) Routes() []*agentv1.Route    { return a.rec.Routes() }

func (a *Agent) CA() *agentv1.CAInfo {
	return &agentv1.CAInfo{RootPath: a.ca.RootPath(), Fingerprint: a.ca.Fingerprint(), NotAfter: timestamppb.New(a.ca.Root.NotAfter)}
}

func (a *Agent) Runtime() *agentv1.RuntimeInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rtInfo == nil {
		ep := a.rt.Endpoint()
		return &agentv1.RuntimeInfo{Name: ep.Name, Endpoint: ep.Host}
	}
	return a.rtInfo
}

// Run serves until ctx is done. Only one agent runs per data directory.
func Run(ctx context.Context, opt Options) error {
	log := opt.Log
	if log == nil {
		log = slog.Default()
	}
	if err := paths.Ensure(); err != nil {
		return err
	}
	unlock, err := lock(paths.Lock())
	if err != nil {
		return err
	}
	defer unlock()

	st, err := store.Open(paths.State())
	if err != nil {
		return err
	}
	ep := runtime.Detect()
	rt, err := runtime.New(ep)
	if err != nil {
		return err
	}
	defer rt.Close()

	ca, err := pki.LoadOrCreateCA(paths.CA())
	if err != nil {
		return fmt.Errorf("local CA: %w", err)
	}
	ctl, err := pki.LoadOrCreateControl(paths.Proxy())
	if err != nil {
		return fmt.Errorf("proxy control certificates: %w", err)
	}
	xds, err := listenXDS(ctl)
	if err != nil {
		return fmt.Errorf("xDS server: %w", err)
	}
	defer xds.Close()
	go func() {
		if err := xds.Serve(); err != nil {
			log.Error("xDS server", "error", err)
		}
	}()
	choose := proxy.ChoosePorts
	if opt.ProxyPorts != nil {
		fixed := *opt.ProxyPorts
		choose = func(context.Context) (proxy.Ports, []proxy.Conflict, error) { return fixed, nil, nil }
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Root)
	relays := &proxy.Relays{}
	defer relays.Close()

	a := &Agent{opt: opt, log: log, store: st, rt: rt, hub: api.NewHub(), ca: ca, kick: make(chan struct{}, 1)}
	wakerLn, err := listenPinned(filepath.Join(paths.Proxy(), "waker-port"), lifecycle.ListenWaker)
	if err != nil {
		return fmt.Errorf("waker: %w", err)
	}
	direct := proxy.AnswersDirect(roots)
	proxyPorts := func() proxy.Ports {
		p := a.rec.Proxy()
		return proxy.Ports{HTTP: uint16(p.HttpPort), HTTPS: uint16(p.HttpsPort)}
	}
	a.life = lifecycle.New(rt, lifecycle.Options{
		Instance: paths.Instance(),
		File:     filepath.Join(paths.Home(), "lifecycle.json"),
		SetAwake: func(ctx context.Context, envID string, awake bool) error { return a.rec.SetAwake(ctx, envID, awake) },
		Answers: func(ctx context.Context, host, path string) error {
			return direct(ctx, host, path, proxyPorts())
		},
		Publish: func(envID string, fn func(*agentv1.EnvironmentStatus)) { a.rec.UpdateStatus(envID, fn) },
		Kick:    a.kickLoop,
		Log:     log,
	})
	xds.OnRequests(a.life.OnRequests)
	a.rec = reconcile.New(rt, reconcile.Options{
		Instance:    paths.Instance(),
		ProxyName:   paths.ProxyName(),
		CA:          ca,
		Leaves:      pki.Leaves{Dir: paths.Certs()},
		Control:     ctl,
		XDS:         xds,
		ChoosePorts: choose,
		Probe:       proxy.Probe(roots),
		Relays:      relays,
		Answers:     proxy.Answers(roots),
		SourcesDir:  paths.Sources(),
		EnvsDir:     paths.Envs(),
		Bundle:      bundle(ca),
		Log:         log,
		Lifecycle:   a.life,
		WakerPort:   wakerLn.Addr().(*net.TCPAddr).Port,
	}, func(s *agentv1.EnvironmentStatus) {
		log.Info("environment", "id", s.Id, "project", s.Project, "phase", s.Phase.String(), "error", s.Error, "removed", s.Removed)
		a.hub.Publish(s)
	})

	waker := lifecycle.NewWaker(a.life, roots, proxyPorts)
	go func() {
		if err := waker.Serve(ctx, wakerLn); err != nil {
			log.Error("waker", "error", err)
		}
	}()
	go a.life.Run(ctx, 5*time.Second)

	ln, err := listen(paths.Socket())
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle(agentv1connect.NewAgentServiceHandler(api.New(a, a.hub)))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 2)
	go func() { serveErr <- srv.Serve(ln) }()

	panel, err := servePanel(mux)
	if err != nil {
		return fmt.Errorf("panel: %w", err)
	}
	a.panelURL = panel.url
	go func() { serveErr <- panel.srv.Serve(panel.ln) }()
	log.Info("agent started", "version", opt.Version, "socket", paths.Socket(), "runtime", ep.Name, "endpoint", ep.Host, "xds", xds.Port())

	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		a.loop(ctx)
	}()

	select {
	case <-ctx.Done():
	case err = <-serveErr:
	}
	a.hub.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	_ = panel.srv.Shutdown(shutdownCtx)
	<-loopDone
	_ = os.Remove(paths.Socket())
	log.Info("agent stopped")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// loop runs passes one at a time. A change of the desired state interrupts a running
// pass: a long install must not hold up 'devopus down' or another project's 'up'. Every
// step is idempotent, so the next pass picks up where the interrupted one stopped. A new
// revision with nothing else changed (a repeated 'devopus up') does not interrupt: it
// gets a pass of its own right after.
func (a *Agent) loop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.store.Changed():
		case <-a.kick:
		case <-timer.C:
		}
		desired := a.store.Snapshot()
		passCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- a.pass(passCtx, desired) }()
		var err error
		again := false
	wait:
		for {
			select {
			case err = <-done:
				break wait
			case <-a.store.Changed():
				again = true
				if !sameState(desired, a.store.Snapshot()) {
					cancel()
				}
			}
		}
		cancel()
		if ctx.Err() != nil {
			return
		}
		next := resyncInterval
		switch {
		case again:
			next = 0
		case errors.Is(err, reconcile.ErrNotReady):
			next = waitInterval
		case err != nil:
			next = retryInterval
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(next)
	}
}

// pass refreshes what the agent knows about the runtime and reconciles once.
func (a *Agent) pass(ctx context.Context, desired *agentv1.DesiredState) error {
	ctx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()
	ep := a.rt.Endpoint()
	ri := &agentv1.RuntimeInfo{Name: ep.Name, Endpoint: ep.Host}
	info, err := a.rt.Info(ctx)
	if err == nil {
		ri.Name, ri.ServerVersion, ri.Os, ri.Reachable = info.Name, info.ServerVersion, info.OS, true
	} else {
		ri.Error = fmt.Sprintf("cannot reach %s at %s: %v", ep.Name, ep.Host, err)
	}
	a.mu.Lock()
	a.rtInfo = ri
	a.mu.Unlock()
	if err != nil {
		err = errors.New(ri.Error)
		a.rec.Fail(desired, err)
		a.report(err)
		return err
	}
	err = a.rec.Reconcile(ctx, desired)
	if errors.Is(err, reconcile.ErrNotReady) || (err != nil && ctx.Err() != nil) {
		a.report(nil)
		return err
	}
	a.report(err)
	return err
}

// sameState compares desired states without their revisions.
func sameState(a, b *agentv1.DesiredState) bool {
	a, b = proto.Clone(a).(*agentv1.DesiredState), proto.Clone(b).(*agentv1.DesiredState)
	a.Revision, b.Revision = 0, 0
	return proto.Equal(a, b)
}

// bundle is the public roots of the Mozilla store plus the local CA: modules point
// SSL_CERT_FILE and friends at it and trust both public sites and their environment.
func bundle(ca *pki.CA) []byte {
	var b []byte
	for root := range fallback.Roots() {
		b = append(b, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Certificate})...)
	}
	return append(b, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Root.Raw})...)
}

// report logs an error once until it changes or clears.
func (a *Agent) report(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case err == nil && a.lastErr != nil:
		a.log.Info("reconcile recovered")
	case err != nil && (a.lastErr == nil || a.lastErr.Error() != err.Error()):
		a.log.Error("reconcile", "error", err)
	}
	a.lastErr = err
}

// listenPinned opens a loopback port and keeps it across agent restarts in file, so
// a restarted agent gives the proxy the same configuration.
func listenPinned(file string, listen func(addr string) (net.Listener, error)) (net.Listener, error) {
	if b, err := os.ReadFile(file); err == nil {
		if ln, err := listen("127.0.0.1:" + strings.TrimSpace(string(b))); err == nil {
			return ln, nil
		}
	}
	ln, err := listen("127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(file, []byte(strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)+"\n"), 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// listenXDS keeps the xDS port across agent restarts: it is baked into the proxy's
// bootstrap, and a new port would mean recreating the proxy.
type panel struct {
	ln  net.Listener
	srv *http.Server
	url string
}

// servePanel listens for the local panel on 127.0.0.1, on the same port as last time
// so open tabs keep working across agent restarts.
func servePanel(api http.Handler) (*panel, error) {
	token, err := ui.Token(paths.PanelToken())
	if err != nil {
		return nil, err
	}
	ln, err := listenPinned(paths.PanelPort(), func(addr string) (net.Listener, error) { return net.Listen("tcp", addr) })
	if err != nil {
		return nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	h := ui.New(api, token, "devopus_panel_"+paths.Instance(), port)
	return &panel{
		ln:  ln,
		srv: &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second},
		url: "http://127.0.0.1:" + strconv.Itoa(port),
	}, nil
}

func listenXDS(ctl *pki.Control) (*proxy.XDS, error) {
	file := filepath.Join(paths.Proxy(), "xds-port")
	if b, err := os.ReadFile(file); err == nil {
		if x, err := proxy.ListenXDS("127.0.0.1:"+strings.TrimSpace(string(b)), ctl); err == nil {
			return x, nil
		}
	}
	x, err := proxy.ListenXDS("127.0.0.1:0", ctl)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(file, []byte(strconv.Itoa(x.Port())+"\n"), 0o600); err != nil {
		x.Close()
		return nil, err
	}
	return x, nil
}

// listen opens the API socket, replacing a stale one left by a crashed agent. The lock
// already guarantees no other agent is serving it.
func listen(path string) (net.Listener, error) {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}
