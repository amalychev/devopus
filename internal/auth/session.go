package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	authv1 "github.com/amalychev/devopus/gen/devopus/auth/v1"
	"github.com/amalychev/devopus/internal/auth/store"
	"github.com/amalychev/devopus/internal/auth/token"
)

var (
	errSignedOut = errors.New("the session ended; sign in again with 'devopus login'")
	// errReused tells both the holder of a copied token and the device it was copied
	// from why the session ended.
	errReused = errors.New("this sign-in was used from somewhere else, so the session is closed for safety; sign in again with 'devopus login'")
)

const reasonReused = "refresh token reused"

// live reports whether a session can be used at now.
func live(ses store.Session, now time.Time) bool {
	return ses.RevokedAt == nil && now.Before(ses.IdleExpiresAt) && now.Before(ses.ExpiresAt)
}

func (s *Service) Refresh(ctx context.Context, req *connect.Request[authv1.RefreshRequest]) (*connect.Response[authv1.RefreshResponse], error) {
	ip := s.clientIP(req.Peer().Addr, req.Header())
	var (
		tokens *authv1.Tokens
		user   store.User
	)
	err := s.tx(ctx, func(q *store.Queries) error {
		rt, err := q.RefreshTokenForUpdate(ctx, hash(req.Msg.RefreshToken))
		if noRows(err) {
			return connect.NewError(connect.CodeUnauthenticated, errSignedOut)
		}
		if err != nil {
			return internal(err)
		}
		ses, err := q.SessionForUpdate(ctx, rt.SessionID)
		if err != nil {
			return internal(err)
		}
		now := s.now()
		if rt.UsedAt != nil {
			// The token was refreshed before: someone else has a copy. End the session for
			// everyone who holds it.
			if ses.RevokedAt == nil {
				reason := reasonReused
				if err := q.RevokeSession(ctx, store.RevokeSessionParams{ID: ses.ID, RevokedAt: &now, RevokeReason: &reason}); err != nil {
					return internal(err)
				}
				s.event(ctx, q, ses.UserID, ses.ID, "refresh_token_reused", ip, "")
				s.cfg.Log.Warn("refresh token reused: session revoked", "session", ses.ID, "user", ses.UserID, "ip", ip)
			}
			return commit(connect.NewError(connect.CodeUnauthenticated, errReused))
		}
		if !live(ses, now) {
			if ses.RevokeReason != nil && *ses.RevokeReason == reasonReused {
				return connect.NewError(connect.CodeUnauthenticated, errReused)
			}
			return connect.NewError(connect.CodeUnauthenticated, errSignedOut)
		}
		if err := q.UseRefreshToken(ctx, store.UseRefreshTokenParams{TokenHash: rt.TokenHash, UsedAt: &now}); err != nil {
			return internal(err)
		}
		idle := now.Add(SessionIdle)
		if idle.After(ses.ExpiresAt) {
			idle = ses.ExpiresAt
		}
		if ses, err = q.TouchSession(ctx, store.TouchSessionParams{ID: ses.ID, LastUsedAt: now, IdleExpiresAt: idle}); err != nil {
			return internal(err)
		}
		if user, err = q.UserByID(ctx, ses.UserID); err != nil {
			return internal(err)
		}
		tokens, err = s.issue(ctx, q, ses, req.Msg.Audience)
		return err
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&authv1.RefreshResponse{User: userInfo(user), Tokens: tokens}), nil
}

func (s *Service) SignOut(ctx context.Context, req *connect.Request[authv1.SignOutRequest]) (*connect.Response[authv1.SignOutResponse], error) {
	ip := s.clientIP(req.Peer().Addr, req.Header())
	err := s.tx(ctx, func(q *store.Queries) error {
		rt, err := q.RefreshTokenForUpdate(ctx, hash(req.Msg.RefreshToken))
		if noRows(err) {
			return nil // nothing to sign out of
		}
		if err != nil {
			return internal(err)
		}
		ses, err := q.SessionByID(ctx, rt.SessionID)
		if err != nil {
			return internal(err)
		}
		return s.revoke(ctx, q, ses, "signed out", "signed_out", ip)
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&authv1.SignOutResponse{}), nil
}

func (s *Service) revoke(ctx context.Context, q *store.Queries, ses store.Session, reason, kind, ip string) error {
	if ses.RevokedAt != nil {
		return nil
	}
	now := s.now()
	if err := q.RevokeSession(ctx, store.RevokeSessionParams{ID: ses.ID, RevokedAt: &now, RevokeReason: &reason}); err != nil {
		return internal(err)
	}
	s.event(ctx, q, ses.UserID, ses.ID, kind, ip, "")
	return nil
}

// caller is the signed-in user of a request.
type caller struct {
	user    store.User
	session store.Session
	ip      string
}

// authenticate checks the request's access token and that its session is still live:
// Auth's own calls see a revoked session at once, not when the token expires.
func (s *Service) authenticate(ctx context.Context, peer string, h http.Header) (caller, error) {
	raw, ok := strings.CutPrefix(h.Get("Authorization"), "Bearer ")
	if !ok {
		return caller{}, connect.NewError(connect.CodeUnauthenticated, errors.New("sign in first: devopus login"))
	}
	claims, err := s.Verify(raw)
	if err != nil {
		if errors.Is(err, token.ErrExpired) {
			return caller{}, connect.NewError(connect.CodeUnauthenticated, errors.New("the access token expired; refresh it"))
		}
		return caller{}, connect.NewError(connect.CodeUnauthenticated, err)
	}
	ses, err := s.q.SessionByID(ctx, claims.Session)
	if noRows(err) || (err == nil && (!live(ses, s.now()) || ses.UserID != claims.Subject)) {
		return caller{}, connect.NewError(connect.CodeUnauthenticated, errSignedOut)
	}
	if err != nil {
		return caller{}, internal(err)
	}
	user, err := s.q.UserByID(ctx, ses.UserID)
	if err != nil {
		return caller{}, internal(err)
	}
	return caller{user: user, session: ses, ip: s.clientIP(peer, h)}, nil
}

func (s *Service) GetMe(ctx context.Context, req *connect.Request[authv1.GetMeRequest]) (*connect.Response[authv1.GetMeResponse], error) {
	c, err := s.authenticate(ctx, req.Peer().Addr, req.Header())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&authv1.GetMeResponse{User: userInfo(c.user), Session: sessionInfo(c.session, c.session.ID)}), nil
}

func (s *Service) ListSessions(ctx context.Context, req *connect.Request[authv1.ListSessionsRequest]) (*connect.Response[authv1.ListSessionsResponse], error) {
	c, err := s.authenticate(ctx, req.Peer().Addr, req.Header())
	if err != nil {
		return nil, err
	}
	list, err := s.q.LiveSessions(ctx, store.LiveSessionsParams{UserID: c.user.ID, IdleExpiresAt: s.now()})
	if err != nil {
		return nil, internal(err)
	}
	resp := &authv1.ListSessionsResponse{}
	for _, ses := range list {
		resp.Sessions = append(resp.Sessions, sessionInfo(ses, c.session.ID))
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) RevokeSession(ctx context.Context, req *connect.Request[authv1.RevokeSessionRequest]) (*connect.Response[authv1.RevokeSessionResponse], error) {
	c, err := s.authenticate(ctx, req.Peer().Addr, req.Header())
	if err != nil {
		return nil, err
	}
	err = s.tx(ctx, func(q *store.Queries) error {
		ses, err := q.SessionForUpdate(ctx, req.Msg.SessionId)
		// Another user's session is "not found" too: ids are not to be probed.
		if noRows(err) || (err == nil && ses.UserID != c.user.ID) || (err == nil && !live(ses, s.now())) {
			return connect.NewError(connect.CodeNotFound, fmt.Errorf("no session %q", req.Msg.SessionId))
		}
		if err != nil {
			return internal(err)
		}
		return s.revoke(ctx, q, ses, "revoked by the user", "session_revoked", c.ip)
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&authv1.RevokeSessionResponse{}), nil
}

func (s *Service) ListWorkspaces(ctx context.Context, req *connect.Request[authv1.ListWorkspacesRequest]) (*connect.Response[authv1.ListWorkspacesResponse], error) {
	c, err := s.authenticate(ctx, req.Peer().Addr, req.Header())
	if err != nil {
		return nil, err
	}
	list, err := s.q.UserWorkspaces(ctx, c.user.ID)
	if err != nil {
		return nil, internal(err)
	}
	resp := &authv1.ListWorkspacesResponse{}
	for _, w := range list {
		resp.Workspaces = append(resp.Workspaces, workspaceInfo(w))
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) RegisterWorkspace(ctx context.Context, req *connect.Request[authv1.RegisterWorkspaceRequest]) (*connect.Response[authv1.RegisterWorkspaceResponse], error) {
	c, err := s.authenticate(ctx, req.Peer().Addr, req.Header())
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(req.Msg.Name)
	if name == "" || len(name) > 64 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("a workspace name is 1 to 64 characters"))
	}
	u, err := url.Parse(req.Msg.ServerUrl)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("server URL %q: want https://host", req.Msg.ServerUrl))
	}
	var w store.Workspace
	err = s.tx(ctx, func(q *store.Queries) error {
		now := s.now()
		w, err = q.CreateWorkspace(ctx, store.CreateWorkspaceParams{ID: newID("wsp"), Name: name, ServerUrl: u.String(), CreatedBy: c.user.ID, CreatedAt: now})
		if err != nil {
			return internal(err)
		}
		if err := q.AddWorkspaceMember(ctx, store.AddWorkspaceMemberParams{WorkspaceID: w.ID, UserID: c.user.ID, AddedAt: now}); err != nil {
			return internal(err)
		}
		s.event(ctx, q, c.user.ID, c.session.ID, "workspace_registered", c.ip, w.ID)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&authv1.RegisterWorkspaceResponse{Workspace: workspaceInfo(w)}), nil
}

func workspaceInfo(w store.Workspace) *authv1.Workspace {
	return &authv1.Workspace{Id: w.ID, Name: w.Name, ServerUrl: w.ServerUrl, CreatedAt: timestamppb.New(w.CreatedAt)}
}
