package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// System git via exec, so the developer's SSH keys and settings apply (tech-stack.md).

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func currentBranch(dir string) (string, error) {
	return git(dir, "rev-parse", "--abbrev-ref", "HEAD")
}

func addWorktree(root, path, branch, from string) error {
	if _, err := git(root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		_, err := git(root, "worktree", "add", path, branch)
		return err
	}
	_, err := git(root, "worktree", "add", "-b", branch, path, from)
	return err
}

// unsafeToRemove reports work that would be lost with the worktree: uncommitted changes,
// or commits that are neither on a remote nor merged into the base branch.
func unsafeToRemove(path, baseBranch string) ([]string, error) {
	var problems []string
	status, err := git(path, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	if status != "" {
		n := len(strings.Split(status, "\n"))
		problems = append(problems, fmt.Sprintf("%d uncommitted change(s)", n))
	}
	ahead, err := git(path, "rev-list", "HEAD", "--not", "--remotes", baseBranch)
	if err != nil {
		return nil, err
	}
	if ahead != "" {
		n := len(strings.Split(ahead, "\n"))
		problems = append(problems, fmt.Sprintf("%d commit(s) not pushed and not merged into %s", n, baseBranch))
	}
	return problems, nil
}

func removeWorktree(root, path string) error {
	_, err := git(root, "worktree", "remove", "--force", path)
	return err
}
