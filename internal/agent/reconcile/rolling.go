package reconcile

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"time"

	"github.com/amalychev/devopus/internal/agent/proxy"
	"github.com/amalychev/devopus/internal/agent/runtime"
	"github.com/amalychev/devopus/internal/agent/stack"
)

// Updates without downtime (R14c): on a server, an application the proxy routes to is
// replaced while it keeps serving. The new version starts next to the old one at an
// address of its own; once it accepts connections the proxy switches to it; then the
// application's own container is recreated at its pinned address, the proxy switches
// back and the stand-in goes. A version that does not come up leaves the old one
// serving.

// probeImage checks from inside the environment's network that a container accepts
// connections: the host cannot reach containers' addresses on Docker Desktop.
const probeImage = "busybox:1.37"

// standIn is the name of the container that serves while the application's is replaced.
func standIn(name string) string { return name + "-next" }

// rolling: this unit is replaced without downtime — it serves through the proxy, runs
// on a server, and its container must be recreated.
func (r *Reconciler) rolling(u stack.Unit, existing *runtime.Container) bool {
	return r.opt.Rolling && u.Role == stack.RoleApp && len(u.Routes) > 0 && u.Port > 0 &&
		existing != nil && existing.Running && existing.Labels[runtime.LabelConfig] != u.Spec.Labels[runtime.LabelConfig]
}

// replace swaps a unit's container for one of its new configuration while the proxy
// keeps sending traffic to a container that answers.
func (r *Reconciler) replace(ctx context.Context, envID string, u stack.Unit, existing *runtime.Container) error {
	r.stepMessage(envID, "apps", "starting the new "+u.Name+" next to the running one")
	next := u.Spec
	next.Name, next.IP = standIn(u.Spec.Name), netip.Addr{}
	next.Labels = withLabel(u.Spec.Labels, runtime.LabelStandIn, "true")
	if old, _ := r.container(ctx, next.Name); old != nil {
		_ = r.rt.RemoveContainer(ctx, old.ID)
	}
	if err := r.ensureContainer(ctx, next, nil); err != nil {
		return err
	}
	cleanup := func() {
		if c, _ := r.container(context.WithoutCancel(ctx), next.Name); c != nil {
			_ = r.rt.RemoveContainer(context.WithoutCancel(ctx), c.ID)
		}
	}
	nextAddr, err := r.addressOf(ctx, next.Name, next.Network)
	if err != nil {
		cleanup()
		return err
	}
	if err := r.accepting(ctx, next.Network, netip.AddrPortFrom(nextAddr, uint16(u.Port))); err != nil {
		cleanup()
		return fmt.Errorf("the new version does not come up, the running one keeps serving: %w", err)
	}
	if err := r.repoint(ctx, envID, u.Name, netip.AddrPortFrom(nextAddr, uint16(u.Port))); err != nil {
		cleanup()
		return err
	}
	r.stepMessage(envID, "apps", "the new "+u.Name+" serves; replacing its container")
	if err := r.ensureContainer(ctx, u.Spec, existing); err != nil {
		return err // the stand-in keeps serving until the next pass
	}
	if err := r.accepting(ctx, u.Spec.Network, netip.AddrPortFrom(u.IP, uint16(u.Port))); err != nil {
		return err
	}
	if err := r.repoint(ctx, envID, u.Name, netip.AddrPortFrom(u.IP, uint16(u.Port))); err != nil {
		return err
	}
	cleanup()
	return nil
}

// addressOf is a container's address in a network.
func (r *Reconciler) addressOf(ctx context.Context, name, network string) (netip.Addr, error) {
	for i := 0; i < 50; i++ {
		c, err := r.container(ctx, name)
		if err != nil {
			return netip.Addr{}, err
		}
		if c != nil {
			if a, ok := c.Networks[network]; ok && a.IsValid() {
				return a, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return netip.Addr{}, fmt.Errorf("%s has no address in its network", name)
}

// accepting waits until addr accepts TCP connections, checked by a short-lived
// container in the network.
func (r *Reconciler) accepting(ctx context.Context, network string, addr netip.AddrPort) error {
	if err := r.rt.EnsureImage(ctx, probeImage); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, unitReadyTimeout)
	defer cancel()
	for {
		ok, err := r.probe(ctx, network, addr)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s accepts no connections after %s", addr, unitReadyTimeout)
		case <-time.After(time.Second):
		}
	}
}

func (r *Reconciler) probe(ctx context.Context, network string, addr netip.AddrPort) (bool, error) {
	spec := runtime.ContainerSpec{
		Name:    fmt.Sprintf("devopus-probe-%d", time.Now().UnixNano()),
		Image:   probeImage,
		Cmd:     []string{"nc", "-z", "-w", "2", addr.Addr().String(), strconv.Itoa(int(addr.Port()))},
		Labels:  map[string]string{runtime.LabelManaged: "true", runtime.LabelInstance: r.opt.Instance, runtime.LabelRole: "probe"},
		Network: network,
	}
	id, err := r.rt.CreateContainer(ctx, spec)
	if err != nil {
		return false, err
	}
	defer func() { _ = r.rt.RemoveContainer(context.WithoutCancel(ctx), id) }()
	if err := r.rt.StartContainer(ctx, id); err != nil {
		return false, err
	}
	code, err := r.rt.WaitContainer(ctx, id)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return false, nil
		}
		return false, err
	}
	return code == 0, nil
}

// repoint sends a site's traffic for a unit to addr, without a pass, and waits until
// the proxy does.
func (r *Reconciler) repoint(ctx context.Context, envID, unit string, addr netip.AddrPort) error {
	r.setMu.Lock()
	cfg := r.lastCfg
	sites := make([]proxy.Site, len(cfg.Sites))
	copy(sites, cfg.Sites)
	found := false
	for i := range sites {
		if sites[i].Name != envID {
			continue
		}
		vhosts := make([]proxy.VHost, len(sites[i].VHosts))
		for j, vh := range sites[i].VHosts {
			routes := make([]proxy.PathRoute, len(vh.Routes))
			copy(routes, vh.Routes)
			for k := range routes {
				if routes[k].Upstream.Name == unit {
					routes[k].Upstream.Addr = addr
					found = true
				}
			}
			vh.Routes = routes
			vhosts[j] = vh
		}
		sites[i].VHosts = vhosts
	}
	if !found {
		r.setMu.Unlock()
		return nil // the proxy does not route to it yet: nothing serves to keep
	}
	cfg.Sites = sites
	version, err := r.opt.XDS.Set(ctx, cfg)
	if err == nil {
		r.lastCfg = cfg
	}
	r.setMu.Unlock()
	if err != nil {
		return err
	}
	if w, ok := r.opt.XDS.(interface {
		WaitApplied(context.Context, string) error
	}); ok {
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := w.WaitApplied(wctx, version); err != nil {
			return errors.New("the proxy did not take the switch in 10 s")
		}
	}
	return nil
}

func withLabel(labels map[string]string, k, v string) map[string]string {
	out := map[string]string{k: v}
	for key, val := range labels {
		out[key] = val
	}
	return out
}
