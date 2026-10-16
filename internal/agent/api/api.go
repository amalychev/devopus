// Package api serves the agent's local API (proto/devopus/agent/v1) to the CLI and
// the local panel.
package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"connectrpc.com/connect"

	agentv2 "github.com/amalychev/devopus/gen/devopus/agent/v2"
	"github.com/amalychev/devopus/gen/devopus/agent/v2/agentv2connect"
	"github.com/amalychev/devopus/internal/agent/stack"
	"github.com/amalychev/devopus/internal/agent/store"
	"github.com/amalychev/devopus/internal/importer"
	"github.com/amalychev/devopus/internal/spec"
	"github.com/amalychev/devopus/internal/workspace"
)

// Backend is what the API needs from the running agent.
type Backend interface {
	Store() *store.Store
	Statuses() []*agentv2.EnvironmentStatus
	Runtime() *agentv2.RuntimeInfo
	Version() string
	Proxy() *agentv2.ProxyStatus
	Routes() []*agentv2.Route
	CA() *agentv2.CAInfo
	// Account is the user the agent works for; nil when nobody is signed in.
	Account() *agentv2.AccountInfo
	// Link is the state of the link to the workspace server.
	Link() *agentv2.LinkInfo
	// Unsaved describes work in a feature's worktrees that exists nowhere else.
	Unsaved(ctx context.Context, env *agentv2.EnvironmentSpec, ws *spec.Project) (map[string]string, error)
	// Force lets the removal of a feature drop its worktrees even with work in them.
	Force(envID string)
	// Sleep and Wake switch a feature; they return once it started switching.
	Sleep(envID string) error
	Wake(envID string) error
	Containers() Containers
	// EnvsDir holds the hook and build logs of each environment.
	EnvsDir() string
}

type Server struct {
	b   Backend
	hub *Hub
}

var _ agentv2connect.AgentServiceHandler = (*Server)(nil)

func New(b Backend, hub *Hub) *Server { return &Server{b: b, hub: hub} }

func (s *Server) Status(context.Context, *connect.Request[agentv2.StatusRequest]) (*connect.Response[agentv2.StatusResponse], error) {
	st := s.b.Store().Snapshot()
	projects := st.Projects
	for _, p := range projects {
		p.Config, p.Spec, p.Secrets, p.KeyWrap = nil, nil, nil, nil // the CLI needs names and roots, not files
	}
	return connect.NewResponse(&agentv2.StatusResponse{
		AgentVersion:    s.b.Version(),
		DesiredRevision: st.Revision,
		Runtime:         s.b.Runtime(),
		Projects:        projects,
		Environments:    s.b.Statuses(),
		Proxy:           s.b.Proxy(),
		Ca:              s.b.CA(),
		Account:         s.b.Account(),
		Link:            s.b.Link(),
	}), nil
}

func (s *Server) Routes(_ context.Context, req *connect.Request[agentv2.RoutesRequest]) (*connect.Response[agentv2.RoutesResponse], error) {
	var out []*agentv2.Route
	for _, r := range s.b.Routes() {
		if req.Msg.EnvironmentId == "" || r.EnvironmentId == req.Msg.EnvironmentId {
			out = append(out, r)
		}
	}
	return connect.NewResponse(&agentv2.RoutesResponse{Routes: out, Proxy: s.b.Proxy()}), nil
}

func (s *Server) SetProxy(_ context.Context, req *connect.Request[agentv2.SetProxyRequest]) (*connect.Response[agentv2.SetProxyResponse], error) {
	m := req.Msg
	for _, p := range []uint32{m.CoexistHttpPort, m.CoexistHttpsPort} {
		if p > 65535 {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("port %d is out of range", p))
		}
		if p == 80 || p == 443 {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("the dev server must move off 80/443 for Devopus to take them; pick other ports such as 8080 and 8443"))
		}
	}
	if m.CoexistHttpPort == 0 && m.CoexistHttpsPort != 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("coexistence needs the HTTP port too"))
	}
	rev, err := s.b.Store().SetProxy(m.CoexistHttpPort, m.CoexistHttpsPort, m.Restart)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentv2.SetProxyResponse{Revision: rev}), nil
}

func (s *Server) Up(_ context.Context, req *connect.Request[agentv2.UpRequest]) (*connect.Response[agentv2.UpResponse], error) {
	if d := req.Msg.Dir; d != "" {
		if st, err := os.Stat(d); err != nil || !st.IsDir() || !filepath.IsAbs(d) {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("--dir %s: not an absolute path to a folder", d))
		}
		if _, err := s.b.Store().Link(req.Msg.Project, d); err != nil {
			return nil, storeError(err)
		}
	}
	ws, path, config, err := s.workspaceOf(req.Msg.Project, req.Msg.WorkspacePath, req.Msg.Config)
	if err != nil {
		return nil, err
	}
	envID, rev, err := s.b.Store().Up(ws.Name, filepath.Dir(path), config)
	if err != nil {
		return nil, storeError(err)
	}
	return connect.NewResponse(&agentv2.UpResponse{EnvironmentId: envID, Revision: rev}), nil
}

// parseWorkspace checks workspace.yaml: the agent does it itself, the CLI may be older
// or newer than the agent.
func parseWorkspace(path string, config []byte) (*spec.Project, error) {
	if !filepath.IsAbs(path) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("workspace_path must be absolute, got %q", path))
	}
	res, err := workspace.Parse(config)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s: %w", path, err))
	}
	if res.Diagnostics.HasErrors() {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s is invalid:\n%s", path, res.Diagnostics.Errors().Error()))
	}
	p, err := importer.FromV1(res.Workspace)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s: %w", path, err))
	}
	return p, nil
}

func storeError(err error) error {
	switch {
	case errors.Is(err, store.ErrProjectConflict), errors.Is(err, store.ErrFeatureExists):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, store.ErrUnknownProject), errors.Is(err, store.ErrUnknownFeature):
		return connect.NewError(connect.CodeNotFound, err)
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce
	}
	return connect.NewError(connect.CodeInternal, err)
}

func (s *Server) CreateFeature(_ context.Context, req *connect.Request[agentv2.CreateFeatureRequest]) (*connect.Response[agentv2.CreateFeatureResponse], error) {
	m := req.Msg
	ws, path, config, err := s.workspaceOf(m.Project, m.WorkspacePath, m.Config)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(m.Name) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("a feature needs a name"))
	}
	if _, err := stack.Feature(ws, m.Apps); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	branch := m.Branch
	if branch == "" {
		branch = ws.Policy().BranchPrefix + m.Name
	}
	if !validBranch(branch) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%q is not a valid branch name; pass --branch", branch))
	}
	var reserved []string
	for _, n := range ws.ServiceNames() {
		if e := ws.Services[n].Expose; e != "" {
			reserved = append(reserved, e)
		}
	}
	f, base, rev, err := s.b.Store().CreateFeature(ws.Name, filepath.Dir(path), config,
		store.NewFeature{Name: m.Name, Branch: branch, Apps: m.Apps, Reserved: reserved})
	if err != nil {
		return nil, storeError(err)
	}
	return connect.NewResponse(&agentv2.CreateFeatureResponse{EnvironmentId: f.Id, Slug: f.Slug, Branch: f.Branch, Revision: rev, BaseEnvironmentId: base.Id}), nil
}

func (s *Server) AddFeatureApps(_ context.Context, req *connect.Request[agentv2.AddFeatureAppsRequest]) (*connect.Response[agentv2.AddFeatureAppsResponse], error) {
	m := req.Msg
	ws, path, config, err := s.workspaceOf(m.Project, m.WorkspacePath, m.Config)
	if err != nil {
		return nil, err
	}
	if len(m.Apps) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name the applications to fork"))
	}
	if _, err := stack.Feature(ws, m.Apps); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	env, rev, err := s.b.Store().AddFeatureApps(ws.Name, filepath.Dir(path), config, m.Feature, m.Apps)
	if err != nil {
		return nil, storeError(err)
	}
	return connect.NewResponse(&agentv2.AddFeatureAppsResponse{EnvironmentId: env.Id, Revision: rev}), nil
}

func (s *Server) RemoveFeature(ctx context.Context, req *connect.Request[agentv2.RemoveFeatureRequest]) (*connect.Response[agentv2.RemoveFeatureResponse], error) {
	m := req.Msg
	var ws *spec.Project
	for _, p := range s.b.Store().Snapshot().Projects {
		if p.Name == m.Project {
			if proj, err := store.ParseProject(p); err == nil {
				ws = proj
			}
		}
	}
	env, rev, err := s.b.Store().RemoveFeature(m.Project, m.Feature, func(env *agentv2.EnvironmentSpec) error {
		if m.Force {
			return nil
		}
		unsaved, err := s.b.Unsaved(ctx, env, ws)
		if err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}
		if len(unsaved) == 0 {
			return nil
		}
		dirs := make([]string, 0, len(unsaved))
		for d := range unsaved {
			dirs = append(dirs, d)
		}
		sort.Strings(dirs)
		var b strings.Builder
		fmt.Fprintf(&b, "feature %s has work that exists nowhere else:", env.Name)
		for _, d := range dirs {
			fmt.Fprintf(&b, "\n  %s: %s", d, unsaved[d])
		}
		b.WriteString("\ncommit and push it, or remove the feature with --force (commits stay on the branch)")
		return connect.NewError(connect.CodeFailedPrecondition, errors.New(b.String()))
	})
	if err != nil {
		return nil, storeError(err)
	}
	if m.Force {
		s.b.Force(env.Id)
	}
	return connect.NewResponse(&agentv2.RemoveFeatureResponse{EnvironmentId: env.Id, Revision: rev}), nil
}

func (s *Server) ResetData(_ context.Context, req *connect.Request[agentv2.ResetDataRequest]) (*connect.Response[agentv2.ResetDataResponse], error) {
	m := req.Msg
	var ws *spec.Project
	st := s.b.Store()
	for _, p := range st.Snapshot().Projects {
		if p.Name == m.Project {
			if proj, err := store.ParseProject(p); err == nil {
				ws = proj
			}
		}
	}
	if ws == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%w %q", store.ErrUnknownProject, m.Project))
	}
	// The databases of the environment: in a feature those it runs itself.
	_, candidates := stack.Active(ws)
	if m.Environment != "" {
		f := st.Feature(m.Project, m.Environment)
		if f == nil {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%w %q in %s", store.ErrUnknownFeature, m.Environment, m.Project))
		}
		iso, err := stack.Feature(ws, f.ForkedApps)
		if err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		candidates = iso.Services
	}
	var dbs []string
	for _, n := range candidates {
		if stack.Engine(ws.Services[n].Image) != "" {
			dbs = append(dbs, n)
		}
	}
	services := m.Services
	if len(services) == 0 {
		services = dbs
	}
	for _, svc := range services {
		if !slices.Contains(dbs, svc) {
			where := "the base environment"
			if m.Environment != "" {
				where = "feature " + m.Environment
			}
			have := strings.Join(dbs, ", ")
			if have == "" {
				have = "none"
			}
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s has no database %q of its own to copy (Postgres or MySQL with a volume); it has: %s", where, svc, have))
		}
	}
	if len(services) == 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("no databases to copy: Postgres or MySQL services with a volume"))
	}
	env, rev, err := st.ResetData(m.Project, m.Environment, services)
	if err != nil {
		return nil, storeError(err)
	}
	return connect.NewResponse(&agentv2.ResetDataResponse{EnvironmentId: env.Id, Revision: rev, Services: services}), nil
}

func (s *Server) SetSleep(_ context.Context, req *connect.Request[agentv2.SetSleepRequest]) (*connect.Response[agentv2.SetSleepResponse], error) {
	f := s.b.Store().Feature(req.Msg.Project, req.Msg.Feature)
	if f == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%w %q in %s", store.ErrUnknownFeature, req.Msg.Feature, req.Msg.Project))
	}
	if f.Phase == agentv2.DesiredPhase_DESIRED_PHASE_STOPPED {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s is stopped; 'devopus up' starts it", f.Name))
	}
	do := s.b.Wake
	if req.Msg.Asleep {
		do = s.b.Sleep
	}
	if err := do(f.Id); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewResponse(&agentv2.SetSleepResponse{EnvironmentId: f.Id}), nil
}

// validBranch is a rough git check-ref-format: what people type into a feature name.
func validBranch(b string) bool {
	if b == "" || strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/") || strings.HasSuffix(b, "/") ||
		strings.HasSuffix(b, ".lock") || strings.HasSuffix(b, ".") || strings.Contains(b, "..") || strings.Contains(b, "@{") || strings.Contains(b, "//") {
		return false
	}
	for _, r := range b {
		if r <= ' ' || r == 0x7f || strings.ContainsRune("~^:?*[\\", r) {
			return false
		}
	}
	return true
}

func (s *Server) Down(_ context.Context, req *connect.Request[agentv2.DownRequest]) (*connect.Response[agentv2.DownResponse], error) {
	rev, err := s.b.Store().Down(req.Msg.Project, req.Msg.Purge)
	if err != nil {
		return nil, storeError(err)
	}
	return connect.NewResponse(&agentv2.DownResponse{Revision: rev}), nil
}

func (s *Server) Watch(ctx context.Context, req *connect.Request[agentv2.WatchRequest], stream *connect.ServerStream[agentv2.WatchResponse]) error {
	want := func(st *agentv2.EnvironmentStatus) bool {
		return req.Msg.EnvironmentId == "" || st.Id == req.Msg.EnvironmentId
	}
	// Subscribe before reading the current state so no change falls in between.
	ch, cancel := s.hub.Subscribe()
	defer cancel()
	for _, st := range s.b.Statuses() {
		if want(st) {
			if err := stream.Send(&agentv2.WatchResponse{Status: st}); err != nil {
				return err
			}
		}
	}
	if err := stream.Send(&agentv2.WatchResponse{Synced: true}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case st, ok := <-ch:
			if !ok {
				return connect.NewError(connect.CodeUnavailable, errors.New("agent is shutting down"))
			}
			if want(st) {
				if err := stream.Send(&agentv2.WatchResponse{Status: st}); err != nil {
					return err
				}
			}
		}
	}
}

// Hub fans status changes out to watchers. A watcher that falls behind is dropped
// rather than slowing the reconciler; the CLI reconnects and gets the current state.
type Hub struct {
	mu   sync.Mutex
	subs map[chan *agentv2.EnvironmentStatus]struct{}
}

func NewHub() *Hub { return &Hub{subs: map[chan *agentv2.EnvironmentStatus]struct{}{}} }

func (h *Hub) Subscribe() (<-chan *agentv2.EnvironmentStatus, func()) {
	ch := make(chan *agentv2.EnvironmentStatus, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

func (h *Hub) Publish(st *agentv2.EnvironmentStatus) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- st:
		default:
			delete(h.subs, ch)
			close(ch)
		}
	}
}

// Close ends every watch.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		delete(h.subs, ch)
		close(ch)
	}
}
