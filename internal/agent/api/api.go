// Package api serves the agent's local API (proto/devopus/agent/v1) to the CLI and
// the local panel.
package api

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"connectrpc.com/connect"

	agentv1 "github.com/devopus/devopus/gen/devopus/agent/v1"
	"github.com/devopus/devopus/gen/devopus/agent/v1/agentv1connect"
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
	path := req.Msg.WorkspacePath
	if !filepath.IsAbs(path) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("workspace_path must be absolute, got %q", path))
	}
	// The agent checks the file itself: the CLI may be older or newer than the agent.
	res, err := workspace.Parse(req.Msg.Config)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s: %w", path, err))
	}
	if res.Diagnostics.HasErrors() {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s is invalid:\n%s", path, res.Diagnostics.Errors().Error()))
	}
	envID, rev, err := s.b.Store().Up(res.Workspace.Project, filepath.Dir(path), req.Msg.Config)
	if errors.Is(err, store.ErrProjectConflict) {
		return nil, connect.NewError(connect.CodeAlreadyExists, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentv1.UpResponse{EnvironmentId: envID, Revision: rev}), nil
}

func (s *Server) Down(_ context.Context, req *connect.Request[agentv1.DownRequest]) (*connect.Response[agentv1.DownResponse], error) {
	rev, err := s.b.Store().Down(req.Msg.Project, req.Msg.Purge)
	if errors.Is(err, store.ErrUnknownProject) {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
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
