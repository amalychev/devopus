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
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv2 "github.com/amalychev/devopus/gen/devopus/agent/v2"
	"github.com/amalychev/devopus/internal/agent/proxy"
	"github.com/amalychev/devopus/internal/agent/runtime"
	"github.com/amalychev/devopus/internal/agent/stack"
	"github.com/amalychev/devopus/internal/importer"
	"github.com/amalychev/devopus/internal/spec"
)

// LabelGeneration is the proxy generation the container was made for; a newer one in
// the desired state recreates it (devopus proxy restart).
const LabelGeneration = "com.devopus.proxy.generation"

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
func (r *Reconciler) ensureProxy(ctx context.Context, existing *runtime.Container, dev *agentv2.DeviceSettings) (*proxyState, error) {
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

	r.setProxy(func(p *agentv2.ProxyStatus) { p.State, p.Error = "starting", "" })
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

func coexistOf(dev *agentv2.DeviceSettings) *proxy.Coexist {
	if dev.GetCoexistHttpPort() == 0 && dev.GetCoexistHttpsPort() == 0 {
		return nil
	}
	return &proxy.Coexist{HTTPPort: dev.GetCoexistHttpPort(), HTTPSPort: dev.GetCoexistHttpsPort()}
}

func (r *Reconciler) setProxy(fn func(*agentv2.ProxyStatus)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(r.proxy)
}

func (r *Reconciler) proxyRunning(px *proxyState, conflicts []proxy.Conflict) {
	var relays []string
	if r.opt.Relays != nil {
		relays = r.opt.Relays.Ensure(px.ports)
	}
	r.setProxy(func(p *agentv2.ProxyStatus) {
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
	r.setProxy(func(p *agentv2.ProxyStatus) {
		p.State, p.Error = "error", err.Error()
		p.Conflicts = conflictStatus(conflicts)
		p.Container = r.opt.ProxyName
	})
	return err
}

func conflictStatus(cs []proxy.Conflict) []*agentv2.PortConflict {
	var out []*agentv2.PortConflict
	for _, c := range cs {
		out = append(out, &agentv2.PortConflict{Address: c.Addr.String(), Owner: c.Owner, Pid: int32(c.PID), Kind: c.Kind, Hint: c.Hint()})
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
	r.proxy = &agentv2.ProxyStatus{State: "absent"}
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
func (r *Reconciler) applyRoutes(ctx context.Context, desired *agentv2.DesiredState, preps []*prepared, px *proxyState) error {
	cfg := proxy.Config{HTTPPort: uint32(px.ports.HTTP), HTTPSPort: uint32(px.ports.HTTPS), Coexist: px.coexist, WakerPort: uint32(r.opt.WakerPort)}
	var routes []*agentv2.Route
	for _, p := range preps {
		if p.leaf == nil {
			continue
		}
		site, siteRoutes := site(p, px.ports)
		if isFeature(p.env) && r.opt.Lifecycle != nil && p.plan != nil && p.env.Phase != agentv2.DesiredPhase_DESIRED_PHASE_STOPPED {
			site.Sleep = &proxy.Sleep{Awake: !r.asleep(p.env)}
		}
		cfg.Sites = append(cfg.Sites, site)
		routes = append(routes, siteRoutes...)
	}
	sort.SliceStable(routes, func(i, j int) bool {
		if routes[i].Host != routes[j].Host {
			return routes[i].Host < routes[j].Host
		}
		return routes[i].Path < routes[j].Path
	})

	_, setErr := r.setConfig(ctx, cfg)
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
			r.update(env.Id, func(s *agentv2.EnvironmentStatus) {
				s.Phase, s.Error, s.AppliedRevision = agentv2.Phase_PHASE_ERROR, "routes: "+stepErr.Error(), desired.Revision
			})
			errs = append(errs, fmt.Errorf("environment %s: routes: %w", env.Id, stepErr))
			continue
		}
		// Applications are asked only after something started: a converged environment does not
		// get a request from the agent on every pass.
		sleeping := r.asleep(env)
		if env.Phase != agentv2.DesiredPhase_DESIRED_PHASE_STOPPED && !sleeping && p.plan != nil && (p.started || !r.stepDone(env.Id, "ready")) {
			if stepErr := r.runStep(ctx, env.Id, step{"ready", func(ctx context.Context) (string, error) { return r.waitAnswers(ctx, p, px.ports) }}); stepErr != nil {
				if errors.Is(stepErr, errWaiting) {
					p.waiting = true
					continue
				}
				if ctx.Err() != nil {
					continue
				}
				r.update(env.Id, func(s *agentv2.EnvironmentStatus) {
					s.Phase, s.Error, s.AppliedRevision = agentv2.Phase_PHASE_ERROR, "ready: "+stepErr.Error(), desired.Revision
				})
				errs = append(errs, fmt.Errorf("environment %s: ready: %w", env.Id, stepErr))
				continue
			}
		}
		r.update(env.Id, func(s *agentv2.EnvironmentStatus) {
			s.Error, s.Url, s.AppliedRevision = "", url, desired.Revision
			switch {
			case env.Phase == agentv2.DesiredPhase_DESIRED_PHASE_STOPPED:
				s.Phase = agentv2.Phase_PHASE_STOPPED
			case sleeping && !r.opt.Lifecycle.Settled(env.Id):
				// Going to sleep or waking up: the lifecycle manager says when it is done.
			case sleeping:
				s.Phase = agentv2.Phase_PHASE_SLEEPING
			default:
				s.Phase = agentv2.Phase_PHASE_RUNNING
				s.SleepReason, s.SleepingSince = "", nil
			}
			if isFeature(env) && r.opt.Lifecycle != nil {
				if t := r.opt.Lifecycle.LastActive(env.Id); !t.IsZero() {
					s.LastActive = timestamppb.New(t)
				}
			}
		})
	}
	return errors.Join(errs...)
}

// site is what the proxy serves for an environment: applications on their paths of the
// environment host, exposed web UIs on their own hosts, or a page of the proxy.
func site(p *prepared, ports proxy.Ports) (proxy.Site, []*agentv2.Route) {
	env := p.env
	s := proxy.Site{Name: env.Id, Project: env.Project, Hosts: p.leaf.Hosts, CertPEM: p.leaf.CertPEM, KeyPEM: p.leaf.KeyPEM}
	route := func(host, path, target string) *agentv2.Route {
		url := proxy.URL(host, ports)
		if path != "/" {
			url += path
		}
		return &agentv2.Route{Host: host, EnvironmentId: env.Id, Project: env.Project, Path: path, Target: target, Url: url}
	}
	page := func(pg proxy.Page, target string) (proxy.Site, []*agentv2.Route) {
		s.Page = pg
		var routes []*agentv2.Route
		for _, h := range p.leaf.Hosts {
			routes = append(routes, route(h, "/", target))
		}
		return s, routes
	}
	project := html.EscapeString(env.Project)
	switch {
	case env.Phase == agentv2.DesiredPhase_DESIRED_PHASE_STOPPED:
		return page(proxy.Page{Status: 503, Title: env.Project + " is stopped", Text: "Run <code>devopus up</code> in the project to start it."}, "proxy page: stopped")
	case p.plan == nil && p.err != nil:
		return page(proxy.Page{Status: 502, Title: env.Project + " did not start",
			Text: fmt.Sprintf("Step <code>%s</code> failed:</p><pre>%s</pre><p>Run <code>devopus status</code> for details.", html.EscapeString(p.failed), html.EscapeString(p.err.Error()))}, "proxy page: "+p.failed+" failed")
	case p.plan == nil:
		return page(proxy.Page{Status: 503, Title: env.Project + " is starting", Text: "Devopus is preparing environment <code>" + env.Id + "</code> of <code>" + project + "</code>."}, "proxy page: starting")
	}

	var routes []*agentv2.Route
	// A feature serves the paths of the applications it does not run from the base environment.
	units := p.plan.Units
	from := map[string]string{}
	if p.iso != nil && p.base != nil && p.base.plan != nil {
		own := map[string]bool{}
		for _, u := range units {
			own[u.Name] = true
		}
		for _, u := range p.base.plan.Units {
			if !own[u.Name] && (u.Role == stack.RoleApp && len(u.Routes) > 0 || u.Expose != "") {
				units = append(units, u)
				from[u.Name] = " from " + p.base.env.Id
			}
		}
		s.Shared = p.shared
	}
	upstream := func(u stack.Unit) proxy.Upstream {
		name := u.Name
		if from[u.Name] != "" {
			name = "base-" + u.Name
		}
		return proxy.Upstream{Name: name, Addr: netip.AddrPortFrom(u.IP, uint16(u.Port))}
	}
	main := proxy.VHost{Hosts: []string{p.hosts[0]},
		NotFound: proxy.Page{Status: 404, Title: "No application serves this path", Text: "Applications of <code>" + project + "</code> serve " + html.EscapeString(strings.Join(modulePaths(units), ", ")) + "."}}
	for _, u := range units {
		if u.Role != stack.RoleApp || len(u.Routes) == 0 {
			continue
		}
		up := upstream(u)
		up.Sleeps = from[u.Name] == ""
		for _, prefix := range u.Routes {
			main.Routes = append(main.Routes, proxy.PathRoute{Prefix: prefix, Upstream: up})
			routes = append(routes, route(p.hosts[0], prefix, fmt.Sprintf("application %s (%s)%s", u.Name, up.Addr, from[u.Name])))
		}
	}
	if len(main.Routes) == 0 {
		main.NotFound = proxy.Page{Status: 404, Title: env.Project + " has no routes", Text: "No application of <code>" + project + "</code> has <code>routes</code> in workspace.yaml."}
		routes = append(routes, route(p.hosts[0], "/", "proxy page: no routes"))
	}
	s.VHosts = append(s.VHosts, main)
	for _, u := range units {
		if u.Expose == "" || u.Port == 0 {
			continue
		}
		host := u.Expose + "." + p.hosts[0]
		up := upstream(u)
		up.Sleeps = from[u.Name] == ""
		s.VHosts = append(s.VHosts, proxy.VHost{Hosts: []string{host}, Routes: []proxy.PathRoute{{Prefix: "/", Upstream: up}}})
		routes = append(routes, route(host, "/", fmt.Sprintf("service %s (%s)%s", u.Name, up.Addr, from[u.Name])))
	}
	if sh := s.Shared; sh != nil {
		// Inside the feature: logical names of the base environment's applications and services.
		for _, t := range append(append([]proxy.SharedTarget{}, sh.Apps...), sh.Services...) {
			scheme := "tcp"
			if contains(p.iso.SharedApps, t.Name) {
				scheme = "http"
			}
			routes = append(routes, &agentv2.Route{Host: t.Name, EnvironmentId: env.Id, Project: env.Project,
				Url: fmt.Sprintf("%s://%s:%d", scheme, t.Name, t.Port), Target: fmt.Sprintf("%s (%s) from %s, inside the feature", t.Name, t.Addr, p.base.env.Id)})
		}
	}
	// After a failed step what did start stays reachable; the status says what failed.
	return s, routes
}

func modulePaths(units []stack.Unit) []string {
	var out []string
	for _, u := range units {
		if u.Role == stack.RoleApp {
			out = append(out, u.Routes...)
		}
	}
	sort.Strings(out)
	return out
}

// waitAnswers waits until every application with routes answers through the proxy. An application
// that exits fails the environment; one still starting when the time is up leaves it
// waiting, and the next pass looks again.
func (r *Reconciler) waitAnswers(ctx context.Context, p *prepared, ports proxy.Ports) (string, error) {
	const done = "every application answers"
	if r.opt.Answers == nil {
		return done, nil
	}
	var pending []stack.Unit
	for _, u := range p.plan.Units {
		if u.Role == stack.RoleApp && len(u.Routes) > 0 {
			pending = append(pending, u)
		}
	}
	deadline := time.Now().Add(r.opt.ReadyTimeout)
	for {
		var still []stack.Unit
		for _, u := range pending {
			if err := r.opt.Answers(ctx, p.hosts[0], u.Routes[0], ports); err != nil {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				still = append(still, u)
			}
		}
		pending = still
		if len(pending) == 0 {
			return done, nil
		}
		var names []string
		for _, u := range pending {
			ctr, err := r.container(ctx, u.Spec.Name)
			switch {
			case err != nil:
				return "", err
			case ctr == nil:
				return "", fmt.Errorf("the container of %s is gone", u.Name)
			case !ctr.Running:
				return "", r.explain(ctx, ctr.ID, fmt.Errorf("%s exited", u.Name))
			}
			names = append(names, fmt.Sprintf("%s on port %d", u.Name, u.Port))
		}
		msg := "waiting for " + strings.Join(names, ", ")
		if time.Now().After(deadline) {
			return msg + " (devopus logs " + pending[0].Name + ")", errWaiting
		}
		r.stepMessage(p.env.Id, "ready", msg)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
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
	ws   *spec.Project
	root string
	err  error
}

func parseProjects(projects []*agentv2.ProjectSpec) map[string]*parsed {
	out := map[string]*parsed{}
	for _, p := range projects {
		proj, _, err := importer.Parse(p.Config)
		switch {
		case err != nil:
			out[p.Name] = &parsed{err: err}
		case proj.Domain == "":
			out[p.Name] = &parsed{err: errors.New("no domain")}
		default:
			out[p.Name] = &parsed{ws: proj, root: p.Root}
		}
	}
	return out
}

// Hosts are the names an environment answers on: its own host first, then a host per
// exposed service web UI (specs/workspace-yaml.md).
func Hosts(ws *spec.Project, env *agentv2.EnvironmentSpec) []string {
	host := ws.Domain
	if env.Kind == agentv2.EnvironmentKind_ENVIRONMENT_KIND_FEATURE && env.Slug != "" {
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
