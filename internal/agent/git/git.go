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
	"time"
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

// HasRef reports whether ref resolves to a commit in dir.
func HasRef(ctx context.Context, dir, ref string) bool {
	_, err := run(ctx, dir, nil, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}

// AddWorktree checks out a worktree of the repository at repo into dir. With a branch
// it checks that branch out: an existing local one, one from origin, or a new one from
// base. Without a branch the worktree is detached at base. A worktree already in dir is
// kept; one an interrupted run left half-made is made again.
func AddWorktree(ctx context.Context, repo, dir, branch, base string, log io.Writer) error {
	if _, err := os.Stat(dir); err == nil {
		if _, err := Head(ctx, dir); err == nil {
			return nil
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	// Forget worktrees whose folders are gone, or git refuses to add one at the same path.
	if _, err := run(ctx, repo, log, "worktree", "prune"); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	args := []string{"worktree", "add"}
	switch {
	case branch == "":
		args = append(args, "--detach", dir, base)
	case HasRef(ctx, repo, "refs/heads/"+branch):
		args = append(args, dir, branch)
	default:
		// A branch someone pushed already, such as a colleague's feature.
		fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, _ = run(fctx, repo, log, "fetch", "--quiet", "origin", "+refs/heads/"+branch+":refs/remotes/origin/"+branch)
		cancel()
		if HasRef(ctx, repo, "refs/remotes/origin/"+branch) {
			args = append(args, "--track", "-b", branch, dir, "origin/"+branch)
		} else {
			args = append(args, "--no-track", "-b", branch, dir, base)
		}
	}
	_, err := run(ctx, repo, log, args...)
	return err
}

// Branch is the branch checked out in dir; empty when HEAD is detached.
func Branch(ctx context.Context, dir string) (string, error) {
	out, err := run(ctx, dir, nil, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	if b := strings.TrimSpace(out); b != "HEAD" {
		return b, nil
	}
	return "", nil
}

// Switch puts the checkout in dir on branch, creating it at HEAD when it does not
// exist. Uncommitted changes come along.
func Switch(ctx context.Context, dir, branch string) error {
	if HasRef(ctx, dir, "refs/heads/"+branch) {
		_, err := run(ctx, dir, nil, "switch", branch)
		return err
	}
	_, err := run(ctx, dir, nil, "switch", "-c", branch)
	return err
}

// UnsavedWorktree describes work in a worktree that exists nowhere else: uncommitted
// changes, and commits of its HEAD that are on no remote and not in base (a merged
// feature is saved). Empty base checks against remotes only.
func UnsavedWorktree(ctx context.Context, dir, base string) (string, error) {
	status, err := run(ctx, dir, nil, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	args := []string{"log", "--oneline", "HEAD", "--not", "--remotes"}
	if base != "" && HasRef(ctx, dir, base) {
		args = append(args, base)
	}
	unpushed, err := run(ctx, dir, nil, args...)
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

// Lost describes what removing the folder of a worktree would lose: uncommitted
// changes, and commits of a detached HEAD on no branch and no remote. Commits on a
// branch stay with the branch.
func Lost(ctx context.Context, dir string) (string, error) {
	status, err := run(ctx, dir, nil, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	loose, err := run(ctx, dir, nil, "log", "--oneline", "HEAD", "--not", "--branches", "--remotes")
	if err != nil {
		return "", err
	}
	var parts []string
	if n := lines(status); n > 0 {
		parts = append(parts, fmt.Sprintf("%d uncommitted changes", n))
	}
	if n := lines(loose); n > 0 {
		parts = append(parts, fmt.Sprintf("%d commits on no branch", n))
	}
	return strings.Join(parts, ", "), nil
}

// TopLevel is the root of the repository that dir is in, and dir's path inside it.
func TopLevel(ctx context.Context, dir string) (top, prefix string, err error) {
	out, err := run(ctx, dir, nil, "rev-parse", "--show-toplevel", "--show-prefix")
	if err != nil {
		return "", "", err
	}
	parts := strings.SplitN(strings.TrimRight(out, "\n"), "\n", 2)
	top = parts[0]
	if len(parts) == 2 {
		prefix = strings.TrimSuffix(parts[1], "/")
	}
	return top, prefix, nil
}

// IsWorktree reports whether dir is a linked worktree rather than a repository.
func IsWorktree(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && !st.IsDir()
}

// RemoveWorktree removes a linked worktree and tells its repository. The branch stays.
func RemoveWorktree(ctx context.Context, dir string) error {
	common, err := run(ctx, dir, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return os.RemoveAll(dir)
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	_, err = run(ctx, "", nil, "--git-dir", strings.TrimSpace(common), "worktree", "prune")
	return err
}

// LinkedWorktrees are the folders of the linked worktrees of the repository at repo
// that still exist.
func LinkedWorktrees(ctx context.Context, repo string) ([]string, error) {
	out, err := run(ctx, repo, nil, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var dirs []string
	for i, l := range strings.Split(out, "\n") {
		p, ok := strings.CutPrefix(l, "worktree ")
		if !ok || i == 0 {
			continue // the first entry is the repository itself
		}
		if _, err := os.Stat(p); err == nil {
			dirs = append(dirs, p)
		}
	}
	return dirs, nil
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
