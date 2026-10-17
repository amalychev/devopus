package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	workspacev1 "github.com/amalychev/devopus/gen/devopus/workspace/v1"
	"github.com/amalychev/devopus/internal/authz"
	"github.com/amalychev/devopus/internal/server/store"
	"github.com/amalychev/devopus/internal/spec"
)

// Servers of the admin (R13, ADR-0019). A server is a member of its own kind — a user
// row srv_… with the role "server" in the workspace and in the projects it runs — so
// devices, device keys, key delivery and statuses work for it as for people. It
// joins once with a one-time code and then signs in with a device token.

const (
	serverTokenPrefix = "dvs_"
	joinCodePrefix    = "dvj_"
	// joinTTL is how long a join code works.
	joinTTL = time.Hour
	// serverSession is how long a server's stream lives before it reconnects and the
	// token is checked again.
	serverSession = time.Hour
)

// newSecret is a credential and the hash the server keeps of it.
func newSecret(prefix string) (string, string) {
	v := secret(prefix)
	return v, hashOf(v)
}

func sortedSet(list []string) []string {
	out := slices.Clone(list)
	slices.Sort(out)
	return slices.Compact(out)
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// authenticateServer signs a server in with its device token.
func (s *Service) authenticateServer(ctx context.Context, raw string) (*caller, error) {
	sv, err := s.q.ServerByToken(ctx, &[]string{hashOf(raw)}[0])
	if noRows(err) {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("the server's token is not valid: it was removed from the workspace, or join it again"))
	}
	if err != nil {
		return nil, internal(err)
	}
	return &caller{userID: sv.ID, email: serverEmail(sv.Name), expires: s.now().Add(serverSession), server: sv.WorkspaceID}, nil
}

// serverEmail is how a server shows where people show their email.
func serverEmail(name string) string { return name + " (server)" }

// canManageServer: servers.manage in every project the server runs or is to run; the
// admin for a server of no project.
func canManageServer(sub authz.Subject, projects []string) error {
	if sub.Role == authz.Admin {
		return nil
	}
	if len(projects) == 0 {
		return denied("a server of no project is the admin's; name its projects with --project")
	}
	for _, p := range projects {
		if !sub.Can(authz.ServersManage, p) {
			return denied("this needs the permission %s in project %q", authz.ServersManage, p)
		}
	}
	return nil
}

func (s *Service) CreateServer(ctx context.Context, req *connect.Request[workspacev1.CreateServerRequest]) (*connect.Response[workspacev1.CreateServerResponse], error) {
	c, m := callerOf(ctx), req.Msg
	if !spec.NameRe.MatchString(m.Name) {
		return nil, invalid("server name %q: lowercase letters, digits and dashes", m.Name)
	}
	projects := sortedSet(m.Projects)
	if err := canManageServer(c.subject, projects); err != nil {
		return nil, err
	}
	code, codeHash := newSecret(joinCodePrefix)
	id := newID("srv")
	now := s.now()
	err := s.tx(ctx, func(q *store.Queries) error {
		if _, err := q.ServerByName(ctx, store.ServerByNameParams{WorkspaceID: c.workspace, Name: m.Name}); err == nil {
			return connect.NewError(connect.CodeAlreadyExists, errorf("the workspace has a server %q already", m.Name))
		} else if !noRows(err) {
			return internal(err)
		}
		if err := q.UpsertUser(ctx, store.UpsertUserParams{ID: id, Email: serverEmail(m.Name), SeenAt: now}); err != nil {
			return internal(err)
		}
		if err := q.AddMember(ctx, store.AddMemberParams{WorkspaceID: c.workspace, UserID: id, Role: string(authz.Server), JoinedAt: now}); err != nil {
			return internal(err)
		}
		if err := s.setServerProjects(ctx, q, c.workspace, id, projects); err != nil {
			return err
		}
		if err := q.CreateServer(ctx, store.CreateServerParams{WorkspaceID: c.workspace, ID: id, Name: m.Name, CreatedBy: c.userID, CreatedAt: now,
			JoinHash: &codeHash, JoinExpires: ptr(now.Add(joinTTL))}); err != nil {
			return internal(err)
		}
		return s.audit(ctx, q, c.workspace, "server.created", m.Name, strings.Join(projects, ", "))
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.CreateServerResponse{
		Server:   &workspacev1.ServerInfo{Id: id, Name: m.Name, Projects: projects, CreatedAt: timestamppb.New(now)},
		JoinCode: code, ExpiresAt: timestamppb.New(now.Add(joinTTL)),
	}), nil
}

func (s *Service) setServerProjects(ctx context.Context, q *store.Queries, ws, id string, projects []string) error {
	if err := q.DeleteProjectMembersOf(ctx, store.DeleteProjectMembersOfParams{WorkspaceID: ws, UserID: id}); err != nil {
		return internal(err)
	}
	for _, name := range projects {
		p, err := q.ProjectByName(ctx, store.ProjectByNameParams{WorkspaceID: ws, Name: name})
		if noRows(err) {
			return notFound("no project %q in this workspace", name)
		}
		if err != nil {
			return internal(err)
		}
		if err := q.SetProjectRole(ctx, store.SetProjectRoleParams{WorkspaceID: ws, ProjectID: p.ID, UserID: id, Role: string(authz.Server)}); err != nil {
			return internal(err)
		}
	}
	return nil
}

func (s *Service) JoinServer(ctx context.Context, req *connect.Request[workspacev1.JoinServerRequest]) (*connect.Response[workspacev1.JoinServerResponse], error) {
	m := req.Msg
	token, tokenHash := newSecret(serverTokenPrefix)
	var resp *workspacev1.JoinServerResponse
	err := s.tx(ctx, func(q *store.Queries) error {
		h := hashOf(m.Code)
		sv, err := q.ServerByJoin(ctx, &h)
		if noRows(err) {
			return connect.NewError(connect.CodeNotFound, errors.New("the join code is not valid: it was used already, or mistyped; 'devopus servers add' gives a new one"))
		}
		if err != nil {
			return internal(err)
		}
		if sv.JoinExpires == nil || s.now().After(*sv.JoinExpires) {
			return connect.NewError(connect.CodeDeadlineExceeded, errors.New("the join code expired: 'devopus servers join-code <name>' gives a new one"))
		}
		if err := q.JoinServer(ctx, store.JoinServerParams{WorkspaceID: sv.WorkspaceID, ID: sv.ID, TokenHash: &tokenHash, JoinedAt: ptr(s.now())}); err != nil {
			return internal(err)
		}
		w, err := q.Workspace(ctx, sv.WorkspaceID)
		if err != nil {
			return internal(err)
		}
		resp = &workspacev1.JoinServerResponse{WorkspaceId: w.ID, WorkspaceName: w.Name, ServerId: sv.ID, Name: sv.Name, Token: token}
		return q.AddAudit(ctx, store.AddAuditParams{WorkspaceID: w.ID, ActorID: sv.ID, Action: "server.joined", Target: sv.Name, Detail: m.DeviceName, CreatedAt: s.now()})
	})
	if err != nil {
		return nil, err
	}
	s.hub.wake(resp.WorkspaceId)
	return connect.NewResponse(resp), nil
}

func (s *Service) ListServers(ctx context.Context, req *connect.Request[workspacev1.ListServersRequest]) (*connect.Response[workspacev1.ListServersResponse], error) {
	c := callerOf(ctx)
	rows, err := s.q.Servers(ctx, c.workspace)
	if err != nil {
		return nil, internal(err)
	}
	resp := &workspacev1.ListServersResponse{}
	for _, r := range rows {
		// The admin sees all; others the servers of projects they are in.
		if c.subject.Role != authz.Admin && !slices.ContainsFunc(r.Projects, func(p string) bool { return c.subject.Can(authz.WorkspaceView, p) }) {
			continue
		}
		resp.Servers = append(resp.Servers, serverInfo(r))
	}
	return connect.NewResponse(resp), nil
}

func serverInfo(r store.ServersRow) *workspacev1.ServerInfo {
	info := &workspacev1.ServerInfo{Id: r.ID, Name: r.Name, Projects: r.Projects, Joined: r.JoinedAt != nil, CreatedAt: timestamppb.New(r.CreatedAt)}
	if r.LastSeen != nil {
		info.LastSeen = timestamppb.New(*r.LastSeen)
	}
	if r.AgentVersion != nil {
		info.AgentVersion = *r.AgentVersion
	}
	return info
}

func (s *Service) serverNamed(ctx context.Context, q *store.Queries, c *caller, name string) (store.ServersRow, error) {
	rows, err := q.Servers(ctx, c.workspace)
	if err != nil {
		return store.ServersRow{}, internal(err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r, nil
		}
	}
	return store.ServersRow{}, notFound("no server %q in this workspace: 'devopus servers list'", name)
}

func (s *Service) SetServerProjects(ctx context.Context, req *connect.Request[workspacev1.SetServerProjectsRequest]) (*connect.Response[workspacev1.SetServerProjectsResponse], error) {
	c, m := callerOf(ctx), req.Msg
	projects := sortedSet(m.Projects)
	var info *workspacev1.ServerInfo
	err := s.tx(ctx, func(q *store.Queries) error {
		r, err := s.serverNamed(ctx, q, c, m.Name)
		if err != nil {
			return err
		}
		// Both what it ran and what it is to run must be the caller's to manage.
		if err := canManageServer(c.subject, sortedSet(append(append([]string{}, r.Projects...), projects...))); err != nil {
			return err
		}
		if err := s.setServerProjects(ctx, q, c.workspace, r.ID, projects); err != nil {
			return err
		}
		r.Projects = projects
		info = serverInfo(r)
		return s.audit(ctx, q, c.workspace, "server.projects", m.Name, strings.Join(projects, ", "))
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.SetServerProjectsResponse{Server: info}), nil
}

func (s *Service) RemoveServer(ctx context.Context, req *connect.Request[workspacev1.RemoveServerRequest]) (*connect.Response[workspacev1.RemoveServerResponse], error) {
	c, m := callerOf(ctx), req.Msg
	err := s.tx(ctx, func(q *store.Queries) error {
		r, err := s.serverNamed(ctx, q, c, m.Name)
		if err != nil {
			return err
		}
		// Whoever added it may remove it; others need servers.manage in its projects.
		if r.CreatedBy != c.userID {
			if err := canManageServer(c.subject, r.Projects); err != nil {
				return err
			}
		}
		// The membership goes, and with it the server row, its devices and its keys.
		if err := q.RemoveMember(ctx, store.RemoveMemberParams{WorkspaceID: c.workspace, UserID: r.ID}); err != nil {
			return internal(err)
		}
		return s.audit(ctx, q, c.workspace, "server.removed", m.Name, "")
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.RemoveServerResponse{}), nil
}

func ptr[T any](v T) *T { return &v }
