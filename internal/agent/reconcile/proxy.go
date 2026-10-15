package reconcile

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/netip"
	"sort"
	"strings"
	"sync"

	agentv1 "github.com/devcorb/devcorb/gen/devcorb/agent/v1"
	"github.com/devcorb/devcorb/internal/agent/proxy"
	"github.com/devcorb/devcorb/internal/agent/runtime"
	"github.com/devcorb/devcorb/internal/workspace"
)

// LabelGeneration is the proxy generation the container was made for; a newer one in
// the desired state recreates it (devcorb proxy restart).
const LabelGeneration = "com.devcorb.proxy.generation"

// proxyState is the running proxy as this pass knows it.
type proxyState struct {
	id       string
	ports    proxy.Ports
	networks map[string]netip.Addr // network ID → the proxy's address there
	coexist  *proxy.Coexist
}

// ensureProxy keeps one Envoy container running. Its ports stick for the container's
// life: switching between 80/443 and the fallback changes every URL, so it happens only
// when the container is recreated (a new generation, or the ports were taken while it
// was down).
func (r *Reconciler) ensureProxy(ctx context.Context, existing *runtime.Container, dev *agentv1.DeviceSettings) (*proxyState, error) {
	if r.opt.XDS == nil || r.opt.Control == nil {
		return nil, errors.New("the proxy is not configured")
	}
	gen := fmt.Sprint(dev.GetProxyGeneration())
	labels := r.labels(nil)
	labels[LabelGeneration] = gen
	coexist := coexistOf(dev)

	var ports proxy.Ports
	var conflicts []proxy.Conflict
	sticky := false
	if existing != nil && existing.Labels[LabelGeneration] == gen {
		ports, sticky = proxy.PortsOf(existing.Labels)
	}
	if !sticky && existing != nil {
		// Out of the way first, or the probe sees our own ports as taken.
		if err := r.rt.RemoveContainer(ctx, existing.ID); err != nil {
			return nil, r.proxyFailed(err, nil)
		}
		existing = nil
	}
	if !sticky && r.opt.Relays != nil {
		r.opt.Relays.Close()
	}
	choose := func() error {
		var err error
		ports, conflicts, err = r.opt.ChoosePorts(ctx)
		if err != nil {
			return r.proxyFailed(err, conflicts)
		}
		return nil
	}
	if !sticky {
		if err := choose(); err != nil {
			return nil, err
		}
	} else if ports != proxy.DefaultPorts {
		// Say why the proxy is on the fallback ports, also after an agent restart.
		conflicts = proxy.Taken(ctx, proxy.DefaultPorts)
	}

	spec, err := proxy.ContainerSpec(r.opt.ProxyName, labels, ports, r.opt.XDS.Port(), r.opt.Control)
	if err != nil {
		return nil, r.proxyFailed(err, nil)
	}
	if existing != nil && existing.Labels[runtime.LabelConfig] == spec.Labels[runtime.LabelConfig] {
		if !existing.Running {
			err := r.rt.StartContainer(ctx, existing.ID)
			if err != nil && !proxy.IsPortConflict(err) {
				return nil, r.proxyFailed(r.explain(ctx, existing.ID, err), conflicts)
			}
			if err != nil {
				// Someone took the ports while the proxy was down: probe again.
				if err := r.rt.RemoveContainer(ctx, existing.ID); err != nil {
					return nil, r.proxyFailed(err, nil)
				}
				existing = nil
				if err := choose(); err != nil {
					return nil, err
				}
				if spec, err = proxy.ContainerSpec(r.opt.ProxyName, labels, ports, r.opt.XDS.Port(), r.opt.Control); err != nil {
					return nil, r.proxyFailed(err, nil)
				}
			}
		}
		if existing != nil {
			px := &proxyState{id: existing.ID, ports: ports, networks: existing.Networks, coexist: coexist}
			r.proxyRunning(px, conflicts)
			return px, nil
		}
	} else if existing != nil {
		// Another image or bootstrap: an agent upgrade. Same ports, new container.
		if err := r.rt.RemoveContainer(ctx, existing.ID); err != nil {
			return nil, r.proxyFailed(err, nil)
		}
	}

	r.setProxy(func(p *agentv1.ProxyStatus) { p.State, p.Error = "starting", "" })
	if err := r.rt.EnsureImage(ctx, spec.Image); err != nil {
		return nil, r.proxyFailed(err, nil)
	}
	id, err := r.rt.CreateContainer(ctx, spec)
	if err != nil {
		return nil, r.proxyFailed(err, nil)
	}
	if err := r.rt.StartContainer(ctx, id); err != nil {
		err = r.explain(ctx, id, err)
		_ = r.rt.RemoveContainer(ctx, id)
		if proxy.IsPortConflict(err) {
			conflicts = proxy.Taken(ctx, ports)
		}
		return nil, r.proxyFailed(err, conflicts)
	}
	px := &proxyState{id: id, ports: ports, networks: map[string]netip.Addr{}, coexist: coexist}
	r.proxyRunning(px, conflicts)
	return px, nil
}

func coexistOf(dev *agentv1.DeviceSettings) *proxy.Coexist {
	if dev.GetCoexistHttpPort() == 0 && dev.GetCoexistHttpsPort() == 0 {
		return nil
	}
	return &proxy.Coexist{HTTPPort: dev.GetCoexistHttpPort(), HTTPSPort: dev.GetCoexistHttpsPort()}
}

func (r *Reconciler) setProxy(fn func(*agentv1.ProxyStatus)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(r.proxy)
}

func (r *Reconciler) proxyRunning(px *proxyState, conflicts []proxy.Conflict) {
	var relays []string
	if r.opt.Relays != nil {
		relays = r.opt.Relays.Ensure(px.ports)
	}
	r.setProxy(func(p *agentv1.ProxyStatus) {
		p.Relays = relays
		p.State, p.Error = "running", ""
		p.Mode = px.ports.Mode(px.coexist != nil)
		p.HttpPort, p.HttpsPort = uint32(px.ports.HTTP), uint32(px.ports.HTTPS)
		p.Conflicts = conflictStatus(conflicts)
		p.Container = r.opt.ProxyName
		p.CoexistHttpPort, p.CoexistHttpsPort = 0, 0
		if px.coexist != nil {
			p.CoexistHttpPort, p.CoexistHttpsPort = px.coexist.HTTPPort, px.coexist.HTTPSPort
		}
	})
}

func (r *Reconciler) proxyFailed(err error, conflicts []proxy.Conflict) error {
	r.setProxy(func(p *agentv1.ProxyStatus) {
		p.State, p.Error = "error", err.Error()
		p.Conflicts = conflictStatus(conflicts)
		p.Container = r.opt.ProxyName
	})
	return err
}

func conflictStatus(cs []proxy.Conflict) []*agentv1.PortConflict {
	var out []*agentv1.PortConflict
	for _, c := range cs {
		out = append(out, &agentv1.PortConflict{Address: c.Addr.String(), Owner: c.Owner, Pid: int32(c.PID), Kind: c.Kind, Hint: c.Hint()})
	}
	return out
}

// removeProxy runs when no environment is left: nothing should hold 80/443 then.
func (r *Reconciler) removeProxy(ctx context.Context, existing *runtime.Container) error {
	if existing != nil {
		if err := r.rt.RemoveContainer(ctx, existing.ID); err != nil {
			return fmt.Errorf("remove proxy: %w", err)
		}
	}
	if r.opt.Relays != nil {
		r.opt.Relays.Close()
	}
	if r.opt.XDS != nil {
		if _, err := r.opt.XDS.Set(ctx, proxy.Config{HTTPSPort: 443}); err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.proxy = &agentv1.ProxyStatus{State: "absent"}
	r.routes = nil
	r.mu.Unlock()
	return nil
}

// attachProxy puts the proxy into the environment network at its fixed address. A
// network it is already in is left alone: reconnecting changes the proxy's MAC, and
// OrbStack keeps sending replies to the old one until its ARP cache expires (P-2).
func (r *Reconciler) attachProxy(ctx context.Context, px *proxyState, nw runtime.Network) (string, error) {
	if len(nw.Subnets) == 0 {
		return "", errors.New("the network has no subnet")
	}
	ip := proxy.ProxyIP(nw.Subnets[0])
	if got, ok := px.networks[nw.ID]; ok {
		if got == ip {
			return "at " + ip.String(), nil
		}
		if err := r.rt.DisconnectNetwork(ctx, nw.ID, px.id); err != nil {
			return "", err
		}
	}
	if err := r.rt.ConnectNetwork(ctx, nw.ID, px.id, ip); err != nil {
		return "", err
	}
	px.networks[nw.ID] = ip
	return "at " + ip.String(), nil
}

// applyRoutes gives the proxy every site, waits until the new sites are served and
// finishes the environments.
func (r *Reconciler) applyRoutes(ctx context.Context, desired *agentv1.DesiredState, preps []*prepared, px *proxyState) error {
	cfg := proxy.Config{HTTPSPort: uint32(px.ports.HTTPS), Coexist: px.coexist}
	var routes []*agentv1.Route
	for _, p := range preps {
		if p.leaf == nil {
			continue
		}
		page := proxy.Page{Status: 200, Title: p.env.Project + " is up",
			Text: fmt.Sprintf("Devcorb serves environment <code>%s</code> of <code>%s</code> here. No module answers on this address yet.", p.env.Id, html.EscapeString(p.env.Project))}
		target := "proxy page: no module yet"
		if p.env.Phase == agentv1.DesiredPhase_DESIRED_PHASE_STOPPED {
			page = proxy.Page{Status: 503, Title: p.env.Project + " is stopped", Text: "Run <code>devcorb up</code> in the project to start it."}
			target = "proxy page: stopped"
		}
		cfg.Sites = append(cfg.Sites, proxy.Site{Name: p.env.Id, Project: p.env.Project, Hosts: p.leaf.Hosts, CertPEM: p.leaf.CertPEM, KeyPEM: p.leaf.KeyPEM, Page: page})
		for _, h := range p.leaf.Hosts {
			routes = append(routes, &agentv1.Route{Host: h, EnvironmentId: p.env.Id, Project: p.env.Project, Path: "/", Target: target, Url: proxy.URL(h, px.ports)})
		}
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].Host < routes[j].Host })

	_, setErr := r.opt.XDS.Set(ctx, cfg)
	if setErr == nil {
		r.mu.Lock()
		r.routes = routes
		r.mu.Unlock()
	}

	// Probe every site at once: Envoy applies the configuration in one go.
	results := make([]error, len(preps))
	var wg sync.WaitGroup
	probeCtx, cancel := context.WithTimeout(ctx, r.opt.ProbeTimeout)
	defer cancel()
	for i, p := range preps {
		if p.leaf == nil || setErr != nil || r.opt.Probe == nil || p.err != nil {
			continue
		}
		wg.Add(1)
		go func(i int, host string) {
			defer wg.Done()
			results[i] = r.opt.Probe(probeCtx, host, px.ports)
		}(i, p.hosts[0])
	}
	wg.Wait()

	var errs []error
	for i, p := range preps {
		env := p.env
		if p.err != nil {
			// Its status already says which step failed.
			continue
		}
		err := setErr
		if err == nil {
			err = results[i]
		}
		if err != nil {
			err = r.whyNotServed(err)
		}
		url := proxy.URL(p.hosts[0], px.ports)
		if stepErr := r.runStep(ctx, env.Id, step{"routes", func(context.Context) (string, error) { return url, err }}); stepErr != nil {
			r.update(env.Id, func(s *agentv1.EnvironmentStatus) {
				s.Phase, s.Error, s.AppliedRevision = agentv1.Phase_PHASE_ERROR, "routes: "+stepErr.Error(), desired.Revision
			})
			errs = append(errs, fmt.Errorf("environment %s: routes: %w", env.Id, stepErr))
			continue
		}
		r.update(env.Id, func(s *agentv1.EnvironmentStatus) {
			s.Error, s.Url, s.AppliedRevision = "", url, desired.Revision
			if env.Phase == agentv1.DesiredPhase_DESIRED_PHASE_STOPPED {
				s.Phase = agentv1.Phase_PHASE_STOPPED
			} else {
				s.Phase = agentv1.Phase_PHASE_RUNNING
			}
		})
	}
	return errors.Join(errs...)
}

// whyNotServed adds what the agent knows about Envoy to a failed probe.
func (r *Reconciler) whyNotServed(err error) error {
	st := r.opt.XDS.State()
	switch {
	case st.Rejected != "":
		return fmt.Errorf("%w; Envoy rejected the configuration: %s", err, st.Rejected)
	case !st.Connected:
		return fmt.Errorf("%w; Envoy is not connected to the agent (docker logs %s)", err, r.opt.ProxyName)
	}
	return err
}

// parsed is a project's workspace.yaml, or why it cannot be read.
type parsed struct {
	ws  *workspace.Workspace
	err error
}

func parseProjects(projects []*agentv1.ProjectSpec) map[string]*parsed {
	out := map[string]*parsed{}
	for _, p := range projects {
		res, err := workspace.Parse(p.Config)
		switch {
		case err != nil:
			out[p.Name] = &parsed{err: err}
		case res.Workspace == nil || res.Workspace.Domain == "":
			out[p.Name] = &parsed{err: errors.New("no domain")}
		default:
			out[p.Name] = &parsed{ws: res.Workspace}
		}
	}
	return out
}

// Hosts are the names an environment answers on: its own host first, then a host per
// exposed service web UI (specs/workspace-yaml.md).
func Hosts(ws *workspace.Workspace, env *agentv1.EnvironmentSpec) []string {
	host := ws.Domain
	if env.Kind == agentv1.EnvironmentKind_ENVIRONMENT_KIND_FEATURE && env.Slug != "" {
		host = env.Slug + "." + ws.Domain
	}
	out := []string{host}
	var names []string
	for _, svc := range ws.Services {
		if svc != nil && svc.Expose != "" {
			names = append(names, svc.Expose)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		out = append(out, n+"."+host)
	}
	return out
}

func joinHosts(hosts []string) string {
	if len(hosts) <= 3 {
		return strings.Join(hosts, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(hosts[:2], ", "), len(hosts)-2)
}
