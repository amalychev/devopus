// Package reconcile brings the runtime to the desired state (ADR-0001,
// docs/architecture/agent.md): observe by labels, run idempotent steps per
// environment, collect garbage the desired state no longer has.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/docklet/docklet/gen/docklet/agent/v1"
	"github.com/docklet/docklet/internal/agent/runtime"
)

// Runtime is the part of the container runtime the reconciler uses.
type Runtime interface {
	Networks(ctx context.Context) ([]runtime.Network, error)
	CreateNetwork(ctx context.Context, name string, subnet netip.Prefix, labels map[string]string) (string, error)
	RemoveNetwork(ctx context.Context, id string) error
}

// Pool is the address pool of environment networks: 256 networks of /24 that do not
// collide with Docker's default 172.17–172.31 or typical corporate VPNs (ADR-0006).
var Pool = netip.MustParsePrefix("10.213.0.0/16")

type Reconciler struct {
	rt      Runtime
	pool    netip.Prefix
	publish func(*agentv1.EnvironmentStatus)
	now     func() time.Time

	mu     sync.Mutex
	status map[string]*agentv1.EnvironmentStatus
}

// New makes a reconciler; publish receives a copy of every status that changed.
func New(rt Runtime, publish func(*agentv1.EnvironmentStatus)) *Reconciler {
	if publish == nil {
		publish = func(*agentv1.EnvironmentStatus) {}
	}
	return &Reconciler{rt: rt, pool: Pool, publish: publish, now: time.Now, status: map[string]*agentv1.EnvironmentStatus{}}
}

// Statuses returns copies of the current statuses, ordered by project and ID.
func (r *Reconciler) Statuses() []*agentv1.EnvironmentStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*agentv1.EnvironmentStatus, 0, len(r.status))
	for _, s := range r.status {
		out = append(out, proto.Clone(s).(*agentv1.EnvironmentStatus))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		return out[i].Id < out[j].Id
	})
	return out
}

// NetworkName is the network of an environment; the same name is the compose project.
func NetworkName(project, envID string) string { return project + "-env-" + envID }

// Reconcile runs one pass. Errors of single environments end up in their status; the
// returned error means the pass could not observe the runtime or left work undone.
func (r *Reconciler) Reconcile(ctx context.Context, desired *agentv1.DesiredState) error {
	nets, err := r.rt.Networks(ctx)
	if err != nil {
		return fmt.Errorf("observe networks: %w", err)
	}
	managed := map[string]runtime.Network{}
	for _, n := range nets {
		if n.Labels[runtime.LabelManaged] == "true" && n.Labels[runtime.LabelEnv] != "" {
			managed[n.Labels[runtime.LabelEnv]] = n
		}
	}

	var errs []error
	wanted := map[string]bool{}
	envs := append([]*agentv1.EnvironmentSpec{}, desired.Environments...)
	sort.Slice(envs, func(i, j int) bool { return envs[i].Id < envs[j].Id })
	for _, env := range envs {
		wanted[env.Id] = true
		if err := r.reconcileEnv(ctx, desired.Revision, env, managed, nets); err != nil {
			errs = append(errs, fmt.Errorf("environment %s: %w", env.Id, err))
		}
	}

	// GC: only resources labelled as ours whose environment left the desired state.
	for id, n := range managed {
		if wanted[id] {
			continue
		}
		r.update(id, func(s *agentv1.EnvironmentStatus) {
			if s.Project == "" {
				s.Project = n.Labels[runtime.LabelProject]
			}
			s.Phase = agentv1.Phase_PHASE_DELETING
		})
		if err := r.rt.RemoveNetwork(ctx, n.ID); err != nil {
			r.update(id, func(s *agentv1.EnvironmentStatus) { s.Error = "remove network: " + err.Error() })
			errs = append(errs, fmt.Errorf("gc %s: %w", n.Name, err))
			continue
		}
		r.forget(id, desired.Revision)
	}
	// Environments that were wanted but never got resources.
	for _, s := range r.Statuses() {
		if _, has := managed[s.Id]; !wanted[s.Id] && !has {
			r.forget(s.Id, desired.Revision)
		}
	}
	return errors.Join(errs...)
}

// step is one idempotent part of bringing an environment up.
type step struct {
	name string
	run  func(ctx context.Context) (string, error)
}

func (r *Reconciler) reconcileEnv(ctx context.Context, rev uint64, env *agentv1.EnvironmentSpec, managed map[string]runtime.Network, all []runtime.Network) error {
	r.update(env.Id, func(s *agentv1.EnvironmentStatus) {
		s.Project, s.Kind, s.Slug = env.Project, env.Kind, env.Slug
		if s.Phase == agentv1.Phase_PHASE_UNSPECIFIED {
			s.Phase = agentv1.Phase_PHASE_CREATING
		}
	})
	steps := []step{
		{"network", func(ctx context.Context) (string, error) { return r.ensureNetwork(ctx, env, managed, all) }},
	}
	for _, st := range steps {
		if err := r.runStep(ctx, env.Id, st); err != nil {
			r.update(env.Id, func(s *agentv1.EnvironmentStatus) {
				s.Phase = agentv1.Phase_PHASE_ERROR
				s.Error = st.name + ": " + err.Error()
			})
			return err
		}
	}
	r.update(env.Id, func(s *agentv1.EnvironmentStatus) {
		s.Error = ""
		s.AppliedRevision = rev
		if env.Phase == agentv1.DesiredPhase_DESIRED_PHASE_STOPPED {
			s.Phase = agentv1.Phase_PHASE_STOPPED
		} else {
			s.Phase = agentv1.Phase_PHASE_RUNNING
		}
	})
	return nil
}

// runStep records progress only on transitions, so a converged environment does not
// produce events on every pass.
func (r *Reconciler) runStep(ctx context.Context, envID string, st step) error {
	r.update(envID, func(s *agentv1.EnvironmentStatus) {
		p := findStep(s, st.name)
		if p.State != agentv1.StepState_STEP_STATE_DONE {
			p.State = agentv1.StepState_STEP_STATE_RUNNING
			if p.StartedAt == nil {
				p.StartedAt = timestamppb.New(r.now())
			}
		}
	})
	msg, err := st.run(ctx)
	r.update(envID, func(s *agentv1.EnvironmentStatus) {
		p := findStep(s, st.name)
		switch {
		case err != nil:
			p.State, p.Message, p.FinishedAt = agentv1.StepState_STEP_STATE_FAILED, err.Error(), timestamppb.New(r.now())
		case p.State != agentv1.StepState_STEP_STATE_DONE || p.Message != msg:
			p.State, p.Message, p.FinishedAt = agentv1.StepState_STEP_STATE_DONE, msg, timestamppb.New(r.now())
		}
	})
	return err
}

func findStep(s *agentv1.EnvironmentStatus, name string) *agentv1.StepProgress {
	for _, p := range s.Steps {
		if p.Name == name {
			return p
		}
	}
	p := &agentv1.StepProgress{Name: name, State: agentv1.StepState_STEP_STATE_PENDING}
	s.Steps = append(s.Steps, p)
	return p
}

func (r *Reconciler) ensureNetwork(ctx context.Context, env *agentv1.EnvironmentSpec, managed map[string]runtime.Network, all []runtime.Network) (string, error) {
	name := NetworkName(env.Project, env.Id)
	if n, ok := managed[env.Id]; ok {
		r.setNetwork(env.Id, n.Name, n.Subnets)
		return describe(n.Subnets), nil
	}
	var used []netip.Prefix
	for _, n := range all {
		used = append(used, n.Subnets...)
	}
	labels := map[string]string{
		runtime.LabelManaged: "true",
		runtime.LabelProject: env.Project,
		runtime.LabelEnv:     env.Id,
		runtime.LabelKind:    kindLabel(env.Kind),
	}
	// Another runtime user may take a subnet between our observation and the create.
	for attempt := 0; attempt < 8; attempt++ {
		subnet, ok := FreeSubnet(r.pool, used)
		if !ok {
			return "", fmt.Errorf("address pool %s is exhausted (256 environments)", r.pool)
		}
		_, err := r.rt.CreateNetwork(ctx, name, subnet, labels)
		if errors.Is(err, runtime.ErrSubnetTaken) {
			used = append(used, subnet)
			continue
		}
		if err != nil {
			return "", err
		}
		r.setNetwork(env.Id, name, []netip.Prefix{subnet})
		return describe([]netip.Prefix{subnet}), nil
	}
	return "", errors.New("no free subnet after 8 attempts")
}

func (r *Reconciler) setNetwork(envID, name string, subnets []netip.Prefix) {
	r.update(envID, func(s *agentv1.EnvironmentStatus) {
		s.Network = &agentv1.NetworkStatus{Name: name, Subnet: describe(subnets)}
	})
}

func describe(subnets []netip.Prefix) string {
	if len(subnets) == 0 {
		return ""
	}
	return subnets[0].String()
}

func kindLabel(k agentv1.EnvironmentKind) string {
	if k == agentv1.EnvironmentKind_ENVIRONMENT_KIND_FEATURE {
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
func (r *Reconciler) update(envID string, fn func(*agentv1.EnvironmentStatus)) {
	r.mu.Lock()
	s, ok := r.status[envID]
	if !ok {
		s = &agentv1.EnvironmentStatus{Id: envID}
		r.status[envID] = s
	}
	before := proto.Clone(s).(*agentv1.EnvironmentStatus)
	fn(s)
	if proto.Equal(before, s) {
		r.mu.Unlock()
		return
	}
	s.UpdatedAt = timestamppb.New(r.now())
	out := proto.Clone(s).(*agentv1.EnvironmentStatus)
	r.mu.Unlock()
	r.publish(out)
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
	s.Removed, s.Phase, s.AppliedRevision, s.UpdatedAt = true, agentv1.Phase_PHASE_UNSPECIFIED, rev, timestamppb.New(r.now())
	r.mu.Unlock()
	r.publish(s)
}

// Fail marks every desired environment as failed for a reason outside any single
// environment, such as an unreachable runtime, so waiting clients learn about it.
func (r *Reconciler) Fail(desired *agentv1.DesiredState, err error) {
	for _, env := range desired.Environments {
		r.update(env.Id, func(s *agentv1.EnvironmentStatus) {
			s.Project, s.Kind, s.Slug = env.Project, env.Kind, env.Slug
			s.Phase = agentv1.Phase_PHASE_ERROR
			s.Error = err.Error()
			s.AppliedRevision = desired.Revision
		})
	}
}
