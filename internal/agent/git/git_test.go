package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// repo makes a remote with one commit on main and a clone of it.
func repo(t *testing.T) (remote, clone string) {
	t.Helper()
	dir := t.TempDir()
	remote, clone = filepath.Join(dir, "remote.git"), filepath.Join(dir, "clone")
	sh(t, dir, "init", "--bare", "-b", "main", remote)
	seed := filepath.Join(dir, "seed")
	sh(t, dir, "clone", remote, seed)
	os.WriteFile(filepath.Join(seed, "a.txt"), []byte("a"), 0o644)
	sh(t, seed, "add", ".")
	sh(t, seed, "commit", "-m", "a")
	sh(t, seed, "push", "origin", "HEAD:main")
	sh(t, seed, "push", "origin", "HEAD:feature/pushed")
	if err := Clone(context.Background(), remote, "main", clone, nil); err != nil {
		t.Fatal(err)
	}
	return remote, clone
}

func TestWorktrees(t *testing.T) {
	ctx := context.Background()
	_, clone := repo(t)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	wt := filepath.Join(t.TempDir(), "f1", "api")

	if err := AddWorktree(ctx, clone, wt, "feature/x", "main", nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := Branch(ctx, wt); b != "feature/x" {
		t.Errorf("branch = %q", b)
	}
	if !IsWorktree(wt) || IsWorktree(clone) {
		t.Error("IsWorktree")
	}
	// Again: nothing to do.
	if err := AddWorktree(ctx, clone, wt, "feature/x", "main", nil); err != nil {
		t.Fatal(err)
	}
	if u, err := UnsavedWorktree(ctx, wt, "main"); err != nil || u != "" {
		t.Errorf("fresh worktree unsaved = %q, %v", u, err)
	}
	os.WriteFile(filepath.Join(wt, "b.txt"), []byte("b"), 0o644)
	if u, _ := UnsavedWorktree(ctx, wt, "main"); u != "1 uncommitted changes" {
		t.Errorf("unsaved = %q", u)
	}
	sh(t, wt, "add", ".")
	sh(t, wt, "commit", "-m", "b")
	if u, _ := UnsavedWorktree(ctx, wt, "main"); u != "1 unpushed commits" {
		t.Errorf("unsaved = %q", u)
	}
	// Merged into the base branch: saved.
	sh(t, clone, "merge", "--ff-only", "feature/x")
	if u, _ := UnsavedWorktree(ctx, wt, "main"); u != "" {
		t.Errorf("merged unsaved = %q", u)
	}

	// A branch from origin, and a detached worktree switched to a branch later.
	pushed := filepath.Join(t.TempDir(), "pushed")
	if err := AddWorktree(ctx, clone, pushed, "feature/pushed", "main", nil); err != nil {
		t.Fatal(err)
	}
	if up := sh(t, pushed, "rev-parse", "--abbrev-ref", "@{upstream}"); up != "origin/feature/pushed" {
		t.Errorf("upstream = %q", up)
	}
	detached := filepath.Join(t.TempDir(), "web")
	if err := AddWorktree(ctx, clone, detached, "", "main", nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := Branch(ctx, detached); b != "" {
		t.Errorf("detached branch = %q", b)
	}
	if err := Switch(ctx, detached, "feature/y"); err != nil {
		t.Fatal(err)
	}
	if b, _ := Branch(ctx, detached); b != "feature/y" {
		t.Errorf("switched branch = %q", b)
	}

	list, err := LinkedWorktrees(ctx, clone)
	if err != nil || len(list) != 3 {
		t.Errorf("worktrees = %v, %v", list, err)
	}
	if err := RemoveWorktree(ctx, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Error("worktree folder stays")
	}
	if list, _ := LinkedWorktrees(ctx, clone); len(list) != 2 {
		t.Errorf("after remove: %v", list)
	}
	// The branch stays with its commits; the same worktree can come back.
	if !HasRef(ctx, clone, "refs/heads/feature/x") {
		t.Error("branch removed")
	}
	if err := AddWorktree(ctx, clone, wt, "feature/x", "main", nil); err != nil {
		t.Fatal(err)
	}

	// A folder an interrupted add left behind is made again.
	half := filepath.Join(t.TempDir(), "half")
	os.MkdirAll(half, 0o755)
	os.WriteFile(filepath.Join(half, ".git"), []byte("gitdir: /nowhere"), 0o644)
	if err := AddWorktree(ctx, clone, half, "", "main", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Head(ctx, half); err != nil {
		t.Error(err)
	}
}

func TestNoAccess(t *testing.T) {
	for _, msg := range []string{
		"git clone: git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.",
		"git clone: remote: Repository not found.\nfatal: repository 'https://github.com/acme/secret.git/' not found",
		"git clone: fatal: Authentication failed for 'https://gitlab.com/acme/api.git/'",
		"git clone: fatal: could not read Username for 'https://github.com': terminal prompts disabled",
	} {
		if !NoAccess(errors.New(msg)) {
			t.Errorf("not recognised: %s", msg)
		}
	}
	if NoAccess(errors.New("git clone: fatal: unable to access: Could not resolve host: github.com")) || NoAccess(nil) {
		t.Error("a network error is not about access")
	}
}
