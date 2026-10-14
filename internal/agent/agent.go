// Package agent runs the local Docklet agent: the desired-state store, the
// reconcile loop and the local API on a unix socket (docs/architecture/agent.md).
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	agentv1 "github.com/docklet/docklet/gen/docklet/agent/v1"
	"github.com/docklet/docklet/gen/docklet/agent/v1/agentv1connect"
	"github.com/docklet/docklet/internal/agent/api"
	"github.com/docklet/docklet/internal/agent/paths"
	"github.com/docklet/docklet/internal/agent/reconcile"
	"github.com/docklet/docklet/internal/agent/runtime"
	"github.com/docklet/docklet/internal/agent/store"
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
}

type Agent struct {
	opt   Options
	log   *slog.Logger
	store *store.Store
	rt    *runtime.Docker
	rec   *reconcile.Reconciler
	hub   *api.Hub

	mu      sync.Mutex
	rtInfo  *agentv1.RuntimeInfo
	lastErr error
}

func (a *Agent) Store() *store.Store                    { return a.store }
func (a *Agent) Statuses() []*agentv1.EnvironmentStatus { return a.rec.Statuses() }
func (a *Agent) Version() string                        { return a.opt.Version }

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

	a := &Agent{opt: opt, log: log, store: st, rt: rt, hub: api.NewHub()}
	a.rec = reconcile.New(rt, func(s *agentv1.EnvironmentStatus) {
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
	log.Info("agent started", "version", opt.Version, "socket", paths.Socket(), "runtime", ep.Name, "endpoint", ep.Host)

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
