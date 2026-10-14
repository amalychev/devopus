package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/docklet/docklet/gen/docklet/agent/v1"
	"github.com/docklet/docklet/gen/docklet/agent/v1/agentv1connect"
	"github.com/docklet/docklet/internal/agent/client"
	"github.com/docklet/docklet/internal/agent/paths"
	"github.com/docklet/docklet/internal/agent/runtime"
	"github.com/docklet/docklet/internal/agent/store"
)

// TestAgentEndToEnd runs the agent against the real runtime: up creates the
// environment network, purge removes it. Skipped when no runtime answers.
func TestAgentEndToEnd(t *testing.T) {
	rt, err := runtime.New(runtime.Detect())
	if err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := rt.Info(ctx); err != nil {
		t.Skipf("no container runtime: %v", err)
	}

	// unix socket paths are limited to ~104 bytes: t.TempDir() on macOS is too long.
	home, err := os.MkdirTemp("/tmp", "dk-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("DOCKLET_HOME", home)

	project := "e2e-" + store.NewID()
	done := make(chan error, 1)
	agentCtx, stop := context.WithCancel(ctx)
	go func() {
		done <- Run(agentCtx, Options{Version: "test", Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	}()
	t.Cleanup(func() {
		stop()
		<-done
	})

	c := client.New(paths.Socket())
	for i := 0; ; i++ {
		if err := client.Ping(ctx, c); err == nil {
			break
		} else if i > 100 {
			t.Fatalf("agent did not start: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// A second agent on the same data directory refuses to start.
	if err := Run(ctx, Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("second agent: %v", err)
	}

	config := fmt.Appendf(nil, "version: 1\nproject: %s\ndomain: %s.localhost\nmodules:\n  web:\n    repo: git@example.com:web.git\n    image: nginx:1.27\n    routes: [/]\n", project, project)
	invalid, err := c.Up(ctx, connect.NewRequest(&agentv1.UpRequest{WorkspacePath: "/src/x/workspace.yaml", Config: []byte("version: 1\n")}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("invalid config accepted: %v %v", invalid, err)
	}

	up, err := c.Up(ctx, connect.NewRequest(&agentv1.UpRequest{WorkspacePath: "/src/" + project + "/workspace.yaml", Config: config}))
	if err != nil {
		t.Fatal(err)
	}
	envID := up.Msg.EnvironmentId
	t.Cleanup(func() {
		// leave nothing behind even if the test fails half-way
		_, _ = c.Down(context.Background(), connect.NewRequest(&agentv1.DownRequest{Project: project, Purge: true}))
		time.Sleep(time.Second)
	})
	st := waitFor(t, ctx, c, envID, func(s *agentv1.EnvironmentStatus) bool { return s.Phase == agentv1.Phase_PHASE_RUNNING })
	if st.Network.GetSubnet() == "" {
		t.Fatalf("no network in %v", st)
	}
	if !hasNetwork(t, ctx, rt, envID) {
		t.Fatal("network not created")
	}

	if _, err := c.Down(ctx, connect.NewRequest(&agentv1.DownRequest{Project: project, Purge: true})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, c, envID, func(s *agentv1.EnvironmentStatus) bool { return s.Removed })
	if hasNetwork(t, ctx, rt, envID) {
		t.Fatal("network left behind")
	}
}

func waitFor(t *testing.T, ctx context.Context, c agentv1connect.AgentServiceClient, envID string, cond func(*agentv1.EnvironmentStatus) bool) *agentv1.EnvironmentStatus {
	t.Helper()
	w, err := c.Watch(ctx, connect.NewRequest(&agentv1.WatchRequest{EnvironmentId: envID}))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for w.Receive() {
		if s := w.Msg().Status; s != nil {
			if s.Phase == agentv1.Phase_PHASE_ERROR {
				t.Fatalf("environment failed: %s", s.Error)
			}
			if cond(s) {
				return s
			}
		}
	}
	t.Fatalf("watch ended: %v", w.Err())
	return nil
}

func hasNetwork(t *testing.T, ctx context.Context, rt *runtime.Docker, envID string) bool {
	t.Helper()
	nets, err := rt.Networks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nets {
		if n.Labels[runtime.LabelEnv] == envID {
			return true
		}
	}
	return false
}
