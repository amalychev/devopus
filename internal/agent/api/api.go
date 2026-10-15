// Package api serves the agent's local API (proto/devopus/agent/v1) to the CLI and
// the local panel.
package api

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"connectrpc.com/connect"

	agentv1 "github.com/devopus/devopus/gen/devopus/agent/v1"
	"github.com/devopus/devopus/gen/devopus/agent/v1/agentv1connect"
	"github.com/devopus/devopus/internal/agent/stack"
	"github.com/devopus/devopus/internal/agent/store"
	"github.com/devopus/devopus/internal/workspace"
)

// Backend is what the API needs from the running agent.
type Backend interface {
	Store() *store.Store
	Statuses() []*agentv1.EnvironmentStatus
	Runtime() *agentv1.RuntimeInfo
	Version() string
	Proxy() *agentv1.ProxyStatus
	Routes() []*agentv1.Route
	CA() *agentv1.CAInfo
	// Unsaved describes work in a feature's worktrees that exists nowhere else.
	Unsaved(ctx context.Context, env *agentv1.EnvironmentSpec, ws *workspace.Workspace) (map[string]string, error)
	// Force lets the removal of a feature drop its worktrees even with work in them.
	Force(envID string)
}

type Server struct {
	b   Backend
	hub *Hub
}

var _ agentv1connect.AgentServiceHandler = (*Server)(nil)

func New(b Backend, hub *Hub) *Server { return &Server{b: b, hub: hub} }

func (s *Server) Status(context.Context, *connect.Request[agentv1.StatusRequest]) (*connect.Response[agentv1.StatusResponse], error) {
	st := s.b.Store().Snapshot()
	projects := st.Projects
	for _, p := range projects {
		p.Config = nil // the CLI needs names and roots, not files
	}
	return connect.NewResponse(&agentv1.StatusResponse{
		AgentVersion:    s.b.Version(),
		DesiredRevision: st.Revision,
		Runtime:         s.b.Runtime(),
		Projects:        projects,
		Environments:    s.b.Statuses(),
		Proxy:           s.b.Proxy(),
		Ca:              s.b.CA(),
	}), nil
}

func (s *Server) Routes(_ context.Context, req *connect.Request[agentv1.RoutesRequest]) (*connect.Response[agentv1.RoutesResponse], error) {
	var out []*agentv1.Route
	for _, r := range s.b.Routes() {
		if req.Msg.EnvironmentId == "" || r.EnvironmentId == req.Msg.EnvironmentId {
			out = append(out, r)
		}
	}
	return connect.NewResponse(&agentv1.RoutesResponse{Routes: out, Proxy: s.b.Proxy()}), nil
}

func (s *Server) SetProxy(_ context.Context, req *connect.Request[agentv1.SetProxyRequest]) (*connect.Response[agentv1.SetProxyResponse], error) {
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
	return connect.NewResponse(&agentv1.SetProxyResponse{Revision: rev}), nil
}

func (s *Server) Up(_ context.Context, req *connect.Request[agentv1.UpRequest]) (*connect.Response[agentv1.UpResponse], error) {
	ws, err := parseWorkspace(req.Msg.WorkspacePath, req.Msg.Config)
	if err != nil {
		return nil, err
	}
	envID, rev, err := s.b.Store().Up(ws.Project, filepath.Dir(req.Msg.WorkspacePath), req.Msg.Config)
	if err != nil {
		return nil, storeError(err)
	}
	return connect.NewResponse(&agentv1.UpResponse{EnvironmentId: envID, Revision: rev}), nil
}

// parseWorkspace checks workspace.yaml: the agent does it itself, the CLI may be older
// or newer than the agent.
func parseWorkspace(path string, config []byte) (*workspace.Workspace, error) {
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
	return res.Workspace, nil
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

func (s *Server) CreateFeature(_ context.Context, req *connect.Request[agentv1.CreateFeatureRequest]) (*connect.Response[agentv1.CreateFeatureResponse], error) {
	m := req.Msg
	ws, err := parseWorkspace(m.WorkspacePath, m.Config)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(m.Name) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("a feature needs a name"))
	}
	if _, err := stack.Feature(ws, m.Modules); err != nil {
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
	f, base, rev, err := s.b.Store().CreateFeature(ws.Project, filepath.Dir(m.WorkspacePath), m.Config,
		store.NewFeature{Name: m.Name, Branch: branch, Modules: m.Modules, Reserved: reserved})
	if err != nil {
		return nil, storeError(err)
	}
	return connect.NewResponse(&agentv1.CreateFeatureResponse{EnvironmentId: f.Id, Slug: f.Slug, Branch: f.Branch, Revision: rev, BaseEnvironmentId: base.Id}), nil
}

func (s *Server) AddFeatureModules(_ context.Context, req *connect.Request[agentv1.AddFeatureModulesRequest]) (*connect.Response[agentv1.AddFeatureModulesResponse], error) {
	m := req.Msg
	ws, err := parseWorkspace(m.WorkspacePath, m.Config)
	if err != nil {
		return nil, err
	}
	if len(m.Modules) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name the modules to fork"))
	}
	if _, err := stack.Feature(ws, m.Modules); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	env, rev, err := s.b.Store().AddFeatureModules(ws.Project, filepath.Dir(m.WorkspacePath), m.Config, m.Feature, m.Modules)
	if err != nil {
		return nil, storeError(err)
	}
	return connect.NewResponse(&agentv1.AddFeatureModulesResponse{EnvironmentId: env.Id, Revision: rev}), nil
}

func (s *Server) RemoveFeature(ctx context.Context, req *connect.Request[agentv1.RemoveFeatureRequest]) (*connect.Response[agentv1.RemoveFeatureResponse], error) {
	m := req.Msg
	var ws *workspace.Workspace
	for _, p := range s.b.Store().Snapshot().Projects {
		if p.Name == m.Project {
			if res, err := workspace.Parse(p.Config); err == nil {
				ws = res.Workspace
			}
		}
	}
	env, rev, err := s.b.Store().RemoveFeature(m.Project, m.Feature, func(env *agentv1.EnvironmentSpec) error {
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
	return connect.NewResponse(&agentv1.RemoveFeatureResponse{EnvironmentId: env.Id, Revision: rev}), nil
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

func (s *Server) Down(_ context.Context, req *connect.Request[agentv1.DownRequest]) (*connect.Response[agentv1.DownResponse], error) {
	rev, err := s.b.Store().Down(req.Msg.Project, req.Msg.Purge)
	if err != nil {
		return nil, storeError(err)
	}
	return connect.NewResponse(&agentv1.DownResponse{Revision: rev}), nil
}

func (s *Server) Watch(ctx context.Context, req *connect.Request[agentv1.WatchRequest], stream *connect.ServerStream[agentv1.WatchResponse]) error {
	want := func(st *agentv1.EnvironmentStatus) bool {
		return req.Msg.EnvironmentId == "" || st.Id == req.Msg.EnvironmentId
	}
	// Subscribe before reading the current state so no change falls in between.
	ch, cancel := s.hub.Subscribe()
	defer cancel()
	for _, st := range s.b.Statuses() {
		if want(st) {
			if err := stream.Send(&agentv1.WatchResponse{Status: st}); err != nil {
				return err
			}
		}
	}
	if err := stream.Send(&agentv1.WatchResponse{Synced: true}); err != nil {
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
				if err := stream.Send(&agentv1.WatchResponse{Status: st}); err != nil {
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
	subs map[chan *agentv1.EnvironmentStatus]struct{}
}

func NewHub() *Hub { return &Hub{subs: map[chan *agentv1.EnvironmentStatus]struct{}{}} }

func (h *Hub) Subscribe() (<-chan *agentv1.EnvironmentStatus, func()) {
	ch := make(chan *agentv1.EnvironmentStatus, 64)
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

func (h *Hub) Publish(st *agentv1.EnvironmentStatus) {
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
