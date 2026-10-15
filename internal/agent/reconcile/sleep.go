package reconcile

import (
	"context"
	"errors"
	"time"

	agentv1 "github.com/devopus/devopus/gen/devopus/agent/v1"
	"github.com/devopus/devopus/internal/agent/lifecycle"
	"github.com/devopus/devopus/internal/agent/proxy"
	"github.com/devopus/devopus/internal/agent/stack"
)

// Lifecycle is the part of the lifecycle manager the reconciler uses: it does not start
// what sleeps, and tells the manager what runs.
type Lifecycle interface {
	Asleep(envID string) bool
	// Settled: no sleep or wake in progress; the manager reports the phase until then.
	Settled(envID string) bool
	Update(envs []lifecycle.Env)
	LastActive(envID string) time.Time
}

// asleep: a feature whose containers the lifecycle manager stopped. The reconciler
// keeps its network, routes and worktrees and leaves its containers alone.
func (r *Reconciler) asleep(env *agentv1.EnvironmentSpec) bool {
	return isFeature(env) && r.opt.Lifecycle != nil && r.opt.Lifecycle.Asleep(env.Id)
}

// SetAwake sends a feature's traffic to its containers or to the waker, without a pass,
// and waits until the proxy does (P-7: switch first, then stop).
func (r *Reconciler) SetAwake(ctx context.Context, envID string, awake bool) error {
	r.setMu.Lock()
	cfg := r.lastCfg
	sites := make([]proxy.Site, len(cfg.Sites))
	copy(sites, cfg.Sites)
	found := false
	for i := range sites {
		if sites[i].Name == envID && sites[i].Sleep != nil {
			sites[i].Sleep = &proxy.Sleep{Awake: awake}
			found = true
		}
	}
	if !found {
		r.setMu.Unlock()
		return errors.New("the proxy does not serve this feature yet")
	}
	cfg.Sites = sites
	version, err := r.opt.XDS.Set(ctx, cfg)
	if err == nil {
		r.lastCfg = cfg
	}
	r.setMu.Unlock()
	r.opt.Log.Debug("sleep switch", "env", envID, "awake", awake, "version", version)
	if err != nil {
		return err
	}
	if w, ok := r.opt.XDS.(interface {
		WaitApplied(context.Context, string) error
	}); ok {
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := w.WaitApplied(wctx, version); err != nil {
			return errors.New("the proxy did not apply the change in 5 s")
		}
	}
	return nil
}

// setConfig gives the proxy a pass's configuration with the sleep state as it is now:
// the lifecycle manager may have switched a feature since the pass built its sites.
func (r *Reconciler) setConfig(ctx context.Context, cfg proxy.Config) (string, error) {
	r.setMu.Lock()
	defer r.setMu.Unlock()
	for i := range cfg.Sites {
		if cfg.Sites[i].Sleep != nil {
			cfg.Sites[i].Sleep = &proxy.Sleep{Awake: r.opt.Lifecycle == nil || !r.opt.Lifecycle.Asleep(cfg.Sites[i].Name)}
		}
	}
	version, err := r.opt.XDS.Set(ctx, cfg)
	if err == nil {
		r.lastCfg = cfg
	}
	return version, err
}

// tellLifecycle gives the lifecycle manager the running features of this pass.
func (r *Reconciler) tellLifecycle(preps []*prepared) {
	if r.opt.Lifecycle == nil {
		return
	}
	var envs []lifecycle.Env
	for _, p := range preps {
		env := p.env
		if !isFeature(env) || env.Phase == agentv1.DesiredPhase_DESIRED_PHASE_STOPPED || p.plan == nil || p.ws == nil || len(p.hosts) == 0 {
			continue
		}
		pol := p.ws.Policy()
		sleepAfter, _ := time.ParseDuration(pol.SleepAfter)
		e := lifecycle.Env{
			ID: env.Id, Project: env.Project, Name: env.Name, Host: p.hosts[0],
			SleepAfter: sleepAfter, MaxRunning: pol.MaxRunning,
			Ready: r.asleep(env) || (p.err == nil && r.stepDone(env.Id, "ready")),
		}
		for _, u := range p.plan.Units {
			lu := lifecycle.Unit{Name: u.Name, Container: u.Spec.Name, Role: u.Role, Healthcheck: u.Healthcheck}
			if u.Role == stack.RoleModule && len(u.Routes) > 0 {
				lu.Path = u.Routes[0]
			}
			e.Units = append(e.Units, lu)
		}
		for _, dir := range p.sources {
			e.Dirs = append(e.Dirs, dir)
		}
		envs = append(envs, e)
	}
	r.opt.Lifecycle.Update(envs)
}
