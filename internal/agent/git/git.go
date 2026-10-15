// Package git runs the system git (docs/architecture/tech-stack.md): it brings the
// developer's SSH keys, credential helpers and settings.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Clone clones url at branch into dir. It clones into a temporary folder next to dir
// and renames it, so an interrupted clone never looks like a finished one.
func Clone(ctx context.Context, url, branch, dir string, log io.Writer) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), "."+filepath.Base(dir)+".clone-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	args := []string{"clone", "--progress"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	if _, err := run(ctx, "", log, append(args, "--", url, tmp)...); err != nil {
		return err
	}
	return os.Rename(tmp, dir)
}

// IsRepo reports whether dir is the top of a git checkout.
func IsRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// Head is the commit checked out in dir.
func Head(ctx context.Context, dir string) (string, error) {
	out, err := run(ctx, dir, nil, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

// Unsaved describes work in dir that exists nowhere else: uncommitted changes and
// commits on no remote. Empty means the folder can go.
func Unsaved(ctx context.Context, dir string) (string, error) {
	status, err := run(ctx, dir, nil, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	unpushed, err := run(ctx, dir, nil, "log", "--branches", "--not", "--remotes", "--oneline")
	if err != nil {
		return "", err
	}
	var parts []string
	if n := lines(status); n > 0 {
		parts = append(parts, fmt.Sprintf("%d uncommitted changes", n))
	}
	if n := lines(unpushed); n > 0 {
		parts = append(parts, fmt.Sprintf("%d unpushed commits", n))
	}
	return strings.Join(parts, ", "), nil
}

func lines(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// run executes git without prompts: a background agent has no terminal to ask in.
func run(ctx context.Context, dir string, log io.Writer, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	if os.Getenv("GIT_SSH_COMMAND") == "" {
		cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if log != nil {
		cmd.Stdout = io.MultiWriter(&stdout, log)
		cmd.Stderr = io.MultiWriter(&stderr, log)
	}
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			msg := strings.TrimSpace(stderr.String())
			if i := strings.LastIndex(msg, "\n"); i >= 0 && strings.HasPrefix(msg[i+1:], "fatal:") {
				msg = msg[i+1:]
			}
			return "", fmt.Errorf("git %s: %s", args[0], msg)
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return stdout.String(), nil
}
