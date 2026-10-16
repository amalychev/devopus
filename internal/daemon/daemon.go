//go:build unix

// Package daemon runs Devopus components (the agent, the local panel) as background
// processes: one per data directory, guarded by a lock that also records the pid, so
// any CLI can find and stop the running one however it was started.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrAlreadyRunning: another process holds the lock.
var ErrAlreadyRunning = errors.New("already running")

// Lock takes an exclusive flock on path and writes the pid into it; the kernel
// releases it if the process dies.
func Lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (lock %s)", ErrAlreadyRunning, path)
		}
		return nil, err
	}
	_ = f.Truncate(0)
	fmt.Fprintf(f, "%d\n", os.Getpid())
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// Holder returns the pid of the process holding the lock at path, or 0.
func Holder(path string) (int, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return 0, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("a process holds %s but did not write its pid", path)
	}
	return pid, nil
}

// Stop sends SIGTERM to the holder of the lock and waits until it lets go. A process
// under a service manager exits cleanly, and launchd and systemd do not restart a
// clean exit.
func Stop(ctx context.Context, lock string) error {
	pid, err := Holder(lock)
	if err != nil || pid == 0 {
		return err
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if pid, _ := Holder(lock); pid == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("pid %d did not stop within 20s", pid)
}

// Spawn starts this binary with args, detached from the terminal, appending its
// output to logFile.
func Spawn(logFile string, args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
