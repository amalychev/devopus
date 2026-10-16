package reconcile

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/dotenv"

	agentv2 "github.com/amalychev/devopus/gen/devopus/agent/v2"
	"github.com/amalychev/devopus/internal/agent/git"
	"github.com/amalychev/devopus/internal/agent/proxy"
	"github.com/amalychev/devopus/internal/agent/runtime"
	"github.com/amalychev/devopus/internal/agent/stack"
	"github.com/amalychev/devopus/internal/spec"
)

// SecretsFile holds the values of ${{ secrets.* }} next to workspace.yaml (stage 1).
const SecretsFile = ".env.secrets"

// errWaiting: the environment is fine but not ready yet; the step stays running and
// the agent looks again soon.
var errWaiting = errors.New("waiting")

// ErrNotReady is returned by a pass that left environments waiting for their applications.
var ErrNotReady = errors.New("environments are still starting")

// unitSteps are the steps that bring up what workspace.yaml describes. They run after
// the network, certificate, proxy and DNS steps of a running environment.
func (r *Reconciler) unitSteps(env *agentv2.EnvironmentSpec, root string, p *prepared, nw *runtime.Network, o *observed) []step {
	var sources map[string]string
	var heads map[string]string
	ran := map[string]bool{} // applications whose hooks ran in this pass
	steps := []step{
		{"source", func(ctx context.Context) (string, error) {
			var msg string
			var err error
			codeRoot := root
			if isFeature(env) {
				sources, heads, codeRoot, msg, err = r.ensureFeatureSources(ctx, env, root, p)
			} else {
				sources, heads, msg, err = r.ensureSources(ctx, env, root, p.ws, p.admin)
			}
			if err != nil {
				return "", err
			}
			p.sources = sources
			secrets, err := readSecrets(filepath.Join(root, SecretsFile))
			if err != nil {
				return "", err
			}
			plan, err := stack.Build(p.ws, stack.Env{
				ID: env.Id, Project: env.Project, Slug: slugOf(env), Root: root, CodeRoot: codeRoot, Isolation: p.iso,
				Network: nw.ID, Subnet: nw.Subnets[0], URL: p.url,
				Sources: sources, Heads: heads, Secrets: secrets,
				CA: r.caPEM(), Bundle: r.opt.Bundle,
				DNS:    proxy.DNSIP(nw.Subnets[0]),
				Labels: r.labels(env),
				BaseID: baseID(p),
			})
			if err != nil {
				return "", err
			}
			p.plan = plan
			notes := append(append([]string{}, p.notes...), plan.Notes...)
			r.update(env.Id, func(s *agentv2.EnvironmentStatus) { s.Notes = notes })
			return msg, nil
		}},
	}
	if r.asleep(env) {
		// Its containers are stopped on purpose; the waker starts them.
		return steps
	}
	steps = append(steps, step{"images", func(ctx context.Context) (string, error) {
		return r.ensureImages(ctx, env, p.plan)
	}})
	if isFeature(env) {
		steps = append(steps, step{"data", func(ctx context.Context) (string, error) {
			return r.ensureData(ctx, env, p, o)
		}})
	}
	steps = append(steps, []step{
		{"services", func(ctx context.Context) (string, error) {
			if err := r.ensureVolumes(ctx, env, p.plan, o.volumes[env.Id]); err != nil {
				return "", err
			}
			if err := r.removeStale(ctx, p.plan, o.byEnv[env.Id]); err != nil {
				return "", err
			}
			return r.ensureUnits(ctx, env, p, o, stack.RoleService, nil)
		}},
		{"install", func(ctx context.Context) (string, error) {
			return r.runHooks(ctx, env, p, o, "install", ran)
		}},
		{"migrate", func(ctx context.Context) (string, error) {
			return r.runHooks(ctx, env, p, o, "migrate", ran)
		}},
	}...)
	if !isFeature(env) {
		steps = append(steps, step{"golden", func(ctx context.Context) (string, error) {
			return r.ensureGolden(ctx, env, p)
		}})
	}
	return append(steps, step{"apps", func(ctx context.Context) (string, error) {
		return r.ensureUnits(ctx, env, p, o, stack.RoleApp, ran)
	}})
}

// baseID is the base environment of a feature; empty for a base environment.
func baseID(p *prepared) string {
	if p.base != nil && isFeature(p.env) {
		return p.base.env.Id
	}
	return ""
}

func slugOf(env *agentv2.EnvironmentSpec) string {
	if env.Slug != "" {
		return env.Slug
	}
	return "main"
}

func (r *Reconciler) caPEM() []byte {
	if r.opt.CA == nil {
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: r.opt.CA.Root.Raw})
}

// SourceDir is where an application from another repository is checked out for an environment.
func (r *Reconciler) SourceDir(project, envID, application string) string {
	return filepath.Join(r.opt.SourcesDir, project, envID, application)
}

// ensureSources finds the code of every active application: a folder of the repository, or
// a clone of the application's repository at its base branch, made once.
func (r *Reconciler) ensureSources(ctx context.Context, env *agentv2.EnvironmentSpec, root string, ws *spec.Project, admin string) (map[string]string, map[string]string, string, error) {
	applications, _ := stack.Active(ws)
	sources, heads := map[string]string{}, map[string]string{}
	var cloned []string
	rootHead := ""
	if h, err := git.Head(ctx, root); err == nil {
		rootHead = h
	}
	for _, name := range applications {
		m := ws.Apps[name]
		if m.Path != "" {
			dir := filepath.Join(root, filepath.FromSlash(m.Path))
			if st, err := os.Stat(dir); err != nil || !st.IsDir() {
				return nil, nil, "", fmt.Errorf("%s: folder %s does not exist", name, dir)
			}
			sources[name], heads[name] = dir, rootHead
			continue
		}
		dir := r.SourceDir(env.Project, env.Id, name)
		if !git.IsRepo(dir) {
			r.stepMessage(env.Id, "source", fmt.Sprintf("cloning %s", m.Repo))
			log, err := r.logFile(env.Id, "clone-"+name)
			if err != nil {
				return nil, nil, "", err
			}
			err = git.Clone(ctx, m.Repo, m.BaseBranch(), dir, log)
			log.Close()
			if git.NoAccess(err) {
				// The agent clones with the developer's own git keys (user-flows §4).
				whom := "the workspace admin"
				if admin != "" {
					whom += " (" + admin + ")"
				}
				return nil, nil, "", fmt.Errorf("%s: no access to %s with your git keys; ask %s to give your account access to this repository; the agent retries on its own (%w)", name, m.Repo, whom, err)
			}
			if err != nil {
				return nil, nil, "", fmt.Errorf("%s: %w", name, err)
			}
			cloned = append(cloned, name)
		}
		sources[name] = dir
		if h, err := git.Head(ctx, dir); err == nil {
			heads[name] = h
		}
	}
	checkouts := map[string]string{}
	for name, dir := range sources {
		if ws.Apps[name].Repo != "" {
			checkouts[name] = dir
		}
	}
	r.setSources(ctx, env.Id, sources, checkouts)
	msg := plural(len(applications), "application")
	if len(cloned) > 0 {
		msg += ", cloned " + strings.Join(cloned, ", ")
	}
	return sources, heads, msg, nil
}

func readSecrets(path string) (map[string]string, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	vals, err := dotenv.ReadFile(path, func(string) (string, bool) { return "", false })
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return vals, nil
}

// ensureImages builds the applications' images that do not exist yet and pulls the rest.
func (r *Reconciler) ensureImages(ctx context.Context, env *agentv2.EnvironmentSpec, plan *stack.Plan) (string, error) {
	var built []string
	for _, b := range plan.Builds {
		ok, err := r.rt.ImageExists(ctx, b.Tag)
		if err != nil {
			return "", err
		}
		if ok {
			continue
		}
		name := strings.TrimPrefix(b.Tag[:strings.LastIndex(b.Tag, ":")], "devopus/"+env.Project+"-")
		r.stepMessage(env.Id, "images", "building "+name)
		log, err := r.logFile(env.Id, "build-"+name)
		if err != nil {
			return "", err
		}
		err = r.rt.Build(ctx, b, log)
		log.Close()
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		built = append(built, name)
	}
	var pulled []string
	for _, img := range plan.Images {
		ok, err := r.rt.ImageExists(ctx, img)
		if err != nil {
			return "", err
		}
		if ok {
			continue
		}
		r.stepMessage(env.Id, "images", "pulling "+img)
		if err := r.rt.EnsureImage(ctx, img); err != nil {
			return "", err
		}
		pulled = append(pulled, img)
	}
	msg := plural(len(plan.Images)+len(plan.Builds), "image")
	if len(pulled) > 0 {
		msg += ", pulled " + strings.Join(pulled, ", ")
	}
	if len(built) > 0 {
		msg += ", built " + strings.Join(built, ", ")
	}
	return msg, nil
}

func (r *Reconciler) ensureVolumes(ctx context.Context, env *agentv2.EnvironmentSpec, plan *stack.Plan, have []runtime.Volume) error {
	exists := map[string]bool{}
	for _, v := range have {
		exists[v.Name] = true
	}
	for _, v := range plan.Volumes {
		if !exists[v] {
			if err := r.rt.CreateVolume(ctx, v, r.labels(env)); err != nil {
				return err
			}
		}
	}
	return nil
}

// removeStale removes containers of units workspace.yaml no longer has, and hook
// containers a crashed pass left behind. Volumes stay: they hold data.
func (r *Reconciler) removeStale(ctx context.Context, plan *stack.Plan, ctrs []runtime.Container) error {
	want := map[string]bool{}
	for _, u := range plan.Units {
		want[u.Spec.Name] = true
	}
	var errs []error
	for _, c := range ctrs {
		switch c.Labels[runtime.LabelRole] {
		case stack.RoleService, stack.RoleApp, stack.RoleWorker:
			if !want[c.Name] {
				errs = append(errs, r.rt.RemoveContainer(ctx, c.ID))
			}
		case stack.RoleHook, roleCopy:
			errs = append(errs, r.rt.RemoveContainer(ctx, c.ID))
		}
	}
	return errors.Join(errs...)
}

// unitReadyTimeout bounds the wait for a healthcheck to pass.
const unitReadyTimeout = 3 * time.Minute

// ensureUnits starts the units of one kind in order: services, or applications with their
// workers. A unit with a healthcheck must be healthy before the next one starts.
// Applications whose hooks just ran are restarted to pick up new dependencies.
func (r *Reconciler) ensureUnits(ctx context.Context, env *agentv2.EnvironmentSpec, p *prepared, o *observed, role string, restart map[string]bool) (string, error) {
	byName := map[string]*runtime.Container{}
	for i, c := range o.byEnv[env.Id] {
		byName[c.Name] = &o.byEnv[env.Id][i]
	}
	n := 0
	for _, u := range p.plan.Units {
		if u.Role != role && !(role == stack.RoleApp && u.Role == stack.RoleWorker) {
			continue
		}
		n++
		existing := byName[u.Spec.Name]
		if existing != nil && existing.Running && restart[u.Owner] && existing.Labels[runtime.LabelConfig] == u.Spec.Labels[runtime.LabelConfig] {
			if err := r.rt.StopContainer(ctx, existing.ID); err != nil {
				return "", fmt.Errorf("%s: %w", u.Name, err)
			}
			existing.Running = false
		}
		if existing == nil || !existing.Running || existing.Labels[runtime.LabelConfig] != u.Spec.Labels[runtime.LabelConfig] {
			r.stepMessage(env.Id, stepOf(role), "starting "+u.Name)
			p.started = true
		}
		if err := r.ensureContainer(ctx, u.Spec, existing); err != nil {
			return "", fmt.Errorf("%s: %w", u.Name, err)
		}
		if u.Healthcheck {
			if err := r.waitHealthy(ctx, env.Id, u); err != nil {
				return "", fmt.Errorf("%s: %w", u.Name, err)
			}
		}
	}
	return fmt.Sprintf("%d running", n), nil
}

func stepOf(role string) string {
	if role == stack.RoleService {
		return "services"
	}
	return "apps"
}

func (r *Reconciler) waitHealthy(ctx context.Context, envID string, u stack.Unit) error {
	ctx, cancel := context.WithTimeout(ctx, unitReadyTimeout)
	defer cancel()
	announced := false
	for {
		c, err := r.container(ctx, u.Spec.Name)
		if err != nil {
			return err
		}
		switch {
		case c == nil:
			return errors.New("the container is gone")
		case c.Health == "healthy":
			return nil
		case !c.Running:
			return r.explain(ctx, c.ID, fmt.Errorf("exited (%s)", c.State))
		}
		if !announced {
			r.stepMessage(envID, stepOf(u.Role), "waiting for "+u.Name+" to be healthy")
			announced = true
		}
		select {
		case <-ctx.Done():
			if c.Health == "unhealthy" {
				return r.explain(ctx, c.ID, errors.New("unhealthy"))
			}
			return fmt.Errorf("not healthy after %s", unitReadyTimeout)
		case <-time.After(time.Second):
		}
	}
}

// container finds one container of this instance by name.
func (r *Reconciler) container(ctx context.Context, name string) (*runtime.Container, error) {
	ctrs, err := r.rt.Containers(ctx, r.opt.Instance)
	if err != nil {
		return nil, err
	}
	for i := range ctrs {
		if ctrs[i].Name == name {
			return &ctrs[i], nil
		}
	}
	return nil, nil
}

// hookMarks remembers which hooks ran with which inputs, per environment.
type hookMarks map[string]string

func (r *Reconciler) marksFile(envID string) string {
	return filepath.Join(r.opt.EnvsDir, envID, "hooks.json")
}

func (r *Reconciler) loadMarks(envID string) hookMarks {
	m := hookMarks{}
	if data, err := os.ReadFile(r.marksFile(envID)); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	return m
}

func (r *Reconciler) saveMarks(envID string, m hookMarks) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.marksFile(envID)), 0o700); err != nil {
		return err
	}
	return os.WriteFile(r.marksFile(envID), data, 0o600)
}

// runHooks runs the hooks of one name whose inputs changed since they last succeeded.
func (r *Reconciler) runHooks(ctx context.Context, env *agentv2.EnvironmentSpec, p *prepared, o *observed, name string, ran map[string]bool) (string, error) {
	marks := r.loadMarks(env.Id)
	var done, skipped []string
	for _, h := range p.plan.Hooks {
		if h.Name != name {
			continue
		}
		id := h.App + "." + h.Name
		if marks[id] == h.Key {
			skipped = append(skipped, h.App)
			continue
		}
		r.stepMessage(env.Id, name, h.App+": "+h.Command)
		if err := r.runHook(ctx, env.Id, h); err != nil {
			return "", err
		}
		marks[id] = h.Key
		if err := r.saveMarks(env.Id, marks); err != nil {
			return "", err
		}
		ran[h.App] = true
		done = append(done, h.App)
	}
	switch {
	case len(done) > 0:
		return "ran in " + strings.Join(done, ", "), nil
	case len(skipped) > 0:
		return "up to date", nil
	}
	return "none", nil
}

// runHook runs a hook to completion in a one-shot container and keeps its output in
// the environment's logs.
func (r *Reconciler) runHook(ctx context.Context, envID string, h stack.Hook) error {
	if old, err := r.container(ctx, h.Spec.Name); err != nil {
		return err
	} else if old != nil {
		if err := r.rt.RemoveContainer(ctx, old.ID); err != nil {
			return err
		}
	}
	id, err := r.rt.CreateContainer(ctx, h.Spec)
	if err != nil {
		return fmt.Errorf("%s: %w", h.App, err)
	}
	defer func() { _ = r.rt.RemoveContainer(context.WithoutCancel(ctx), id) }()
	if err := r.rt.StartContainer(ctx, id); err != nil {
		return fmt.Errorf("%s: %w", h.App, r.explain(ctx, id, err))
	}
	code, err := r.rt.WaitContainer(ctx, id)
	logPath := ""
	if f, ferr := r.logFile(envID, h.App+"-"+h.Name); ferr == nil {
		_ = r.rt.CopyLogs(context.WithoutCancel(ctx), id, f)
		logPath = f.Name()
		f.Close()
	}
	if err != nil {
		return fmt.Errorf("%s: %w", h.App, err)
	}
	if code != 0 {
		err := fmt.Errorf("%s: %q exited with %d", h.App, h.Command, code)
		if logPath != "" {
			err = fmt.Errorf("%w (log: %s)", err, logPath)
		}
		return r.explain(ctx, id, err)
	}
	return nil
}

// logFile opens a fresh log of a hook, build or clone of an environment.
func (r *Reconciler) logFile(envID, name string) (*os.File, error) {
	dir := filepath.Join(r.opt.EnvsDir, envID, "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return os.Create(filepath.Join(dir, name+".log"))
}

// stepMessage tells watchers what a long step is doing right now.
func (r *Reconciler) stepMessage(envID, name, msg string) {
	r.update(envID, func(s *agentv2.EnvironmentStatus) {
		p := findStep(s, name)
		if p.State == agentv2.StepState_STEP_STATE_RUNNING {
			p.Message = msg
		}
	})
}

// stopUnits stops the containers of a stopped environment; they keep their data.
func (r *Reconciler) stopUnits(ctx context.Context, ctrs []runtime.Container) error {
	var errs []error
	for _, c := range ctrs {
		switch c.Labels[runtime.LabelRole] {
		case stack.RoleService, stack.RoleApp, stack.RoleWorker:
			if c.Running {
				errs = append(errs, r.rt.StopContainer(ctx, c.ID))
			}
		case stack.RoleHook, roleCopy:
			errs = append(errs, r.rt.RemoveContainer(ctx, c.ID))
		}
	}
	return errors.Join(errs...)
}

// unitStatuses describes the units of an environment as the runtime has them now.
func (r *Reconciler) unitStatuses(ctx context.Context, envID string, plan *stack.Plan, hosts map[string]string) []*agentv2.UnitStatus {
	ctrs, err := r.rt.Containers(ctx, r.opt.Instance)
	if err != nil {
		return nil
	}
	byName := map[string]runtime.Container{}
	for _, c := range ctrs {
		if c.Labels[runtime.LabelEnv] == envID {
			byName[c.Name] = c
		}
	}
	var out []*agentv2.UnitStatus
	add := func(name, role, container string, addr string, routes []string) {
		st := &agentv2.UnitStatus{Name: name, Role: role, Container: container, Address: addr, Routes: routes, State: "missing"}
		if c, ok := byName[container]; ok {
			switch {
			case c.Running && (c.Health == "starting" || c.Health == "unhealthy"):
				st.State = "starting"
				if c.Health == "unhealthy" {
					st.State = "unhealthy"
				}
			case c.Running:
				st.State = "running"
			case c.State == "exited":
				st.State = "exited"
				st.Error = r.rt.ContainerLogs(ctx, c.ID, 5)
			default:
				st.State = c.State
			}
		}
		out = append(out, st)
	}
	if plan != nil {
		for _, u := range plan.Units {
			addr := ""
			if u.IP.IsValid() {
				addr = netip.AddrPortFrom(u.IP, uint16(u.Port)).String()
				if u.Port == 0 {
					addr = u.IP.String()
				}
			}
			routes := u.Routes
			if u.Expose != "" {
				routes = []string{hosts[u.Name]}
			}
			add(u.Name, u.Role, u.Spec.Name, addr, routes)
		}
		return out
	}
	// Without a plan (a stopped environment): what the runtime has.
	var names []string
	for name, c := range byName {
		switch c.Labels[runtime.LabelRole] {
		case stack.RoleService, stack.RoleApp, stack.RoleWorker:
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		c := byName[name]
		add(c.Labels[stack.LabelUnit], c.Labels[runtime.LabelRole], name, "", nil)
	}
	return out
}
