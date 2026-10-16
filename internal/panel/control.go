package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/devopus/devopus/internal/agent/paths"
	"github.com/devopus/devopus/internal/daemon"
	"github.com/devopus/devopus/internal/service"
)

// ErrNotRunning: nothing answers on the panel's port.
var ErrNotRunning = errors.New("the panel is not running")

// Port is the panel's pinned port, or 0 before its first run.
func Port() int {
	b, err := os.ReadFile(paths.PanelPort())
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return p
}

// Health asks the running panel for its version.
func Health(ctx context.Context) (version string, port int, err error) {
	port = Port()
	if port == 0 {
		return "", 0, ErrNotRunning
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL(port)+HealthPath, nil)
	if err != nil {
		return "", port, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", port, ErrNotRunning
	}
	defer res.Body.Close()
	var h struct{ Version string }
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&h) != nil {
		return "", port, fmt.Errorf("port %d answers, but not as the Devopus panel", port)
	}
	return h.Version, port, nil
}

// Pid is the running panel's pid, or 0.
func Pid() int {
	pid, _ := daemon.Holder(paths.PanelLock())
	return pid
}

// Start starts the panel: its service, if installed, or a background process.
func Start() error {
	if service.Installed(service.Panel) {
		s, err := service.For(service.Panel)
		if err != nil {
			return err
		}
		return s.Start()
	}
	if err := os.MkdirAll(paths.Panel(), 0o700); err != nil {
		return err
	}
	return daemon.Spawn(paths.PanelLog(), "panel", "run")
}

// Stop stops the panel however it was started.
func Stop(ctx context.Context) error {
	if err := daemon.Stop(ctx, paths.PanelLock()); err != nil {
		return fmt.Errorf("stop the panel: %w", err)
	}
	return nil
}

// Wait waits until the panel answers and returns its port.
func Wait(ctx context.Context, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, port, err := Health(ctx); err == nil {
			return port, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 0, fmt.Errorf("the panel did not answer within %s; see %s", timeout, paths.PanelLog())
}

// Ensure returns the port of a running panel of this version, starting it, or
// restarting one of another version. started reports whether it did.
func Ensure(ctx context.Context, version string) (port int, started bool, err error) {
	running, port, err := Health(ctx)
	switch {
	case err == nil && (running == version || version == "dev" || running == "dev"):
		return port, false, nil
	case err == nil:
		if err := Stop(ctx); err != nil {
			return 0, false, err
		}
	case Pid() != 0:
		// Holds the lock but does not answer: starting, or stuck.
		if port, err := Wait(ctx, 5*time.Second); err == nil {
			return port, false, nil
		}
		if err := Stop(ctx); err != nil {
			return 0, false, err
		}
	}
	if err := Start(); err != nil {
		return 0, false, fmt.Errorf("start the panel: %w", err)
	}
	port, err = Wait(ctx, 10*time.Second)
	return port, true, err
}

// SignInURL is the link that signs the browser in.
func SignInURL(port int) (string, error) {
	token, err := os.ReadFile(paths.PanelToken())
	if err != nil {
		return "", fmt.Errorf("read the panel token: %w", err)
	}
	return BaseURL(port) + AuthPath + "?token=" + strings.TrimSpace(string(token)), nil
}
