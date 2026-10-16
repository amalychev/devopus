package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	agentv1 "github.com/amalychev/devopus/gen/devopus/agent/v1"
	"github.com/amalychev/devopus/internal/agent/git"
	"github.com/amalychev/devopus/internal/agent/proxy"
	"github.com/amalychev/devopus/internal/agent/runtime"
	"github.com/amalychev/devopus/internal/agent/stack"
	"github.com/amalychev/devopus/internal/workspace"
)

// RepoCheckout is the folder of a feature's worktree of the project's own repository,
// where modules with path come from. Module names cannot start with an underscore.
const RepoCheckout = "_repo"

// isolate works out what a feature runs itself and how it reaches the rest of the
// base environment (Simple Isolation).
func (r *Reconciler) isolate(env *agentv1.EnvironmentSpec, p *prepared, nw runtime.Network, px *proxyState) (string, error) {
	iso, err := stack.Feature(p.ws, env.ForkedModules)
	if err != nil {
		return "", err
	}
	b := p.base
	switch {
	case b == nil:
		return "", errors.New("the project has no base environment")
	case b.plan == nil && b.err != nil:
		return "", fmt.Errorf("the base environment did not start (%s: %v)", b.failed, b.err)
	case b.plan == nil:
		return "", errors.New("the base environment is not running; 'devopus up' starts it")
	}
	units := map[string]stack.Unit{}
	for _, u := range b.plan.Units {
		units[u.Name] = u
	}
	sh := &proxy.Shared{IP: proxy.ProxyIP(nw.Subnets[0])}
	var notes []string
	for _, name := range iso.SharedModules {
		u, ok := units[name]
		if !ok {
			return "", fmt.Errorf("the base environment does not run %s", name)
		}
		sh.Modules = append(sh.Modules, proxy.SharedTarget{Name: name, Port: uint32(u.Port), Addr: netip.AddrPortFrom(u.IP, uint16(u.Port))})
	}
	for _, name := range iso.SharedServices {
		u, ok := units[name]
		if !ok {
			return "", fmt.Errorf("the base environment does not run %s", name)
		}
		port := p.ws.Services[name].Port
		if port == 0 {
			notes = append(notes, fmt.Sprintf("%s has no port in workspace.yaml: the feature cannot reach it", name))
			continue
		}
		sh.Services = append(sh.Services, proxy.SharedTarget{Name: name, Port: uint32(port), Addr: netip.AddrPortFrom(u.IP, uint16(port))})
	}
	if err := sh.Check(uint32(px.ports.HTTP), uint32(px.ports.HTTPS)); err != nil {
		return "", err
	}
	for _, m := range iso.NoMigrate {
		var on []string
		for _, u := range p.ws.Modules[m].Uses {
			if p.ws.Services[u] != nil && contains(iso.SharedServices, u) {
				on = append(on, u)
			}
		}
		notes = append(notes, fmt.Sprintf("migrations of %s are skipped: they would change %s of the base environment", m, strings.Join(on, ", ")))
	}
	p.iso, p.shared, p.notes = iso, sh, notes
	shared := append(append([]string{}, iso.SharedModules...), iso.SharedServices...)
	r.update(env.Id, func(s *agentv1.EnvironmentStatus) { s.Shared, s.Notes = shared, notes })

	msg := "forks " + strings.Join(iso.Forked, ", ")
	if d := iso.Dependents(); len(d) > 0 {
		msg += "; also runs " + strings.Join(d, ", ") + " (they use the forks)"
	}
	if len(iso.Services) > 0 {
		msg += "; own " + strings.Join(iso.Services, ", ")
	}
	if len(shared) > 0 {
		msg += "; shares " + strings.Join(shared, ", ") + " with the base"
	}
	return msg, nil
}

// ensureFeatureSources gives every module of a feature its own checkout: the forked
// ones on the feature branch, the others detached at their base branch. Modules with
// path share one worktree of the project's repository; modules with repo get a
// worktree of the base environment's clone.
func (r *Reconciler) ensureFeatureSources(ctx context.Context, env *agentv1.EnvironmentSpec, root string, p *prepared) (sources, heads map[string]string, codeRoot, msg string, err error) {
	ws, iso := p.ws, p.iso
	forked := map[string]bool{}
	for _, f := range env.ForkedModules {
		forked[f] = true
	}
	sources, heads = map[string]string{}, map[string]string{}
	checkouts := map[string]string{} // module → its worktree
	var made []string

	codeRoot = root
	repoBranch, repoBase, needRepo := "", "", false
	for _, name := range iso.Modules {
		if m := ws.Modules[name]; m.Path != "" {
			needRepo = true
			if forked[name] {
				repoBranch = env.Branch
			}
			if repoBase == "" {
				repoBase = m.BaseBranch()
			}
		}
	}
	if needRepo {
		top, prefix, err := git.TopLevel(ctx, root)
		if err != nil {
			return nil, nil, "", "", fmt.Errorf("modules with path need %s to be in a git repository to fork them: %w", root, err)
		}
		if !git.HasRef(ctx, top, repoBase) {
			repoBase = "HEAD"
		}
		dir := r.SourceDir(env.Project, env.Id, RepoCheckout)
		created, err := r.ensureWorktree(ctx, env.Id, top, dir, repoBranch, repoBase)
		if err != nil {
			return nil, nil, "", "", fmt.Errorf("worktree of %s: %w", top, err)
		}
		if created {
			made = append(made, "the repository")
		}
		codeRoot = filepath.Join(dir, filepath.FromSlash(prefix))
	}
	repoHead := ""
	if needRepo {
		repoHead, _ = git.Head(ctx, codeRoot)
	}
	for _, name := range iso.Modules {
		m := ws.Modules[name]
		if m.Path != "" {
			dir := filepath.Join(codeRoot, filepath.FromSlash(m.Path))
			if st, err := os.Stat(dir); err != nil || !st.IsDir() {
				return nil, nil, "", "", fmt.Errorf("%s: folder %s does not exist on the feature's branch", name, dir)
			}
			sources[name], heads[name] = dir, repoHead
			checkouts[name] = r.SourceDir(env.Project, env.Id, RepoCheckout)
			continue
		}
		// The base environment's clone holds the objects; a module the base does not run
		// is cloned there first.
		clone := r.SourceDir(env.Project, p.base.env.Id, name)
		if !git.IsRepo(clone) {
			r.stepMessage(env.Id, "source", "cloning "+m.Repo)
			log, err := r.logFile(env.Id, "clone-"+name)
			if err != nil {
				return nil, nil, "", "", err
			}
			err = git.Clone(ctx, m.Repo, m.BaseBranch(), clone, log)
			log.Close()
			if err != nil {
				return nil, nil, "", "", fmt.Errorf("%s: %w", name, err)
			}
		}
		branch := ""
		if forked[name] {
			branch = env.Branch
		}
		dir := r.SourceDir(env.Project, env.Id, name)
		created, err := r.ensureWorktree(ctx, env.Id, clone, dir, branch, m.BaseBranch())
		if err != nil {
			return nil, nil, "", "", fmt.Errorf("%s: %w", name, err)
		}
		if created {
			made = append(made, name)
		}
		sources[name], checkouts[name] = dir, dir
		if h, err := git.Head(ctx, dir); err == nil {
			heads[name] = h
		}
	}
	r.setSources(ctx, env.Id, sources, checkouts)
	r.checkMerged(ctx, env, p, checkouts)
	if needRepo {
		r.update(env.Id, func(s *agentv1.EnvironmentStatus) { s.CodeRoot = codeRoot })
	}
	msg = fmt.Sprintf("%s on %s", plural(len(iso.Modules), "module"), env.Branch)
	if len(made) > 0 {
		msg += ", new worktrees: " + strings.Join(made, ", ")
	}
	return sources, heads, codeRoot, msg, nil
}

// ensureWorktree makes a worktree, or moves an existing detached one onto the branch
// when its module was forked later. A worktree on another branch is left alone: the
// developer switched it.
func (r *Reconciler) ensureWorktree(ctx context.Context, envID, repo, dir, branch, base string) (bool, error) {
	if git.IsRepo(dir) {
		if _, err := git.Head(ctx, dir); err == nil {
			if branch == "" {
				return false, nil
			}
			cur, err := git.Branch(ctx, dir)
			if err != nil || cur != "" {
				return false, err
			}
			return false, git.Switch(ctx, dir, branch)
		}
	}
	r.stepMessage(envID, "source", "creating a worktree in "+dir)
	log, err := r.logFile(envID, "worktree-"+filepath.Base(dir))
	if err != nil {
		return false, err
	}
	defer log.Close()
	return true, git.AddWorktree(ctx, repo, dir, branch, base, log)
}

// setSources publishes where the code of each module is.
func (r *Reconciler) setSources(ctx context.Context, envID string, sources, checkouts map[string]string) {
	names := make([]string, 0, len(sources))
	for n := range sources {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []*agentv1.ModuleSource
	for _, n := range names {
		src := &agentv1.ModuleSource{Module: n, Path: sources[n], Checkout: checkouts[n]}
		if src.Checkout != "" {
			src.Branch, _ = git.Branch(ctx, src.Checkout)
		}
		out = append(out, src)
	}
	r.update(envID, func(s *agentv1.EnvironmentStatus) { s.Sources = out })
}

// Unsaved describes, per worktree of a feature, work that exists nowhere else:
// uncommitted changes and commits on no remote and not in the base branch. Removing a
// feature without force refuses while there is any.
func (r *Reconciler) Unsaved(ctx context.Context, env *agentv1.EnvironmentSpec, ws *workspace.Workspace) (map[string]string, error) {
	dir := filepath.Join(r.opt.SourcesDir, env.Project, env.Id)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range entries {
		src := filepath.Join(dir, e.Name())
		if !git.IsWorktree(src) {
			continue
		}
		base := ""
		if ws != nil {
			if m := ws.Modules[e.Name()]; m != nil {
				base = m.BaseBranch()
			} else if e.Name() == RepoCheckout {
				base = repoBaseBranch(ws)
			}
		}
		u, err := git.UnsavedWorktree(ctx, src, base)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", src, err)
		}
		if u != "" {
			out[src] = u
		}
	}
	return out, nil
}

// repoBaseBranch is the base branch of the project's own repository: that of its
// modules with path.
func repoBaseBranch(ws *workspace.Workspace) string {
	for _, n := range ws.ModuleNames() {
		if m := ws.Modules[n]; m.Path != "" {
			return m.BaseBranch()
		}
	}
	return ""
}

// Force lets the next collection remove an environment's worktrees even with work in
// them: the developer asked for it (devopus feature rm --force).
func (r *Reconciler) Force(envID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forced[envID] = true
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// mergeCheck is how often a feature's branch is checked against its base branch; the
// check fetches the base branch from origin.
const mergeCheck = 5 * time.Minute

// forksFile keeps where each worktree of a feature started: a branch merged into its
// base has commits of its own, and all of them are in the base.
func (r *Reconciler) forksFile(envID string) string {
	return filepath.Join(r.opt.EnvsDir, envID, "forks.json")
}

// checkMerged marks a feature whose branch is merged into its base branch, so people
// can remove it (features.cleanup: on_merge). Squash merges keep no ancestry and are
// not recognised.
func (r *Reconciler) checkMerged(ctx context.Context, env *agentv1.EnvironmentSpec, p *prepared, checkouts map[string]string) {
	if p.ws.Policy().Cleanup != "on_merge" || env.Branch == "" {
		return
	}
	r.mu.Lock()
	last, merged := r.mergeAt[env.Id], r.merged[env.Id]
	r.mu.Unlock()
	if r.now().Sub(last) < mergeCheck {
		r.setMerged(env, merged)
		return
	}
	forks := map[string]string{}
	if b, err := os.ReadFile(r.forksFile(env.Id)); err == nil {
		_ = json.Unmarshal(b, &forks)
	}
	changed := false
	merged = false
	seen := map[string]bool{}
	for _, m := range env.ForkedModules {
		dir := checkouts[m]
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		if b, err := git.Branch(ctx, dir); err != nil || b != env.Branch {
			continue
		}
		head, err := git.Head(ctx, dir)
		if err != nil {
			continue
		}
		if forks[dir] == "" {
			forks[dir], changed = head, true
			continue
		}
		base := p.ws.Modules[m].BaseBranch()
		if git.FetchBranch(ctx, dir, base) == nil && git.HasRef(ctx, dir, "refs/remotes/origin/"+base) {
			base = "origin/" + base
		} else if !git.HasRef(ctx, dir, base) {
			continue
		}
		own, err1 := git.Count(ctx, dir, forks[dir]+"..HEAD")
		left, err2 := git.Count(ctx, dir, base+"..HEAD")
		if err1 == nil && err2 == nil && own > 0 && left == 0 {
			merged = true
		}
	}
	if changed {
		if b, err := json.Marshal(forks); err == nil && os.MkdirAll(filepath.Dir(r.forksFile(env.Id)), 0o700) == nil {
			if err := os.WriteFile(r.forksFile(env.Id), b, 0o600); err != nil {
				r.opt.Log.Warn("fork points", "env", env.Id, "error", err)
			}
		}
	}
	r.mu.Lock()
	r.mergeAt[env.Id], r.merged[env.Id] = r.now(), merged
	r.mu.Unlock()
	r.setMerged(env, merged)
}

func (r *Reconciler) setMerged(env *agentv1.EnvironmentSpec, merged bool) {
	r.update(env.Id, func(s *agentv1.EnvironmentStatus) { s.Merged = merged })
}
