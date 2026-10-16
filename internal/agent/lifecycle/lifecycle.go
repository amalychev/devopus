// Package lifecycle puts idle features to sleep and wakes them on their first request
// (docs/architecture/agent.md, P-7). The route of a feature never changes: the proxy
// sends its traffic to its containers while the agent declares them healthy, and to the
// agent's waker while not. Sleeping is the agent's own business, not desired state: a
// feature people want running may be asleep.
package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv2 "github.com/amalychev/devopus/gen/devopus/agent/v2"
	"github.com/amalychev/devopus/internal/agent/runtime"
)

// Env is what the manager knows about a feature, from the reconciler's last pass.
type Env struct {
	ID, Project, Name string
	// Host is the feature's host; Units in start order.
	Host  string
	Units []Unit
	// Dirs are the folders of its code: a change there is activity.
	Dirs       []string
	SleepAfter time.Duration
	// MaxRunning features of the project may be awake; 0: no limit.
	MaxRunning int
	// Ready: the feature came up; one that is still being created does not sleep.
	Ready bool
}

// Unit is a container of a feature.
type Unit struct {
	Name, Container, Role string
	Healthcheck           bool
	// Path is a route of an application: the waker waits until it answers.
	Path string
}

// Runtime is what the manager needs from the container runtime.
type Runtime interface {
	Containers(ctx context.Context, instance string) ([]runtime.Container, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string) error
}

// Options connect the manager to the rest of the agent.
type Options struct {
	Instance string
	// File keeps which features sleep across agent restarts.
	File string
	// SetAwake tells the proxy where the feature's traffic goes and waits until it does.
	SetAwake func(ctx context.Context, envID string, awake bool) error
	// Answers checks once, past the waker, that a unit answers on path.
	Answers func(ctx context.Context, host, path string) error
	// Publish changes an environment's status.
	Publish func(envID string, fn func(*agentv2.EnvironmentStatus))
	// Kick asks the reconciler for a pass.
	Kick func()
	Log  *slog.Logger
	// WakeTimeout bounds a wake: starting containers and waiting for the applications.
	WakeTimeout time.Duration
}

// sleepGrace is how long the units keep running after their traffic went to the waker.
const sleepGrace = time.Second

// Reasons a feature sleeps.
const (
	ReasonIdle   = "idle"
	ReasonBudget = "budget"
	ReasonManual = "manual"
)

type state struct {
	Asleep     bool      `json:"asleep"`
	Reason     string    `json:"reason,omitempty"`
	Since      time.Time `json:"since"`
	LastActive time.Time `json:"last_active"`
	// in memory
	wake *wake
	// sleeping is closed when a sleep in progress ends.
	sleeping chan struct{}
	scanned  time.Time
}

// wake is one wake in progress: every request that arrives meanwhile waits for it.
type wake struct {
	done chan struct{}
	err  error
}

type Manager struct {
	rt  Runtime
	opt Options
	now func() time.Time

	mu    sync.Mutex
	envs  map[string]Env
	state map[string]*state
}

func New(rt Runtime, opt Options) *Manager {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.WakeTimeout == 0 {
		opt.WakeTimeout = 3 * time.Minute
	}
	m := &Manager{rt: rt, opt: opt, now: time.Now, envs: map[string]Env{}, state: map[string]*state{}}
	if b, err := os.ReadFile(opt.File); err == nil {
		var saved map[string]*state
		if json.Unmarshal(b, &saved) == nil {
			for id, s := range saved {
				// Activity before a restart is unknown: count from now.
				s.LastActive = m.now()
				m.state[id] = s
			}
		}
	}
	return m
}

// Asleep reports whether the feature sleeps or is waking up: its traffic goes to the waker.
func (m *Manager) Asleep(envID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.state[envID]
	return s != nil && s.Asleep
}

// Settled reports whether the feature sleeps or is awake, with no switch in progress.
func (m *Manager) Settled(envID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.state[envID]
	return s == nil || (s.sleeping == nil && s.wake == nil)
}

// Update gives the manager the running features after a reconciler pass; the others
// are forgotten (removed, or stopped by 'devopus down').
func (m *Manager) Update(envs []Env) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.envs = map[string]Env{}
	for _, e := range envs {
		m.envs[e.ID] = e
		if m.state[e.ID] == nil {
			m.state[e.ID] = &state{LastActive: m.now()}
		}
	}
	for id, s := range m.state {
		if _, ok := m.envs[id]; !ok && s.wake == nil && s.sleeping == nil {
			delete(m.state, id)
		}
	}
	m.saveLocked()
}

// Forget drops what the manager knows about an environment: 'devopus down' stops it,
// 'devopus up' starts it awake.
func (m *Manager) Forget(envID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.state[envID]; s != nil && s.wake == nil && s.sleeping == nil {
		delete(m.state, envID)
		m.saveLocked()
	}
}

// Active records activity: requests through the proxy, a changed file, a wake.
func (m *Manager) Active(envID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.state[envID]; s != nil {
		s.LastActive = m.now()
	}
}

// OnRequests takes the proxy's request counters by cluster. Clusters of a site are named
// after its environment; requests a feature makes to the base (shared) are not activity.
func (m *Manager) OnRequests(cluster string, _ uint64) {
	env, rest, ok := strings.Cut(cluster, "-")
	if !ok || strings.HasPrefix(rest, "shared-") {
		return
	}
	m.Active(env)
}

// LastActive is when the feature was last in use.
func (m *Manager) LastActive(envID string) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.state[envID]; s != nil {
		return s.LastActive
	}
	return time.Time{}
}

func (m *Manager) saveLocked() {
	if m.opt.File == "" {
		return
	}
	b, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return
	}
	tmp := m.opt.File + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, m.opt.File)
	}
}

// Run checks the features until ctx ends: idle ones and those over the budget sleep.
func (m *Manager) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Check(ctx)
		}
	}
}

// Check runs one round: scans files of awake features and puts idle ones to sleep.
func (m *Manager) Check(ctx context.Context) {
	m.mu.Lock()
	var scan []Env
	for id, e := range m.envs {
		s := m.state[id]
		if s != nil && !s.Asleep && e.Ready && m.now().Sub(s.scanned) >= scanEvery(e.SleepAfter) {
			s.scanned = m.now()
			scan = append(scan, e)
		}
	}
	m.mu.Unlock()
	for _, e := range scan {
		if changed := latestChange(e.Dirs); !changed.IsZero() && m.now().Sub(changed) < e.SleepAfter {
			m.mu.Lock()
			if s := m.state[e.ID]; s != nil && changed.After(s.LastActive) {
				s.LastActive = changed
			}
			m.mu.Unlock()
		}
	}

	m.mu.Lock()
	var idle []string
	for id, e := range m.envs {
		s := m.state[id]
		if s != nil && !s.Asleep && s.sleeping == nil && s.wake == nil && e.Ready && e.SleepAfter > 0 && m.now().Sub(s.LastActive) >= e.SleepAfter {
			idle = append(idle, id)
		}
	}
	m.mu.Unlock()
	sort.Strings(idle)
	for _, id := range idle {
		if err := m.Sleep(ctx, id, ReasonIdle); err != nil {
			m.opt.Log.Warn("sleep", "env", id, "error", err)
		}
	}
	m.enforceBudget(ctx, "")
}

// scanEvery is how often the code of a feature is checked for changes.
func scanEvery(sleepAfter time.Duration) time.Duration {
	return min(max(sleepAfter/4, time.Second), 30*time.Second)
}

// enforceBudget puts the least recently used awake features of each project to sleep
// while more than max_running are awake. keep is the feature just woken.
func (m *Manager) enforceBudget(ctx context.Context, keep string) {
	m.mu.Lock()
	byProject := map[string][]string{}
	limits := map[string]int{}
	for id, e := range m.envs {
		s := m.state[id]
		if s == nil || s.Asleep || s.sleeping != nil || e.MaxRunning <= 0 {
			continue
		}
		byProject[e.Project] = append(byProject[e.Project], id)
		limits[e.Project] = e.MaxRunning
	}
	var over []string
	for p, ids := range byProject {
		sort.Slice(ids, func(i, j int) bool {
			if ids[i] == keep || ids[j] == keep {
				return ids[j] == keep
			}
			return m.state[ids[i]].LastActive.Before(m.state[ids[j]].LastActive)
		})
		if n := len(ids) - limits[p]; n > 0 {
			over = append(over, ids[:n]...)
		}
	}
	m.mu.Unlock()
	for _, id := range over {
		if err := m.Sleep(ctx, id, ReasonBudget); err != nil {
			m.opt.Log.Warn("sleep over the budget", "env", id, "error", err)
		}
	}
}

// Known reports whether the manager can switch the feature: it runs and came up.
func (m *Manager) Known(envID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.envs[envID]
	return ok && e.Ready
}

// Sleep puts a feature to sleep: its traffic goes to the waker first, then its
// containers stop (P-7: no request is lost in between).
func (m *Manager) Sleep(ctx context.Context, envID, reason string) error {
	m.mu.Lock()
	e, ok := m.envs[envID]
	s := m.state[envID]
	if !ok || s == nil {
		m.mu.Unlock()
		return fmt.Errorf("environment %s is not a running feature", envID)
	}
	if s.Asleep || s.sleeping != nil || s.wake != nil {
		m.mu.Unlock()
		return nil
	}
	done := make(chan struct{})
	s.sleeping = done
	s.Asleep, s.Reason, s.Since = true, reason, m.now()
	m.saveLocked()
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		s.sleeping = nil
		m.mu.Unlock()
		close(done)
	}()

	m.opt.Log.Info("feature goes to sleep", "env", envID, "name", e.Name, "reason", reason)
	if err := m.opt.SetAwake(ctx, envID, false); err != nil {
		return err
	}
	// Envoy acknowledges the switch before every worker follows it: a request may still
	// reach the unit for a moment, and the unit must still be there to answer it.
	select {
	case <-time.After(sleepGrace):
	case <-ctx.Done():
		return ctx.Err()
	}
	ctrs, err := m.containers(ctx, e)
	if err != nil {
		return err
	}
	// Applications and workers first, then what they use.
	var errs []error
	for i := len(e.Units) - 1; i >= 0; i-- {
		if c := ctrs[e.Units[i].Container]; c != nil && c.Running {
			errs = append(errs, m.rt.StopContainer(ctx, c.ID))
		}
	}
	since := m.now()
	m.opt.Publish(envID, func(st *agentv2.EnvironmentStatus) {
		st.Phase, st.SleepReason, st.SleepingSince = agentv2.Phase_PHASE_SLEEPING, reason, timestamppb.New(since)
	})
	m.opt.Kick()
	return errors.Join(errs...)
}

// Wake starts a sleeping feature, once for every caller that arrives meanwhile, and
// returns a channel closed when it serves again, with the error of the wake.
func (m *Manager) Wake(envID string) (<-chan struct{}, func() error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.state[envID]
	e, ok := m.envs[envID]
	done := make(chan struct{})
	if s == nil || !ok {
		close(done)
		return done, func() error { return fmt.Errorf("environment %s is not a running feature", envID) }
	}
	s.LastActive = m.now()
	if !s.Asleep {
		close(done)
		return done, func() error { return nil }
	}
	if s.wake == nil {
		w := &wake{done: make(chan struct{})}
		s.wake = w
		go m.wake(e, s, w, s.sleeping)
	}
	w := s.wake
	return w.done, func() error { return w.err }
}

func (m *Manager) wake(e Env, s *state, w *wake, sleeping chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), m.opt.WakeTimeout)
	defer cancel()
	// A request that came while the feature was going to sleep: let it finish first.
	if sleeping != nil {
		select {
		case <-sleeping:
		case <-ctx.Done():
		}
	}
	start := m.now()
	m.opt.Publish(e.ID, func(st *agentv2.EnvironmentStatus) { st.Phase = agentv2.Phase_PHASE_CREATING })
	err := m.start(ctx, e)
	if err == nil {
		// Awake before the switch: a pass that builds the proxy's configuration meanwhile
		// must not send the traffic back to the waker.
		m.mu.Lock()
		s.Asleep, s.Reason, s.LastActive = false, "", m.now()
		m.saveLocked()
		m.mu.Unlock()
		if err = m.opt.SetAwake(ctx, e.ID, true); err != nil {
			m.mu.Lock()
			s.Asleep = true
			m.saveLocked()
			m.mu.Unlock()
		}
	}
	m.mu.Lock()
	s.wake = nil
	w.err = err
	m.mu.Unlock()
	close(w.done)
	if err != nil {
		m.opt.Log.Warn("wake", "env", e.ID, "error", err)
		m.opt.Publish(e.ID, func(st *agentv2.EnvironmentStatus) {
			st.Phase, st.Error = agentv2.Phase_PHASE_SLEEPING, "wake: "+err.Error()
		})
	} else {
		m.opt.Log.Info("feature woke up", "env", e.ID, "name", e.Name, "took", m.now().Sub(start).Round(time.Millisecond))
		m.opt.Publish(e.ID, func(st *agentv2.EnvironmentStatus) {
			st.Phase, st.Error, st.SleepReason, st.SleepingSince = agentv2.Phase_PHASE_RUNNING, "", "", nil
		})
		m.enforceBudget(context.Background(), e.ID)
	}
	m.opt.Kick()
}

// start starts the feature's containers in order, services first, and waits until its
// routed applications answer past the waker.
func (m *Manager) start(ctx context.Context, e Env) error {
	ctrs, err := m.containers(ctx, e)
	if err != nil {
		return err
	}
	for _, u := range e.Units {
		c := ctrs[u.Container]
		if c == nil {
			return fmt.Errorf("the container of %s is gone; the agent recreates it", u.Name)
		}
		if !c.Running {
			if err := m.rt.StartContainer(ctx, c.ID); err != nil {
				return fmt.Errorf("%s: %w", u.Name, err)
			}
		}
		if u.Healthcheck && u.Role == "service" {
			if err := m.waitHealthy(ctx, u); err != nil {
				return err
			}
		}
	}
	var wg sync.WaitGroup
	errs := make([]error, len(e.Units))
	for i, u := range e.Units {
		if u.Path == "" || m.opt.Answers == nil {
			continue
		}
		wg.Add(1)
		go func(i int, u Unit) {
			defer wg.Done()
			for {
				if err := m.opt.Answers(ctx, e.Host, u.Path); err == nil {
					return
				}
				select {
				case <-ctx.Done():
					errs[i] = fmt.Errorf("%s did not answer on %s: %w", u.Name, u.Path, ctx.Err())
					return
				case <-time.After(50 * time.Millisecond):
				}
			}
		}(i, u)
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (m *Manager) waitHealthy(ctx context.Context, u Unit) error {
	for {
		all, err := m.rt.Containers(ctx, m.opt.Instance)
		if err != nil {
			return err
		}
		for _, c := range all {
			if c.Name != u.Container {
				continue
			}
			if c.Health == "healthy" || c.Health == "" {
				return nil
			}
			if !c.Running {
				return fmt.Errorf("%s exited", u.Name)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s is not healthy: %w", u.Name, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (m *Manager) containers(ctx context.Context, e Env) (map[string]*runtime.Container, error) {
	all, err := m.rt.Containers(ctx, m.opt.Instance)
	if err != nil {
		return nil, err
	}
	out := map[string]*runtime.Container{}
	for i := range all {
		out[all[i].Name] = &all[i]
	}
	return out, nil
}

// skipDirs are not code: changes there are tools at work, not a developer.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, ".venv": true, "venv": true,
	"__pycache__": true, ".next": true, ".nuxt": true, "dist": true, "build": true, "target": true, ".cache": true, "tmp": true, "log": true, "logs": true}

// latestChange is the newest modification time in the folders. It walks them: FSEvents
// and inotify would be cheaper, but a walk every few seconds is enough for activity.
func latestChange(dirs []string) time.Time {
	var latest time.Time
	for _, d := range dirs {
		_ = filepath.WalkDir(d, func(path string, de os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if de.IsDir() && path != d && skipDirs[de.Name()] {
				return filepath.SkipDir
			}
			if info, err := de.Info(); err == nil && info.ModTime().After(latest) {
				latest = info.ModTime()
			}
			return nil
		})
	}
	return latest
}
