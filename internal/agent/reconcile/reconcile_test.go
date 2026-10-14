package reconcile

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"testing"

	agentv1 "github.com/docklet/docklet/gen/docklet/agent/v1"
	"github.com/docklet/docklet/internal/agent/runtime"
)

// fakeRuntime keeps networks in memory and refuses overlapping subnets like dockerd.
type fakeRuntime struct {
	mu      sync.Mutex
	nets    map[string]runtime.Network
	creates int
	// steal makes the next create fail as if another tool took the subnet first.
	steal bool
}

func newFake(foreign ...runtime.Network) *fakeRuntime {
	f := &fakeRuntime{nets: map[string]runtime.Network{}}
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
	delete(f.nets, id)
	return nil
}

func desired(rev uint64, envs ...*agentv1.EnvironmentSpec) *agentv1.DesiredState {
	return &agentv1.DesiredState{Revision: rev, Environments: envs}
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
	rt := newFake(foreign)
	rec := &recorder{}
	r := New(rt, rec.publish)

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
	if s := st[0].Steps[0]; s.Name != "network" || s.State != agentv1.StepState_STEP_STATE_DONE {
		t.Errorf("step = %v", s)
	}

	// A converged pass changes nothing and publishes nothing.
	n := rec.count()
	if err := r.Reconcile(ctx, desired(1, base("aaaaaa"))); err != nil {
		t.Fatal(err)
	}
	if rec.count() != n || rt.creates != 1 {
		t.Errorf("second pass: %d new events, %d creates", rec.count()-n, rt.creates)
	}

	// The agent restarts: a fresh reconciler adopts the network instead of creating one.
	r2 := New(rt, nil)
	if err := r2.Reconcile(ctx, desired(2, base("aaaaaa"))); err != nil {
		t.Fatal(err)
	}
	if rt.creates != 1 || r2.Statuses()[0].Network.Subnet != "10.213.2.0/24" {
		t.Errorf("restart: creates %d, status %v", rt.creates, r2.Statuses())
	}

	// The environment leaves the desired state: its network goes, the foreign one stays.
	if err := r2.Reconcile(ctx, desired(3)); err != nil {
		t.Fatal(err)
	}
	nets, _ := rt.Networks(ctx)
	if len(nets) != 1 || nets[0].ID != "corp" {
		t.Errorf("after gc: %v", nets)
	}
	if len(r2.Statuses()) != 0 {
		t.Errorf("statuses after gc: %v", r2.Statuses())
	}
}

func TestStoppedPhase(t *testing.T) {
	r := New(newFake(), nil)
	env := base("bbbbbb")
	env.Phase = agentv1.DesiredPhase_DESIRED_PHASE_STOPPED
	if err := r.Reconcile(context.Background(), desired(1, env)); err != nil {
		t.Fatal(err)
	}
	if p := r.Statuses()[0].Phase; p != agentv1.Phase_PHASE_STOPPED {
		t.Errorf("phase = %v", p)
	}
}

func TestSubnetRaceRetries(t *testing.T) {
	rt := newFake()
	rt.steal = true
	r := New(rt, nil)
	if err := r.Reconcile(context.Background(), desired(1, base("cccccc"))); err != nil {
		t.Fatal(err)
	}
	if s := r.Statuses()[0].Network.Subnet; s != "10.213.1.0/24" {
		t.Errorf("subnet = %s, want the next one after the stolen 10.213.0.0/24", s)
	}
}

func TestEnvironmentsGetDistinctSubnets(t *testing.T) {
	rt := newFake()
	r := New(rt, nil)
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
