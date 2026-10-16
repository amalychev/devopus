// Package reconcile brings the runtime to the desired state (ADR-0001,
// docs/architecture/agent.md): observe by labels, run idempotent steps per
// environment, collect garbage the desired state no longer has.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv2 "github.com/amalychev/devopus/gen/devopus/agent/v2"
	"github.com/amalychev/devopus/internal/agent/git"
	"github.com/amalychev/devopus/internal/agent/pki"
	"github.com/amalychev/devopus/internal/agent/proxy"
	"github.com/amalychev/devopus/internal/agent/runtime"
	"github.com/amalychev/devopus/internal/agent/stack"
	"github.com/amalychev/devopus/internal/spec"
)

// Runtime is the part of the container runtime the reconciler uses.
type Runtime interface {
	Networks(ctx context.Context) ([]runtime.Network, error)
	CreateNetwork(ctx context.Context, name string, subnet netip.Prefix, labels map[string]string) (string, error)
	RemoveNetwork(ctx context.Context, id string) error
	Containers(ctx context.Context, instance string) ([]runtime.Container, error)
	EnsureImage(ctx context.Context, ref string) error
	CreateContainer(ctx context.Context, spec runtime.ContainerSpec) (string, error)
	StartContainer(ctx context.Context, id string) error
	RemoveContainer(ctx context.Context, id string) error
	ConnectNetwork(ctx context.Context, networkID, containerID string, ip netip.Addr) error
	DisconnectNetwork(ctx context.Context, networkID, containerID string) error
	ContainerLogs(ctx context.Context, id string, lines int) string
	Exec(ctx context.Context, id, user string, cmd []string) (runtime.ExecResult, error)
	StopContainer(ctx context.Context, id string) error
	WaitContainer(ctx context.Context, id string) (int, error)
	CopyLogs(ctx context.Context, id string, w io.Writer) error
	Volumes(ctx context.Context, instance string) ([]runtime.Volume, error)
	CreateVolume(ctx context.Context, name string, labels map[string]string) error
	RemoveVolume(ctx context.Context, name string) error
	ImageExists(ctx context.Context, ref string) (bool, error)
	Build(ctx context.Context, spec runtime.BuildSpec, log io.Writer) error
	Images(ctx context.Context, instance string) ([]runtime.Image, error)
	RemoveImage(ctx context.Context, id string) error
}

// XDS is the configuration server Envoy follows.
type XDS interface {
	Set(ctx context.Context, cfg proxy.Config) (string, error)
	State() proxy.XDSState
	Port() int
}

// Pool is the address pool of environment networks: 256 networks of /24 that do not
// collide with Docker's default 172.17–172.31 or typical corporate VPNs (ADR-0006).
var Pool = netip.MustParsePrefix("10.213.0.0/16")

// Options are the reconciler's collaborators besides the runtime.
type Options struct {
	// Instance labels every resource; resources of other instances are invisible.
	Instance string
	// Decrypt opens the secrets of a project of the workspace server (ADR-0009).
	// Projects of folders read theirs from .env.secrets.
	Decrypt func(*agentv2.ProjectSpec) (map[string]string, error)
	// ProxyName is the name of the Envoy container.
	ProxyName string
	CA        *pki.CA
	Leaves    pki.Leaves
	Control   *pki.Control
	XDS       XDS
	// ChoosePorts probes the host and picks the proxy's ports.
	ChoosePorts func(ctx context.Context) (proxy.Ports, []proxy.Conflict, error)
	// Probe checks that the proxy serves host; nil skips the check.
	Probe func(ctx context.Context, host string, ports proxy.Ports) error
	// ProbeTimeout bounds how long a pass waits for Envoy to serve a new site.
	ProbeTimeout time.Duration
	// Relays covers [::1] where the runtime publishes 127.0.0.1 only; nil skips it.
	Relays *proxy.Relays
	// Answers checks once that an application answers through the proxy; nil skips the wait.
	Answers func(ctx context.Context, host, path string, ports proxy.Ports) error
	// ReadyTimeout bounds how long one pass waits for applications to answer.
	ReadyTimeout time.Duration
	// SourcesDir keeps clones of application repositories; EnvsDir hook marks and logs.
	SourcesDir string
	EnvsDir    string
	// Bundle is the public roots plus the local CA, for applications' TLS clients.
	Bundle []byte
	// Lifecycle puts features to sleep; WakerPort is where the proxy finds its waker.
	// Without them features never sleep.
	Lifecycle Lifecycle
	WakerPort int
	Log       *slog.Logger
}

type Reconciler struct {
	rt      Runtime
	opt     Options
	pool    netip.Prefix
	publish func(*agentv2.EnvironmentStatus)
	now     func() time.Time

	mu     sync.Mutex
	status map[string]*agentv2.EnvironmentStatus
	// forced: environments whose worktrees go even with work in them.
	forced map[string]bool
	// merged and mergeAt: whether a feature's branch is merged, and when that was checked.
	merged  map[string]bool
	mergeAt map[string]time.Time
	// needGolden: per project, the databases running features copy; set by each pass.
	needGolden map[string]map[string]bool

	// setMu orders the proxy configurations of passes and of sleep switches; lastCfg is
	// the one the proxy has.
	setMu   sync.Mutex
	lastCfg proxy.Config
	proxy   *agentv2.ProxyStatus
	routes  []*agentv2.Route
}

// New makes a reconciler; publish receives a copy of every status that changed.
func New(rt Runtime, opt Options, publish func(*agentv2.EnvironmentStatus)) *Reconciler {
	if publish == nil {
		publish = func(*agentv2.EnvironmentStatus) {}
	}
	if opt.ProbeTimeout == 0 {
		opt.ProbeTimeout = 30 * time.Second
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	// Never relative to wherever the agent runs.
	if opt.SourcesDir == "" {
		opt.SourcesDir = filepath.Join(os.TempDir(), "devopus", "src")
	}
	if opt.EnvsDir == "" {
		opt.EnvsDir = filepath.Join(os.TempDir(), "devopus", "envs")
	}
	if opt.ReadyTimeout == 0 {
		opt.ReadyTimeout = 90 * time.Second
	}
	return &Reconciler{rt: rt, opt: opt, pool: Pool, publish: publish, now: time.Now,
		status: map[string]*agentv2.EnvironmentStatus{}, forced: map[string]bool{}, merged: map[string]bool{}, mergeAt: map[string]time.Time{}, proxy: &agentv2.ProxyStatus{State: "absent"}}
}

// Statuses returns copies of the current statuses, ordered by project and ID.
func (r *Reconciler) Statuses() []*agentv2.EnvironmentStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*agentv2.EnvironmentStatus, 0, len(r.status))
	for _, s := range r.status {
		out = append(out, proto.Clone(s).(*agentv2.EnvironmentStatus))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		return out[i].Id < out[j].Id
	})
	return out
}

// Proxy returns the proxy's status, with the xDS link as it is right now.
func (r *Reconciler) Proxy() *agentv2.ProxyStatus {
	r.mu.Lock()
	p := proto.Clone(r.proxy).(*agentv2.ProxyStatus)
	r.mu.Unlock()
	if r.opt.XDS != nil {
		st := r.opt.XDS.State()
		p.Connected, p.ConfigVersion, p.Rejected = st.Connected, st.Version, st.Rejected
	}
	return p
}

// Routes returns the routing table the proxy was last given.
func (r *Reconciler) Routes() []*agentv2.Route {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*agentv2.Route, len(r.routes))
	for i, rt := range r.routes {
		out[i] = proto.Clone(rt).(*agentv2.Route)
	}
	return out
}

// NetworkName is the network of an environment; the same name is the compose project.
func NetworkName(project, envID string) string { return project + "-env-" + envID }

// DNSName is the DNS container of an environment.
func DNSName(project, envID string) string { return NetworkName(project, envID) + "-dns" }

// observed is what one pass sees in the runtime.
type observed struct {
	nets    []runtime.Network
	managed map[string]runtime.Network // by environment ID
	byEnv   map[string][]runtime.Container
	volumes map[string][]runtime.Volume // by environment ID
	proxy   *runtime.Container
}

func (r *Reconciler) observe(ctx context.Context) (*observed, error) {
	nets, err := r.rt.Networks(ctx)
	if err != nil {
		return nil, fmt.Errorf("observe networks: %w", err)
	}
	ctrs, err := r.rt.Containers(ctx, r.opt.Instance)
	if err != nil {
		return nil, fmt.Errorf("observe containers: %w", err)
	}
	vols, err := r.rt.Volumes(ctx, r.opt.Instance)
	if err != nil {
		return nil, fmt.Errorf("observe volumes: %w", err)
	}
	o := &observed{nets: nets, managed: map[string]runtime.Network{}, byEnv: map[string][]runtime.Container{}, volumes: map[string][]runtime.Volume{}}
	for _, v := range vols {
		if r.ours(v.Labels) && v.Labels[runtime.LabelEnv] != "" {
			o.volumes[v.Labels[runtime.LabelEnv]] = append(o.volumes[v.Labels[runtime.LabelEnv]], v)
		}
	}
	for _, n := range nets {
		if r.ours(n.Labels) && n.Labels[runtime.LabelEnv] != "" {
			o.managed[n.Labels[runtime.LabelEnv]] = n
		}
	}
	for i, c := range ctrs {
		if !r.ours(c.Labels) {
			continue
		}
		switch {
		case c.Labels[runtime.LabelRole] == "proxy":
			o.proxy = &ctrs[i]
		case c.Labels[runtime.LabelEnv] != "":
			o.byEnv[c.Labels[runtime.LabelEnv]] = append(o.byEnv[c.Labels[runtime.LabelEnv]], c)
		}
	}
	return o, nil
}

func (r *Reconciler) ours(labels map[string]string) bool {
	return labels[runtime.LabelManaged] == "true" && labels[runtime.LabelInstance] == r.opt.Instance
}

func (r *Reconciler) labels(env *agentv2.EnvironmentSpec) map[string]string {
	l := map[string]string{runtime.LabelManaged: "true", runtime.LabelInstance: r.opt.Instance}
	if env != nil {
		l[runtime.LabelProject] = env.Project
		l[runtime.LabelEnv] = env.Id
		l[runtime.LabelKind] = kindLabel(env.Kind)
	}
	return l
}

// Reconcile runs one pass. Errors of single environments end up in their status; the
// returned error means the pass could not observe the runtime or left work undone.
func (r *Reconciler) Reconcile(ctx context.Context, desired *agentv2.DesiredState) error {
	o, err := r.observe(ctx)
	if err != nil {
		return err
	}
	var errs []error
	envs := append([]*agentv2.EnvironmentSpec{}, desired.Environments...)
	// Base environments first: their features reach their applications and services.
	sort.Slice(envs, func(i, j int) bool {
		if envs[i].Kind != envs[j].Kind {
			return envs[i].Kind == agentv2.EnvironmentKind_ENVIRONMENT_KIND_BASE
		}
		return envs[i].Id < envs[j].Id
	})
	workspaces := parseProjects(desired.Projects)

	// The proxy serves every environment of the device; it exists while any does.
	var px *proxyState
	var proxyErr error
	if len(envs) > 0 {
		px, proxyErr = r.ensureProxy(ctx, o.proxy, desired.Device)
		if proxyErr != nil {
			errs = append(errs, fmt.Errorf("proxy: %w", proxyErr))
		}
	}

	r.needGolden = map[string]map[string]bool{}
	for _, env := range envs {
		ws := workspaces[env.Project]
		if !isFeature(env) || env.Phase == agentv2.DesiredPhase_DESIRED_PHASE_STOPPED || ws == nil || ws.ws == nil {
			continue
		}
		if iso, err := stack.Feature(ws.ws, env.ForkedApps); err == nil {
			if r.needGolden[env.Project] == nil {
				r.needGolden[env.Project] = map[string]bool{}
			}
			for _, svc := range iso.Services {
				r.needGolden[env.Project][svc] = true
			}
		}
	}

	wanted := map[string]bool{}
	var preps []*prepared
	bases := map[string]*prepared{}
	for _, env := range envs {
		wanted[env.Id] = true
		p := r.prepareEnv(ctx, desired.Revision, env, workspaces[env.Project], o, px, proxyErr, bases[env.Project])
		if env.Kind == agentv2.EnvironmentKind_ENVIRONMENT_KIND_BASE {
			bases[env.Project] = p
		}
		if p.err != nil {
			errs = append(errs, fmt.Errorf("environment %s: %w", env.Id, p.err))
		}
		preps = append(preps, p)
	}

	if px != nil {
		if err := r.applyRoutes(ctx, desired, preps, px); err != nil {
			errs = append(errs, err)
		}
	}
	r.tellLifecycle(preps)

	errs = append(errs, r.collect(ctx, desired.Revision, wanted, o, px)...)
	r.collectImages(ctx, desired, preps)
	if len(envs) == 0 {
		if err := r.removeProxy(ctx, o.proxy); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		for _, p := range preps {
			if p.waiting {
				return ErrNotReady
			}
		}
	}
	return errors.Join(errs...)
}

// step is one idempotent part of bringing an environment up.
type step struct {
	name string
	run  func(ctx context.Context) (string, error)
}

// prepared is an environment brought up to its routes, or stopped by err on the way.
// With a certificate the proxy keeps serving its site even if a later step failed.
type prepared struct {
	env   *agentv2.EnvironmentSpec
	hosts []string
	leaf  *pki.Leaf
	ws    *spec.Project
	admin string
	// project is the project as registered; its secrets for one of the server.
	project *agentv2.ProjectSpec
	url     string
	plan    *stack.Plan
	err     error
	// failed is the step that stopped the environment.
	failed string
	// waiting: applications are starting; the environment is not running yet.
	waiting bool
	// started: this pass started or restarted a container of the environment.
	started bool
	// sources: where the code of each application is.
	sources map[string]string
	// Features: the base environment, what the feature runs itself and how it reaches
	// the rest.
	base   *prepared
	iso    *stack.Isolation
	shared *proxy.Shared
	// notes for people, from the isolation step.
	notes []string
}

func isFeature(env *agentv2.EnvironmentSpec) bool {
	return env.Kind == agentv2.EnvironmentKind_ENVIRONMENT_KIND_FEATURE
}

func (r *Reconciler) prepareEnv(ctx context.Context, rev uint64, env *agentv2.EnvironmentSpec, ws *parsed, o *observed, px *proxyState, proxyErr error, base *prepared) *prepared {
	r.update(env.Id, func(s *agentv2.EnvironmentStatus) {
		s.Project, s.Kind, s.Slug = env.Project, env.Kind, env.Slug
		s.Name, s.Branch, s.ForkedApps = env.Name, env.Branch, env.ForkedApps
		if s.Phase == agentv2.Phase_PHASE_UNSPECIFIED {
			s.Phase = agentv2.Phase_PHASE_CREATING
		}
	})
	p := &prepared{env: env, base: base}
	var nw runtime.Network
	steps := []step{
		{"network", func(ctx context.Context) (string, error) {
			n, err := r.ensureNetwork(ctx, env, o)
			nw = n
			return describe(n.Subnets), err
		}},
		{"certificate", func(ctx context.Context) (string, error) {
			if ws == nil {
				return "", errors.New("the project is not in the desired state")
			}
			if ws.err != nil {
				return "", fmt.Errorf("workspace.yaml: %w", ws.err)
			}
			p.ws = ws.ws
			p.hosts = Hosts(ws.ws, env)
			leaf, msg, err := r.ensureCertificate(env.Id, p.hosts)
			p.leaf = leaf
			return msg, err
		}},
		{"proxy", func(ctx context.Context) (string, error) {
			if px == nil {
				return "", proxyErr
			}
			p.url = proxy.URL(p.hosts[0], px.ports)
			return r.attachProxy(ctx, px, nw)
		}},
	}
	if env.Phase == agentv2.DesiredPhase_DESIRED_PHASE_STOPPED {
		// A stopped environment keeps its network and data; its DNS server has no clients.
		err := r.stopUnits(ctx, o.byEnv[env.Id])
		if err == nil {
			err = r.removeAll(ctx, o.byEnv[env.Id], "dns")
		}
		defer r.setUnits(ctx, env.Id, nil, nil)
		if err != nil {
			p.err = err
			r.update(env.Id, func(s *agentv2.EnvironmentStatus) {
				s.Phase, s.Error, s.AppliedRevision = agentv2.Phase_PHASE_ERROR, "dns: "+err.Error(), rev
			})
			return p
		}
	} else {
		if isFeature(env) {
			steps = append(steps, step{"isolation", func(ctx context.Context) (string, error) {
				return r.isolate(env, p, nw, px)
			}})
		}
		steps = append(steps, step{"dns", func(ctx context.Context) (string, error) {
			var names []string
			if p.shared != nil {
				for _, t := range append(append([]proxy.SharedTarget{}, p.shared.Apps...), p.shared.Services...) {
					names = append(names, t.Name)
				}
			}
			return r.ensureDNS(ctx, env, nw, o.byEnv[env.Id], names)
		}})
		root := ""
		if ws != nil {
			root, p.admin, p.project = ws.root, ws.admin, ws.project
		}
		steps = append(steps, r.unitSteps(env, root, p, &nw, o)...)
		defer func() { r.setUnits(ctx, env.Id, p.plan, p.hosts) }()
	}
	for _, st := range steps {
		if err := r.runStep(ctx, env.Id, st); err != nil {
			p.err, p.failed = err, st.name
			if ctx.Err() != nil {
				// Interrupted by a newer desired state, not failed: the next pass goes on.
				return p
			}
			r.update(env.Id, func(s *agentv2.EnvironmentStatus) {
				s.Phase = agentv2.Phase_PHASE_ERROR
				s.Error = st.name + ": " + err.Error()
				s.AppliedRevision = rev
			})
			return p
		}
	}
	return p
}

// setUnits publishes the units of an environment as they are now.
func (r *Reconciler) setUnits(ctx context.Context, envID string, plan *stack.Plan, hosts []string) {
	exposed := map[string]string{}
	if plan != nil && len(hosts) > 0 {
		for _, u := range plan.Units {
			if u.Expose != "" {
				exposed[u.Name] = u.Expose + "." + hosts[0]
			}
		}
	}
	units := r.unitStatuses(context.WithoutCancel(ctx), envID, plan, exposed)
	r.update(envID, func(s *agentv2.EnvironmentStatus) { s.Units = units })
}

// runStep records progress only on transitions, so a converged environment does not
// produce events on every pass.
func (r *Reconciler) runStep(ctx context.Context, envID string, st step) error {
	r.update(envID, func(s *agentv2.EnvironmentStatus) {
		p := findStep(s, st.name)
		if p.State != agentv2.StepState_STEP_STATE_DONE && p.State != agentv2.StepState_STEP_STATE_RUNNING {
			p.State, p.Message = agentv2.StepState_STEP_STATE_RUNNING, ""
			if p.StartedAt == nil {
				p.StartedAt = timestamppb.New(r.now())
			}
		}
	})
	msg, err := st.run(ctx)
	if err != nil && ctx.Err() != nil {
		return err
	}
	r.update(envID, func(s *agentv2.EnvironmentStatus) {
		p := findStep(s, st.name)
		switch {
		case errors.Is(err, errWaiting):
			p.Message = msg
		case err != nil:
			if p.State != agentv2.StepState_STEP_STATE_FAILED || p.Message != err.Error() {
				p.State, p.Message, p.FinishedAt = agentv2.StepState_STEP_STATE_FAILED, err.Error(), timestamppb.New(r.now())
			}
		case p.State != agentv2.StepState_STEP_STATE_DONE || p.Message != msg:
			p.State, p.Message, p.FinishedAt = agentv2.StepState_STEP_STATE_DONE, msg, timestamppb.New(r.now())
		}
	})
	return err
}

// stepDone reports whether a step of an environment finished successfully last time.
func (r *Reconciler) stepDone(envID, name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.status[envID]; ok {
		for _, p := range s.Steps {
			if p.Name == name {
				return p.State == agentv2.StepState_STEP_STATE_DONE
			}
		}
	}
	return false
}

func findStep(s *agentv2.EnvironmentStatus, name string) *agentv2.StepProgress {
	for _, p := range s.Steps {
		if p.Name == name {
			return p
		}
	}
	p := &agentv2.StepProgress{Name: name, State: agentv2.StepState_STEP_STATE_PENDING}
	s.Steps = append(s.Steps, p)
	return p
}

func (r *Reconciler) ensureNetwork(ctx context.Context, env *agentv2.EnvironmentSpec, o *observed) (runtime.Network, error) {
	name := NetworkName(env.Project, env.Id)
	if n, ok := o.managed[env.Id]; ok {
		r.setNetwork(env.Id, n.Name, n.Subnets)
		return n, nil
	}
	var used []netip.Prefix
	for _, n := range o.nets {
		used = append(used, n.Subnets...)
	}
	// Another runtime user may take a subnet between our observation and the create.
	for attempt := 0; attempt < 8; attempt++ {
		subnet, ok := FreeSubnet(r.pool, used)
		if !ok {
			return runtime.Network{}, fmt.Errorf("address pool %s is exhausted (256 environments)", r.pool)
		}
		id, err := r.rt.CreateNetwork(ctx, name, subnet, r.labels(env))
		if errors.Is(err, runtime.ErrSubnetTaken) {
			used = append(used, subnet)
			continue
		}
		if err != nil {
			return runtime.Network{}, err
		}
		n := runtime.Network{ID: id, Name: name, Subnets: []netip.Prefix{subnet}, Labels: r.labels(env)}
		o.managed[env.Id] = n
		o.nets = append(o.nets, n)
		r.setNetwork(env.Id, name, n.Subnets)
		return n, nil
	}
	return runtime.Network{}, errors.New("no free subnet after 8 attempts")
}

func (r *Reconciler) setNetwork(envID, name string, subnets []netip.Prefix) {
	r.update(envID, func(s *agentv2.EnvironmentStatus) {
		s.Network = &agentv2.NetworkStatus{Name: name, Subnet: describe(subnets)}
	})
}

// ensureCertificate reuses the stored certificate while it covers the hosts and is far
// from expiry; otherwise it issues a new one. The proxy picks it up over SDS.
func (r *Reconciler) ensureCertificate(envID string, hosts []string) (*pki.Leaf, string, error) {
	if r.opt.CA == nil {
		return nil, "", errors.New("no local CA")
	}
	leaf := r.opt.Leaves.Load(envID)
	if !leaf.Fresh(hosts, r.now()) {
		var err error
		if leaf, err = r.opt.CA.Issue(hosts); err != nil {
			return nil, "", err
		}
		if err := r.opt.Leaves.Save(envID, leaf); err != nil {
			return nil, "", err
		}
	}
	return leaf, fmt.Sprintf("%s (until %s)", joinHosts(leaf.Hosts), leaf.NotAfter.Format("2006-01-02")), nil
}

func (r *Reconciler) ensureDNS(ctx context.Context, env *agentv2.EnvironmentSpec, nw runtime.Network, ctrs []runtime.Container, shared []string) (string, error) {
	if len(nw.Subnets) == 0 {
		return "", errors.New("the network has no subnet")
	}
	spec := proxy.DNSSpec(DNSName(env.Project, env.Id), nw.ID, nw.Subnets[0], r.labels(env), shared)
	ip := proxy.DNSIP(nw.Subnets[0]).String()
	if err := r.ensureContainer(ctx, spec, pick(ctrs, "dns")); err != nil {
		return "", err
	}
	return ip, nil
}

// ensureContainer makes a container match spec: recreated when its config label
// differs, started when stopped, created when missing.
func (r *Reconciler) ensureContainer(ctx context.Context, spec runtime.ContainerSpec, existing *runtime.Container) error {
	if existing != nil && existing.Labels[runtime.LabelConfig] == spec.Labels[runtime.LabelConfig] {
		if existing.Running {
			return nil
		}
		if err := r.rt.StartContainer(ctx, existing.ID); err != nil {
			return r.explain(ctx, existing.ID, err)
		}
		return nil
	}
	if existing != nil {
		if err := r.rt.RemoveContainer(ctx, existing.ID); err != nil {
			return err
		}
	}
	if err := r.rt.EnsureImage(ctx, spec.Image); err != nil {
		return err
	}
	id, err := r.rt.CreateContainer(ctx, spec)
	if err != nil {
		return err
	}
	if err := r.rt.StartContainer(ctx, id); err != nil {
		return r.explain(ctx, id, err)
	}
	return nil
}

// explain adds the container's last output to a start error.
func (r *Reconciler) explain(ctx context.Context, id string, err error) error {
	if logs := r.rt.ContainerLogs(ctx, id, 5); logs != "" {
		return fmt.Errorf("%w\n%s", err, logs)
	}
	return err
}

func pick(ctrs []runtime.Container, role string) *runtime.Container {
	for i := range ctrs {
		if ctrs[i].Labels[runtime.LabelRole] == role {
			return &ctrs[i]
		}
	}
	return nil
}

func (r *Reconciler) removeAll(ctx context.Context, ctrs []runtime.Container, role string) error {
	var errs []error
	for _, c := range ctrs {
		if role == "" || c.Labels[runtime.LabelRole] == role {
			errs = append(errs, r.rt.RemoveContainer(ctx, c.ID))
		}
	}
	return errors.Join(errs...)
}

// collect removes what belongs to environments that left the desired state: their
// containers, the proxy's link to their network, the network and the certificate.
func (r *Reconciler) collect(ctx context.Context, rev uint64, wanted map[string]bool, o *observed, px *proxyState) []error {
	var errs []error
	gone := map[string]string{} // env → project
	feature := map[string]bool{}
	for id, n := range o.managed {
		if !wanted[id] {
			gone[id] = n.Labels[runtime.LabelProject]
			feature[id] = n.Labels[runtime.LabelKind] == "feature"
		}
	}
	for id, ctrs := range o.byEnv {
		if !wanted[id] && len(ctrs) > 0 {
			gone[id] = ctrs[0].Labels[runtime.LabelProject]
			feature[id] = ctrs[0].Labels[runtime.LabelKind] == "feature"
		}
	}
	// Features first: their worktrees must be gone before the base's clones they live in.
	ids := make([]string, 0, len(gone))
	for id := range gone {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if feature[ids[i]] != feature[ids[j]] {
			return feature[ids[i]]
		}
		return ids[i] < ids[j]
	})
	for _, id := range ids {
		project := gone[id]
		r.update(id, func(s *agentv2.EnvironmentStatus) {
			if s.Project == "" {
				s.Project = project
			}
			s.Phase = agentv2.Phase_PHASE_DELETING
		})
		err := r.removeAll(ctx, o.byEnv[id], "")
		for _, v := range o.volumes[id] {
			if err == nil {
				err = r.rt.RemoveVolume(ctx, v.Name)
			}
		}
		if n, ok := o.managed[id]; ok && err == nil {
			proxyID := ""
			if px != nil {
				proxyID = px.id
			} else if o.proxy != nil {
				proxyID = o.proxy.ID
			}
			if proxyID != "" {
				err = r.rt.DisconnectNetwork(ctx, n.ID, proxyID)
			}
			if err == nil {
				err = r.rt.RemoveNetwork(ctx, n.ID)
			}
		}
		if err == nil {
			err = r.opt.Leaves.Remove(id)
		}
		if err == nil {
			err = r.removeFiles(ctx, project, id)
		}
		if err != nil {
			r.update(id, func(s *agentv2.EnvironmentStatus) { s.Error = "remove: " + err.Error() })
			errs = append(errs, fmt.Errorf("gc %s: %w", id, err))
			continue
		}
		r.forget(id, rev)
	}
	// Certificates of environments that are gone from everywhere else.
	for _, name := range r.opt.Leaves.Names() {
		if !wanted[name] {
			_ = r.opt.Leaves.Remove(name)
		}
	}
	// Environments that were wanted but never got resources.
	for _, s := range r.Statuses() {
		if !wanted[s.Id] {
			if _, pending := gone[s.Id]; !pending {
				r.forget(s.Id, rev)
			}
		}
	}
	return errs
}

// collectImages removes images the agent built for projects that are gone, and older
// builds of a project whose every environment has its plan in this pass. An image
// still in use stays until a later pass.
func (r *Reconciler) collectImages(ctx context.Context, desired *agentv2.DesiredState, preps []*prepared) {
	images, err := r.rt.Images(ctx, r.opt.Instance)
	if err != nil || len(images) == 0 {
		return
	}
	projects := map[string]bool{}
	for _, p := range desired.Projects {
		projects[p.Name] = true
	}
	// complete: the project has environments and all of them have their plan.
	complete := map[string]bool{}
	for _, p := range preps {
		if _, seen := complete[p.env.Project]; !seen {
			complete[p.env.Project] = true
		}
	}
	keep := map[string]bool{}
	for _, p := range preps {
		if p.plan == nil {
			complete[p.env.Project] = false
			continue
		}
		for _, b := range p.plan.Builds {
			keep[b.Tag] = true
		}
	}
	for _, img := range images {
		project := img.Labels[runtime.LabelProject]
		used := false
		for _, t := range img.Tags {
			used = used || keep[t]
		}
		if used || (projects[project] && !complete[project]) {
			continue
		}
		if err := r.rt.RemoveImage(ctx, img.ID); err != nil {
			r.opt.Log.Debug("image stays", "image", img.Tags, "error", err)
		}
	}
}

// removeFiles removes the hook marks and logs of an environment and its clones of
// application repositories. A clone with work that exists nowhere else stays where it is.
func (r *Reconciler) removeFiles(ctx context.Context, project, envID string) error {
	if err := os.RemoveAll(filepath.Join(r.opt.EnvsDir, envID)); err != nil {
		return err
	}
	if project == "" {
		return nil
	}
	dir := filepath.Join(r.opt.SourcesDir, project, envID)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	r.mu.Lock()
	forced := r.forced[envID]
	r.mu.Unlock()
	kept := 0
	for _, e := range entries {
		src := filepath.Join(dir, e.Name())
		switch {
		case git.IsWorktree(src):
			// Commits on its branch stay with the branch; only what is nowhere else counts.
			if lost, err := git.Lost(ctx, src); !forced && (err != nil || lost != "") {
				r.opt.Log.Warn("kept a worktree with unsaved work", "path", src, "unsaved", lost, "error", err)
				kept++
				continue
			}
			if err := git.RemoveWorktree(ctx, src); err != nil {
				return err
			}
			continue
		case git.IsRepo(src):
			if unsaved, err := git.Unsaved(ctx, src); err != nil || unsaved != "" {
				r.opt.Log.Warn("kept a checkout with unsaved work", "path", src, "unsaved", unsaved, "error", err)
				kept++
				continue
			}
			if wts, err := git.LinkedWorktrees(ctx, src); err != nil || len(wts) > 0 {
				r.opt.Log.Warn("kept a clone that worktrees still use", "path", src, "worktrees", wts, "error", err)
				kept++
				continue
			}
		}
		if err := os.RemoveAll(src); err != nil {
			return err
		}
	}
	if kept == 0 {
		r.mu.Lock()
		delete(r.forced, envID)
		r.mu.Unlock()
	}
	if kept == 0 {
		_ = os.Remove(dir)
		_ = os.Remove(filepath.Dir(dir))
	}
	return nil
}

func describe(subnets []netip.Prefix) string {
	if len(subnets) == 0 {
		return ""
	}
	return subnets[0].String()
}

func kindLabel(k agentv2.EnvironmentKind) string {
	if k == agentv2.EnvironmentKind_ENVIRONMENT_KIND_FEATURE {
		return "feature"
	}
	return "base"
}

// FreeSubnet returns the first /24 of pool that overlaps none of used.
func FreeSubnet(pool netip.Prefix, used []netip.Prefix) (netip.Prefix, bool) {
	base := pool.Masked().Addr().As4()
	count := 1 << (24 - pool.Bits())
	for i := 0; i < count; i++ {
		a := base
		a[2] += byte(i)
		cand := netip.PrefixFrom(netip.AddrFrom4(a), 24)
		free := true
		for _, u := range used {
			if u.Overlaps(cand) {
				free = false
				break
			}
		}
		if free {
			return cand, true
		}
	}
	return netip.Prefix{}, false
}

// update changes one status and publishes it if anything but the timestamp changed.
func (r *Reconciler) update(envID string, fn func(*agentv2.EnvironmentStatus)) {
	r.mu.Lock()
	s, ok := r.status[envID]
	if !ok {
		s = &agentv2.EnvironmentStatus{Id: envID}
		r.status[envID] = s
	}
	before := proto.Clone(s).(*agentv2.EnvironmentStatus)
	fn(s)
	if proto.Equal(before, s) {
		r.mu.Unlock()
		return
	}
	s.UpdatedAt = timestamppb.New(r.now())
	out := proto.Clone(s).(*agentv2.EnvironmentStatus)
	r.mu.Unlock()
	r.publish(out)
}

// UpdateStatus changes an environment's status from outside a pass: the lifecycle
// manager reports sleep and wake with it.
func (r *Reconciler) UpdateStatus(envID string, fn func(*agentv2.EnvironmentStatus)) {
	r.mu.Lock()
	_, ok := r.status[envID]
	r.mu.Unlock()
	if ok {
		r.update(envID, fn)
	}
}

// forget drops a status and publishes its removal.
func (r *Reconciler) forget(envID string, rev uint64) {
	r.mu.Lock()
	s, ok := r.status[envID]
	if !ok {
		r.mu.Unlock()
		return
	}
	delete(r.status, envID)
	s.Removed, s.Phase, s.AppliedRevision, s.UpdatedAt = true, agentv2.Phase_PHASE_UNSPECIFIED, rev, timestamppb.New(r.now())
	r.mu.Unlock()
	r.publish(s)
}

// Fail marks every desired environment as failed for a reason outside any single
// environment, such as an unreachable runtime, so waiting clients learn about it.
func (r *Reconciler) Fail(desired *agentv2.DesiredState, err error) {
	for _, env := range desired.Environments {
		r.update(env.Id, func(s *agentv2.EnvironmentStatus) {
			s.Project, s.Kind, s.Slug = env.Project, env.Kind, env.Slug
			s.Phase = agentv2.Phase_PHASE_ERROR
			s.Error = err.Error()
			s.AppliedRevision = desired.Revision
		})
	}
}
