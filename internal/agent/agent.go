// Package agent runs the local Devcorb agent: the desired-state store, the
// reconcile loop and the local API on a unix socket (docs/architecture/agent.md).
package agent

import (
	"context"
	"crypto/x509"
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

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/devcorb/devcorb/gen/devcorb/agent/v1"
	"github.com/devcorb/devcorb/gen/devcorb/agent/v1/agentv1connect"
	"github.com/devcorb/devcorb/internal/agent/api"
	"github.com/devcorb/devcorb/internal/agent/paths"
	"github.com/devcorb/devcorb/internal/agent/pki"
	"github.com/devcorb/devcorb/internal/agent/proxy"
	"github.com/devcorb/devcorb/internal/agent/reconcile"
	"github.com/devcorb/devcorb/internal/agent/runtime"
	"github.com/devcorb/devcorb/internal/agent/store"
)

// Intervals of the reconcile loop: a periodic pass catches changes made behind the
// agent's back (a network removed by hand); after a failure it retries sooner.
const (
	resyncInterval = 30 * time.Second
	retryInterval  = 5 * time.Second
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

	mu      sync.Mutex
	rtInfo  *agentv1.RuntimeInfo
	lastErr error
}

func (a *Agent) Store() *store.Store                    { return a.store }
func (a *Agent) Statuses() []*agentv1.EnvironmentStatus { return a.rec.Statuses() }
func (a *Agent) Version() string                        { return a.opt.Version }
func (a *Agent) Proxy() *agentv1.ProxyStatus            { return a.rec.Proxy() }
func (a *Agent) Routes() []*agentv1.Route               { return a.rec.Routes() }

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

	a := &Agent{opt: opt, log: log, store: st, rt: rt, hub: api.NewHub(), ca: ca}
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
	}, func(s *agentv1.EnvironmentStatus) {
		log.Info("environment", "id", s.Id, "project", s.Project, "phase", s.Phase.String(), "error", s.Error, "removed", s.Removed)
		a.hub.Publish(s)
	})

	ln, err := listen(paths.Socket())
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle(agentv1connect.NewAgentServiceHandler(api.New(a, a.hub)))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
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
	<-loopDone
	_ = os.Remove(paths.Socket())
	log.Info("agent stopped")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (a *Agent) loop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.store.Changed():
		case <-timer.C:
		}
		next := resyncInterval
		if err := a.pass(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
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
func (a *Agent) pass(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
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
		a.rec.Fail(a.store.Snapshot(), err)
		a.report(err)
		return err
	}
	err = a.rec.Reconcile(ctx, a.store.Snapshot())
	a.report(err)
	return err
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

// listenXDS keeps the xDS port across agent restarts: it is baked into the proxy's
// bootstrap, and a new port would mean recreating the proxy.
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
