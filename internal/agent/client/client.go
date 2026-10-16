// Package client connects the CLI to the local agent.
package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/devopus/devopus/gen/devopus/agent/v1"
	"github.com/devopus/devopus/gen/devopus/agent/v1/agentv1connect"
	"github.com/devopus/devopus/internal/agent/paths"
	"github.com/devopus/devopus/internal/daemon"
	"github.com/devopus/devopus/internal/service"
)

// New returns an API client over the agent's unix socket.
func New(socket string) agentv1connect.AgentServiceClient {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	// The host is ignored: every request goes to the socket.
	return agentv1connect.NewAgentServiceClient(&http.Client{Transport: tr}, "http://devopus-agent")
}

// ErrNotRunning: nothing answers on the socket.
var ErrNotRunning = errors.New("the agent is not running")

// Ping checks that an agent answers.
func Ping(ctx context.Context, c agentv1connect.AgentServiceClient) error {
	_, err := Version(ctx, c)
	return err
}

// Version returns the running agent's version.
func Version(ctx context.Context, c agentv1connect.AgentServiceClient) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	res, err := c.Status(ctx, connect.NewRequest(&agentv1.StatusRequest{}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeUnavailable {
			return "", ErrNotRunning
		}
		return "", err
	}
	return res.Msg.AgentVersion, nil
}

// Ensure returns a client to a running agent of this CLI's version, starting one if
// needed: through the service when `devopus agent install` registered it, otherwise in the
// background. An agent of another version (left running by an upgrade) is restarted;
// reconcile picks up where it was. started reports whether it started one.
func Ensure(ctx context.Context, version string) (c agentv1connect.AgentServiceClient, started bool, err error) {
	c = New(paths.Socket())
	running, err := Version(ctx, c)
	switch {
	case err == nil && (running == version || version == "dev" || running == "dev"):
		return c, false, nil
	case err == nil:
		if err := Stop(ctx); err != nil {
			return nil, false, fmt.Errorf("restart the agent %s as %s: %w", running, version, err)
		}
	case !errors.Is(err, ErrNotRunning):
		return nil, false, err
	}
	if err := Start(); err != nil {
		return nil, false, fmt.Errorf("start the agent: %w", err)
	}
	if err := Wait(ctx, 10*time.Second); err != nil {
		return nil, true, err
	}
	return c, true, nil
}

// Start starts the agent: the service, if installed, or a background process.
func Start() error {
	if service.Installed(service.Agent) {
		s, err := service.For(service.Agent)
		if err != nil {
			return err
		}
		return s.Start()
	}
	if err := paths.Ensure(); err != nil {
		return err
	}
	return daemon.Spawn(paths.Log(), "agent", "run")
}

// Wait waits until an agent answers.
func Wait(ctx context.Context, timeout time.Duration) error {
	c := New(paths.Socket())
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := Ping(ctx, c); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("the agent did not answer within %s; see %s", timeout, paths.Log())
}

// Stop stops the agent of this data directory, however it was started, and waits
// until it lets go of its lock.
func Stop(ctx context.Context) error {
	if err := daemon.Stop(ctx, paths.Lock()); err != nil {
		return fmt.Errorf("stop the agent: %w", err)
	}
	return nil
}

// Pid is the running agent's pid, or 0.
func Pid() int {
	pid, _ := daemon.Holder(paths.Lock())
	return pid
}
