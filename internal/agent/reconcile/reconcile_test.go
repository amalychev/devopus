package reconcile

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"

	agentv1 "github.com/docklet/docklet/gen/docklet/agent/v1"
	"github.com/docklet/docklet/internal/agent/pki"
	"github.com/docklet/docklet/internal/agent/proxy"
	"github.com/docklet/docklet/internal/agent/runtime"
)

// fakeRuntime keeps networks and containers in memory and refuses overlapping subnets
// like dockerd.
type fakeRuntime struct {
	mu       sync.Mutex
	nets     map[string]runtime.Network
	ctrs     map[string]*runtime.Container
	specs    map[string]runtime.ContainerSpec
	creates  int // networks
	created  int // containers
	removed  int // containers
	connects int
	seq      int
	// steal makes the next network create fail as if another tool took the subnet first.
	steal bool
	// startErr fails the next start of a container with this role.
	startErr map[string]error
}

func newFake(foreign ...runtime.Network) *fakeRuntime {
	f := &fakeRuntime{nets: map[string]runtime.Network{}, ctrs: map[string]*runtime.Container{}, specs: map[string]runtime.ContainerSpec{}, startErr: map[string]error{}}
	for _, n := range foreign {
		f.nets[n.ID] = n
	}
	return f
}

func (f *fakeRuntime) Networks(context.Context) ([]runtime.Network, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []runtime.Network
	for _, n := range f.nets {
		out = append(out, n)
	}
	return out, nil
}

func (f *fakeRuntime) CreateNetwork(_ context.Context, name string, subnet netip.Prefix, labels map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.steal {
		f.steal = false
		return "", runtime.ErrSubnetTaken
	}
	for _, n := range f.nets {
		if n.Name == name {
			return "", fmt.Errorf("network %s exists", name)
		}
		for _, s := range n.Subnets {
			if s.Overlaps(subnet) {
				return "", runtime.ErrSubnetTaken
			}
		}
	}
	f.creates++
	id := fmt.Sprintf("net%d", f.creates)
	f.nets[id] = runtime.Network{ID: id, Name: name, Subnets: []netip.Prefix{subnet}, Labels: labels}
	return id, nil
}

func (f *fakeRuntime) RemoveNetwork(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.ctrs {
		if _, ok := c.Networks[id]; ok {
			return fmt.Errorf("network %s has active endpoints (%s)", id, c.Name)
		}
	}
	delete(f.nets, id)
	return nil
}

func (f *fakeRuntime) Containers(_ context.Context, instance string) ([]runtime.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []runtime.Container
	for _, c := range f.ctrs {
		if c.Labels[runtime.LabelInstance] != instance {
			continue
		}
		cp := *c
		cp.Networks = map[string]netip.Addr{}
		for k, v := range c.Networks {
			cp.Networks[k] = v
		}
		out = append(out, cp)
	}
	return out, nil
}

func (f *fakeRuntime) EnsureImage(context.Context, string) error { return nil }

func (f *fakeRuntime) CreateContainer(_ context.Context, spec runtime.ContainerSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.ctrs {
		if c.Name == spec.Name {
			return "", fmt.Errorf("container name %s is in use", spec.Name)
		}
	}
	f.seq++
	f.created++
	id := fmt.Sprintf("ctr%d", f.seq)
	c := &runtime.Container{ID: id, Name: spec.Name, Image: spec.Image, Labels: spec.Labels, Networks: map[string]netip.Addr{}}
	if spec.Network != "" {
		c.Networks[spec.Network] = spec.IP
	}
	f.ctrs[id] = c
	f.specs[id] = spec
	return id, nil
}

func (f *fakeRuntime) StartContainer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.ctrs[id]
	if !ok {
		return errors.New("no such container")
	}
	if err := f.startErr[c.Labels[runtime.LabelRole]]; err != nil {
		delete(f.startErr, c.Labels[runtime.LabelRole])
		return err
	}
	c.Running = true
	return nil
}

func (f *fakeRuntime) RemoveContainer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.ctrs[id]; ok {
		f.removed++
	}
	delete(f.ctrs, id)
	return nil
}

func (f *fakeRuntime) ConnectNetwork(_ context.Context, netID, ctrID string, ip netip.Addr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connects++
	f.ctrs[ctrID].Networks[netID] = ip
	return nil
}

func (f *fakeRuntime) DisconnectNetwork(_ context.Context, netID, ctrID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.ctrs[ctrID]; ok {
		delete(c.Networks, netID)
	}
	return nil
}

func (f *fakeRuntime) ContainerLogs(context.Context, string, int) string { return "" }

func (f *fakeRuntime) byRole(role string) []*runtime.Container {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*runtime.Container
	for _, c := range f.ctrs {
		if c.Labels[runtime.LabelRole] == role {
			out = append(out, c)
		}
	}
	return out
}

// fakeXDS records the configurations it is given.
type fakeXDS struct {
	mu   sync.Mutex
	cfgs []proxy.Config
}

func (x *fakeXDS) Set(_ context.Context, cfg proxy.Config) (string, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	res, v, err := cfg.Resources()
	if err != nil {
		return "", err
	}
	snap, err := cache.NewSnapshot(v, res)
	if err != nil {
		return "", err
	}
	if err := snap.Consistent(); err != nil {
		return "", err
	}
	x.cfgs = append(x.cfgs, cfg)
	return v, nil
}

func (x *fakeXDS) State() proxy.XDSState { return proxy.XDSState{Connected: true} }
func (x *fakeXDS) Port() int             { return 47000 }

func (x *fakeXDS) last() proxy.Config {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.cfgs[len(x.cfgs)-1]
}

type harness struct {
	rt      *fakeRuntime
	xds     *fakeXDS
	opt     Options
	chooses int
	ports   proxy.Ports
	probe   error
}

func newHarness(t *testing.T, rt *fakeRuntime) *harness {
	t.Helper()
	ca, err := pki.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctl, err := pki.LoadOrCreateControl(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{rt: rt, xds: &fakeXDS{}, ports: proxy.DefaultPorts}
	h.opt = Options{
		Instance:  "test",
		ProxyName: "docklet-proxy-test",
		CA:        ca,
		Leaves:    pki.Leaves{Dir: t.TempDir()},
		Control:   ctl,
		XDS:       h.xds,
		ChoosePorts: func(context.Context) (proxy.Ports, []proxy.Conflict, error) {
			h.chooses++
			if h.ports == proxy.FallbackPorts {
				return h.ports, []proxy.Conflict{{Addr: netip.MustParseAddrPort("127.0.0.1:80"), Owner: "nginx", PID: 569, Kind: "nginx"}}, nil
			}
			return h.ports, nil, nil
		},
		Probe: func(context.Context, string, proxy.Ports) error { return h.probe },
	}
	return h
}

func (h *harness) new(publish func(*agentv1.EnvironmentStatus)) *Reconciler {
	return New(h.rt, h.opt, publish)
}

const shopYAML = "version: 1\nproject: shop\ndomain: shop.localhost\nservices:\n  mail:\n    image: axllent/mailpit:v1.20\n    port: 1025\n    ui_port: 8025\n    expose: mail\nmodules:\n  web:\n    repo: git@example.com:web.git\n    image: nginx:1.27\n    routes: [/]\n"

func desired(rev uint64, envs ...*agentv1.EnvironmentSpec) *agentv1.DesiredState {
	return &agentv1.DesiredState{Revision: rev, Environments: envs, Projects: []*agentv1.ProjectSpec{{Name: "shop", Root: "/src/shop", Config: []byte(shopYAML)}}}
}

func base(id string) *agentv1.EnvironmentSpec {
	return &agentv1.EnvironmentSpec{Id: id, Project: "shop", Kind: agentv1.EnvironmentKind_ENVIRONMENT_KIND_BASE, Phase: agentv1.DesiredPhase_DESIRED_PHASE_RUNNING}
}

type recorder struct {
	mu     sync.Mutex
	events []*agentv1.EnvironmentStatus
}

func (r *recorder) publish(s *agentv1.EnvironmentStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, s)
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func TestCreateConvergeAndGC(t *testing.T) {
	ctx := context.Background()
	foreign := runtime.Network{ID: "corp", Name: "vpn-like", Subnets: []netip.Prefix{netip.MustParsePrefix("10.213.0.0/23")}}
	// A network of another agent instance (a test agent next to the real one) is never touched.
	other := runtime.Network{ID: "other", Name: "shop-env-zzzzzz", Subnets: []netip.Prefix{netip.MustParsePrefix("10.213.9.0/24")},
		Labels: map[string]string{runtime.LabelManaged: "true", runtime.LabelInstance: "real", runtime.LabelEnv: "zzzzzz"}}
	h := newHarness(t, newFake(foreign, other))
	rec := &recorder{}
	r := h.new(rec.publish)

	if err := r.Reconcile(ctx, desired(1, base("aaaaaa"))); err != nil {
		t.Fatal(err)
	}
	st := r.Statuses()
	if len(st) != 1 || st[0].Phase != agentv1.Phase_PHASE_RUNNING || st[0].AppliedRevision != 1 {
		t.Fatalf("status = %v", st)
	}
	if st[0].Network.Name != "shop-env-aaaaaa" || st[0].Network.Subnet != "10.213.2.0/24" {
		t.Errorf("network = %v (must skip the foreign 10.213.0.0/23)", st[0].Network)
	}
	var steps []string
	for _, s := range st[0].Steps {
		if s.State != agentv1.StepState_STEP_STATE_DONE {
			t.Errorf("step %s: %v %s", s.Name, s.State, s.Message)
		}
		steps = append(steps, s.Name)
	}
	if got := strings.Join(steps, ","); got != "network,certificate,proxy,dns,routes" {
		t.Errorf("steps = %s", got)
	}
	if st[0].Url != "https://shop.localhost" {
		t.Errorf("url = %s", st[0].Url)
	}

	// One proxy, attached to the network at .254; one DNS server at .253.
	proxies, dns := h.rt.byRole("proxy"), h.rt.byRole("dns")
	if len(proxies) != 1 || len(dns) != 1 {
		t.Fatalf("proxies %d, dns %d", len(proxies), len(dns))
	}
	netID := findNet(t, h.rt, "shop-env-aaaaaa")
	if ip := proxies[0].Networks[netID]; ip.String() != "10.213.2.254" {
		t.Errorf("proxy at %v", ip)
	}
	if ip := dns[0].Networks[netID]; ip.String() != "10.213.2.253" {
		t.Errorf("dns at %v", ip)
	}
	if !strings.Contains(string(h.rt.specs[dns[0].ID].Files[0].Data), "IN A 10.213.2.254") {
		t.Error("Corefile does not point *.localhost at the proxy")
	}
	cfg := h.xds.last()
	if len(cfg.Sites) != 1 || strings.Join(cfg.Sites[0].Hosts, ",") != "shop.localhost,mail.shop.localhost" {
		t.Errorf("sites = %+v", cfg.Sites)
	}
	if routes := r.Routes(); len(routes) != 2 || routes[0].Host != "mail.shop.localhost" {
		t.Errorf("routes = %v", routes)
	}
	if p := r.Proxy(); p.State != "running" || p.Mode != "default" || p.HttpsPort != 443 {
		t.Errorf("proxy = %v", p)
	}

	// A converged pass changes nothing and publishes nothing.
	n := rec.count()
	if err := r.Reconcile(ctx, desired(1, base("aaaaaa"))); err != nil {
		t.Fatal(err)
	}
	if rec.count() != n || h.rt.creates != 1 || h.rt.created != 2 || h.rt.connects != 1 || h.chooses != 1 {
		t.Errorf("second pass: %d new events, %d network creates, %d container creates, %d connects, %d port probes",
			rec.count()-n, h.rt.creates, h.rt.created, h.rt.connects, h.chooses)
	}

	// The agent restarts: a fresh reconciler adopts everything, the certificate included.
	certBefore := string(h.xds.last().Sites[0].CertPEM)
	r2 := h.new(nil)
	if err := r2.Reconcile(ctx, desired(2, base("aaaaaa"))); err != nil {
		t.Fatal(err)
	}
	if h.rt.creates != 1 || h.rt.created != 2 || h.rt.connects != 1 || h.chooses != 1 || r2.Statuses()[0].Network.Subnet != "10.213.2.0/24" {
		t.Errorf("restart: %d network creates, %d container creates, %d connects, %d probes", h.rt.creates, h.rt.created, h.rt.connects, h.chooses)
	}
	if string(h.xds.last().Sites[0].CertPEM) != certBefore {
		t.Error("restart issued a new certificate")
	}

	// The environment leaves the desired state: its containers, network and certificate
	// go, then the proxy since nothing is left; foreign networks stay.
	if err := r2.Reconcile(ctx, desired(3)); err != nil {
		t.Fatal(err)
	}
	nets, _ := h.rt.Networks(ctx)
	if len(nets) != 2 {
		t.Errorf("after gc: %v", nets)
	}
	if len(h.rt.ctrs) != 0 {
		t.Errorf("containers after gc: %d", len(h.rt.ctrs))
	}
	if len(r2.Statuses()) != 0 || r2.Proxy().State != "absent" || len(h.opt.Leaves.Names()) != 0 {
		t.Errorf("after gc: statuses %v, proxy %v, leaves %v", r2.Statuses(), r2.Proxy(), h.opt.Leaves.Names())
	}
}

func TestStoppedPhase(t *testing.T) {
	h := newHarness(t, newFake())
	r := h.new(nil)
	if err := r.Reconcile(context.Background(), desired(1, base("bbbbbb"))); err != nil {
		t.Fatal(err)
	}
	env := base("bbbbbb")
	env.Phase = agentv1.DesiredPhase_DESIRED_PHASE_STOPPED
	if err := r.Reconcile(context.Background(), desired(2, env)); err != nil {
		t.Fatal(err)
	}
	if p := r.Statuses()[0].Phase; p != agentv1.Phase_PHASE_STOPPED {
		t.Errorf("phase = %v", p)
	}
	if len(h.rt.byRole("dns")) != 0 || len(h.rt.byRole("proxy")) != 1 {
		t.Error("a stopped environment keeps the proxy and drops its DNS server")
	}
	if page := h.xds.last().Sites[0].Page; page.Status != 503 {
		t.Errorf("stopped page: %+v", page)
	}
}

func TestFallbackPortsStick(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newFake())
	h.ports = proxy.FallbackPorts
	r := h.new(nil)
	if err := r.Reconcile(ctx, desired(1, base("dddddd"))); err != nil {
		t.Fatal(err)
	}
	p := r.Proxy()
	if p.Mode != "fallback" || p.HttpsPort != 18443 || len(p.Conflicts) != 1 || p.Conflicts[0].Owner != "nginx" {
		t.Fatalf("proxy = %v", p)
	}
	if u := r.Statuses()[0].Url; u != "https://shop.localhost:18443" {
		t.Errorf("url = %s", u)
	}
	if h.xds.last().HTTPSPort != 18443 {
		t.Error("HTTP must redirect to the fallback HTTPS port")
	}

	// 80/443 free up: the ports stay until the proxy is restarted, so URLs do not move.
	h.ports = proxy.DefaultPorts
	if err := r.Reconcile(ctx, desired(2, base("dddddd"))); err != nil {
		t.Fatal(err)
	}
	if h.chooses != 1 || r.Proxy().HttpsPort != 18443 {
		t.Errorf("ports moved without a restart: %d probes, %v", h.chooses, r.Proxy())
	}

	// docklet proxy restart: a new generation recreates the proxy, which probes again
	// and is attached to the environment network again.
	d := desired(3, base("dddddd"))
	d.Device = &agentv1.DeviceSettings{ProxyGeneration: 1}
	if err := r.Reconcile(ctx, d); err != nil {
		t.Fatal(err)
	}
	if h.chooses != 2 || r.Proxy().HttpsPort != 443 || r.Statuses()[0].Url != "https://shop.localhost" {
		t.Errorf("after restart: %d probes, %v", h.chooses, r.Proxy())
	}
	px := h.rt.byRole("proxy")
	if len(px) != 1 || len(px[0].Networks) != 1 {
		t.Errorf("new proxy not attached: %+v", px)
	}
}

func TestProxyPortsTakenWhileDown(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newFake())
	r := h.new(nil)
	if err := r.Reconcile(ctx, desired(1, base("eeeeee"))); err != nil {
		t.Fatal(err)
	}
	// Docker restarted, the proxy did not come back, and nginx took 80 meanwhile.
	for _, c := range h.rt.byRole("proxy") {
		c.Running = false
	}
	h.rt.startErr["proxy"] = errors.New("Bind for 127.0.0.1:80 failed: port is already allocated")
	h.ports = proxy.FallbackPorts
	if err := r.Reconcile(ctx, desired(2, base("eeeeee"))); err != nil {
		t.Fatal(err)
	}
	if p := r.Proxy(); p.State != "running" || p.Mode != "fallback" {
		t.Errorf("proxy = %v", p)
	}
}

func TestProbeFailureIsReported(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newFake())
	h.probe = errors.New("connection refused")
	r := h.new(nil)
	if err := r.Reconcile(ctx, desired(1, base("ffffff"))); err == nil {
		t.Fatal("a site the proxy does not serve must fail the pass")
	}
	s := r.Statuses()[0]
	if s.Phase != agentv1.Phase_PHASE_ERROR || !strings.HasPrefix(s.Error, "routes: connection refused") || s.AppliedRevision != 1 {
		t.Fatalf("status = %v", s)
	}
	h.probe = nil
	if err := r.Reconcile(ctx, desired(1, base("ffffff"))); err != nil {
		t.Fatal(err)
	}
	if s := r.Statuses()[0]; s.Phase != agentv1.Phase_PHASE_RUNNING || s.Error != "" {
		t.Fatalf("status = %v", s)
	}
}

func TestFailedStepRecovers(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newFake())
	h.rt.startErr["dns"] = errors.New("boom")
	r := h.new(nil)
	if err := r.Reconcile(ctx, desired(1, base("gggggg"))); err == nil {
		t.Fatal("expected the dns step to fail")
	}
	if s := r.Statuses()[0]; s.Phase != agentv1.Phase_PHASE_ERROR || !strings.HasPrefix(s.Error, "dns: boom") || s.AppliedRevision != 1 {
		t.Fatalf("status = %v", s)
	}
	// The site is still served: the certificate was ready.
	if len(h.xds.last().Sites) != 1 {
		t.Error("site dropped because of a later step")
	}
	if err := r.Reconcile(ctx, desired(1, base("gggggg"))); err != nil {
		t.Fatal(err)
	}
	if s := r.Statuses()[0]; s.Phase != agentv1.Phase_PHASE_RUNNING {
		t.Fatalf("status = %v", s)
	}
}

func TestSubnetRaceRetries(t *testing.T) {
	rt := newFake()
	rt.steal = true
	r := newHarness(t, rt).new(nil)
	if err := r.Reconcile(context.Background(), desired(1, base("cccccc"))); err != nil {
		t.Fatal(err)
	}
	if s := r.Statuses()[0].Network.Subnet; s != "10.213.1.0/24" {
		t.Errorf("subnet = %s, want the next one after the stolen 10.213.0.0/24", s)
	}
}

func TestEnvironmentsGetDistinctSubnets(t *testing.T) {
	h := newHarness(t, newFake())
	r := h.new(nil)
	if err := r.Reconcile(context.Background(), desired(1, base("a11111"), base("b22222"), base("c33333"))); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, s := range r.Statuses() {
		if seen[s.Network.Subnet] {
			t.Fatalf("subnet %s used twice", s.Network.Subnet)
		}
		seen[s.Network.Subnet] = true
	}
	if len(h.rt.byRole("proxy")) != 1 || len(h.rt.byRole("proxy")[0].Networks) != 3 {
		t.Error("one proxy must join every environment network")
	}
}

func TestFreeSubnetExhausted(t *testing.T) {
	if _, ok := FreeSubnet(Pool, []netip.Prefix{Pool}); ok {
		t.Error("expected exhaustion")
	}
	got, ok := FreeSubnet(Pool, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})
	if ok {
		t.Errorf("got %s inside a covering 10.0.0.0/8", got)
	}
	got, _ = FreeSubnet(Pool, []netip.Prefix{netip.MustParsePrefix("10.213.0.0/24"), netip.MustParsePrefix("10.213.1.0/24")})
	if got.String() != "10.213.2.0/24" {
		t.Errorf("got %s", got)
	}
}

func findNet(t *testing.T, rt *fakeRuntime, name string) string {
	t.Helper()
	for id, n := range rt.nets {
		if n.Name == name {
			return id
		}
	}
	t.Fatalf("no network %s", name)
	return ""
}
