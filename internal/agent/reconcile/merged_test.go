package reconcile

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	agentv1 "github.com/amalychev/devopus/gen/devopus/agent/v1"
	"github.com/amalychev/devopus/internal/importer"
)

func TestCheckMerged(t *testing.T) {
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	repo := t.TempDir()
	git(repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "a"), []byte("a"), 0o644)
	git(repo, "add", ".")
	git(repo, "commit", "-q", "-m", "a")
	wt := filepath.Join(t.TempDir(), "wt")
	git(repo, "worktree", "add", "-q", "-b", "feature/x", wt)

	proj, _, err := importer.Parse([]byte("version: 1\nproject: p\ndomain: p.localhost\nmodules:\n  api:\n    path: api\n    runtime: node:22\n    command: node a.js\n"))
	if err != nil {
		t.Fatal(err)
	}
	r := New(nil, Options{EnvsDir: t.TempDir()}, nil)
	clock := time.Now()
	r.now = func() time.Time { return clock }
	env := &agentv1.EnvironmentSpec{Id: "f1", Kind: agentv1.EnvironmentKind_ENVIRONMENT_KIND_FEATURE, Branch: "feature/x", ForkedModules: []string{"api"}}
	p := &prepared{env: env, ws: proj}
	check := func() bool {
		clock = clock.Add(mergeCheck)
		r.checkMerged(context.Background(), env, p, map[string]string{"api": wt})
		return r.Statuses()[0].Merged
	}
	// A fresh branch has no work of its own: not merged, though main has all of it.
	if check() || check() {
		t.Fatal("a fresh branch counts as merged")
	}
	os.WriteFile(filepath.Join(wt, "b"), []byte("b"), 0o644)
	git(wt, "add", ".")
	git(wt, "commit", "-q", "-m", "b")
	if check() {
		t.Fatal("an unmerged branch counts as merged")
	}
	git(repo, "merge", "-q", "--ff-only", "feature/x")
	if !check() {
		t.Fatal("a merged branch is not recognised")
	}
}
