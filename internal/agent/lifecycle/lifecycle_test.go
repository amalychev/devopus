package lifecycle

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/amalychev/devopus/gen/devopus/agent/v1"
	"github.com/amalychev/devopus/internal/agent/runtime"
)

type fakeRT struct {
	mu     sync.Mutex
	ctrs   map[string]*runtime.Container
	starts []string
	stops  []string
}

func (f *fakeRT) Containers(context.Context, string) ([]runtime.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []runtime.Container
	for _, c := range f.ctrs {
		out = append(out, *c)
	}
	return out, nil
}

func (f *fakeRT) StartContainer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctrs[id].Running = true
	f.starts = append(f.starts, id)
	return nil
}

func (f *fakeRT) StopContainer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctrs[id].Running = false
	f.stops = append(f.stops, id)
	return nil
}

type harness struct {
	m        *Manager
	rt       *fakeRT
	mu       sync.Mutex
	switches []string
	now      time.Time
}

func newHarness(t *testing.T, envs ...Env) *harness {
	h := &harness{rt: &fakeRT{ctrs: map[string]*runtime.Container{}}, now: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)}
	for _, e := range envs {
		for _, u := range e.Units {
			h.rt.ctrs[u.Container] = &runtime.Container{ID: u.Container, Name: u.Container, Running: true}
		}
	}
	h.m = New(h.rt, Options{
		File: filepath.Join(t.TempDir(), "lifecycle.json"),
		SetAwake: func(_ context.Context, envID string, awake bool) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.switches = append(h.switches, map[bool]string{true: "wake ", false: "sleep "}[awake]+envID)
			return nil
		},
		Answers: func(context.Context, string, string) error { return nil },
		Publish: func(string, func(*agentv1.EnvironmentStatus)) {},
		Kick:    func() {},
	})
	h.m.now = func() time.Time {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.now
	}
	h.m.Update(envs)
	return h
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

func feature(id string, maxRunning int) Env {
	return Env{ID: id, Project: "shop", Name: id, Host: id + ".shop.localhost", Ready: true, SleepAfter: time.Hour, MaxRunning: maxRunning,
		Units: []Unit{
			{Name: "db", Container: id + "-db", Role: "service"},
			{Name: "api", Container: id + "-api", Role: "module", Path: "/api"},
		}}
}

func TestIdleFeatureSleepsAndWakesOnce(t *testing.T) {
	h := newHarness(t, feature("f1", 0))
	ctx := context.Background()

	h.advance(59 * time.Minute)
	h.m.OnRequests("f1-api", 3) // a request through the proxy
	h.m.OnRequests("f1-shared-db-5432", 9)
	h.advance(30 * time.Minute)
	h.m.Check(ctx)
	if h.m.Asleep("f1") {
		t.Fatal("a feature used 30 minutes ago sleeps")
	}
	h.advance(31 * time.Minute)
	h.m.Check(ctx)
	if !h.m.Asleep("f1") {
		t.Fatal("an idle feature stays awake")
	}
	// Traffic to the waker first, then the module before what it uses.
	if !slices.Equal(h.switches, []string{"sleep f1"}) || !slices.Equal(h.rt.stops, []string{"f1-api", "f1-db"}) {
		t.Errorf("switches %v, stops %v", h.switches, h.rt.stops)
	}

	// Many requests, one wake: services first.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done, result := h.m.Wake("f1")
			<-done
			if err := result(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if h.m.Asleep("f1") || !slices.Equal(h.rt.starts, []string{"f1-db", "f1-api"}) {
		t.Errorf("asleep %v, starts %v", h.m.Asleep("f1"), h.rt.starts)
	}
	if !slices.Equal(h.switches, []string{"sleep f1", "wake f1"}) {
		t.Errorf("switches %v", h.switches)
	}
}

func TestBudgetSleepsTheLeastRecentlyUsed(t *testing.T) {
	h := newHarness(t, feature("f1", 2), feature("f2", 2), feature("f3", 2))
	ctx := context.Background()
	h.advance(time.Minute)
	h.m.Active("f1")
	h.advance(time.Minute)
	h.m.Active("f3")
	h.m.Check(ctx)
	if !h.m.Asleep("f2") || h.m.Asleep("f1") || h.m.Asleep("f3") {
		t.Fatalf("asleep: f1 %v f2 %v f3 %v", h.m.Asleep("f1"), h.m.Asleep("f2"), h.m.Asleep("f3"))
	}
	// Waking f2 puts the least recently used of the others to sleep, never f2 itself.
	done, _ := h.m.Wake("f2")
	<-done
	deadline := time.Now().Add(2 * time.Second)
	for !h.m.Asleep("f1") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !h.m.Asleep("f1") || h.m.Asleep("f2") || h.m.Asleep("f3") {
		t.Errorf("after waking f2: f1 %v f2 %v f3 %v", h.m.Asleep("f1"), h.m.Asleep("f2"), h.m.Asleep("f3"))
	}
}

func TestSleepSurvivesARestart(t *testing.T) {
	h := newHarness(t, feature("f1", 0))
	if err := h.m.Sleep(context.Background(), "f1", ReasonManual); err != nil {
		t.Fatal(err)
	}
	again := New(h.rt, Options{File: h.m.opt.File})
	if !again.Asleep("f1") {
		t.Error("a restarted agent forgot that f1 sleeps")
	}
}
