// Package client connects the CLI to the local agent.
package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/docklet/docklet/gen/docklet/agent/v1"
	"github.com/docklet/docklet/gen/docklet/agent/v1/agentv1connect"
	"github.com/docklet/docklet/internal/agent/paths"
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
	return agentv1connect.NewAgentServiceClient(&http.Client{Transport: tr}, "http://docklet-agent")
}

// ErrNotRunning: nothing answers on the socket.
var ErrNotRunning = errors.New("the agent is not running")

// Ping checks that an agent answers.
func Ping(ctx context.Context, c agentv1connect.AgentServiceClient) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := c.Status(ctx, connect.NewRequest(&agentv1.StatusRequest{}))
	if err != nil && connect.CodeOf(err) == connect.CodeUnavailable {
		return ErrNotRunning
	}
	return err
}

// Ensure returns a client to a running agent, starting one in the background if
// needed. started reports whether it did. Until `docklet install` registers a service
// (milestone M9), this is how the agent comes up.
func Ensure(ctx context.Context) (c agentv1connect.AgentServiceClient, started bool, err error) {
	c = New(paths.Socket())
	if err := Ping(ctx, c); err == nil {
		return c, false, nil
	} else if !errors.Is(err, ErrNotRunning) {
		return nil, false, err
	}
	if err := spawn(); err != nil {
		return nil, false, fmt.Errorf("start the agent: %w", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := Ping(ctx, c); err == nil {
			return c, true, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, true, fmt.Errorf("the agent did not answer within 10s; see %s", paths.Log())
}

// spawn starts `docklet agent run` detached from the terminal, logging to a file.
func spawn() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := paths.Ensure(); err != nil {
		return err
	}
	logf, err := os.OpenFile(paths.Log(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, "agent", "run")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
