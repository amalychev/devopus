package server

import (
	"context"
	"regexp"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	workspacev1 "github.com/amalychev/devopus/gen/devopus/workspace/v1"
	"github.com/amalychev/devopus/internal/authz"
	"github.com/amalychev/devopus/internal/server/store"
)

func workspaceName(n string) (string, error) {
	n = strings.TrimSpace(n)
	if n == "" || utf8.RuneCountInString(n) > 64 {
		return "", invalid("a workspace name is 1 to 64 characters")
	}
	return n, nil
}

// projectNameRe: names go into URLs and domains later (pay-1.shop.localhost).
var projectNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

func (s *Service) workspaceInfo(ctx context.Context, q *store.Queries, w store.Workspace, role string) (*workspacev1.Workspace, error) {
	admin, err := q.WorkspaceAdmin(ctx, w.ID)
	if err != nil {
		return nil, err
	}
	return &workspacev1.Workspace{Id: w.ID, Name: w.Name, AdminEmail: admin.Email, CreatedAt: timestamppb.New(w.CreatedAt), Role: role}, nil
}

func (s *Service) CreateWorkspace(ctx context.Context, req *connect.Request[workspacev1.CreateWorkspaceRequest]) (*connect.Response[workspacev1.CreateWorkspaceResponse], error) {
	name, err := workspaceName(req.Msg.Name)
	if err != nil {
		return nil, err
	}
	c := callerOf(ctx)
	var out *workspacev1.Workspace
	err = s.tx(ctx, func(q *store.Queries) error {
		now := s.now()
		w, err := q.CreateWorkspace(ctx, store.CreateWorkspaceParams{ID: newID("wsp"), Name: name, CreatedAt: now})
		if err != nil {
			return err
		}
		if err := q.AddMember(ctx, store.AddMemberParams{WorkspaceID: w.ID, UserID: c.userID, Role: string(authz.Admin), JoinedAt: now}); err != nil {
			return err
		}
		if err := s.audit(ctx, q, w.ID, "workspace.created", name, ""); err != nil {
			return err
		}
		out, err = s.workspaceInfo(ctx, q, w, string(authz.Admin))
		return err
	})
	if err != nil {
		return nil, internal(err)
	}
	return connect.NewResponse(&workspacev1.CreateWorkspaceResponse{Workspace: out}), nil
}

func (s *Service) ListWorkspaces(ctx context.Context, _ *connect.Request[workspacev1.ListWorkspacesRequest]) (*connect.Response[workspacev1.ListWorkspacesResponse], error) {
	rows, err := s.q.UserWorkspaces(ctx, callerOf(ctx).userID)
	if err != nil {
		return nil, internal(err)
	}
	resp := &workspacev1.ListWorkspacesResponse{}
	for _, r := range rows {
		w, err := s.workspaceInfo(ctx, s.q, store.Workspace{ID: r.ID, Name: r.Name, CreatedAt: r.CreatedAt}, r.Role)
		if err != nil {
			return nil, internal(err)
		}
		resp.Workspaces = append(resp.Workspaces, w)
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) GetWorkspace(ctx context.Context, req *connect.Request[workspacev1.GetWorkspaceRequest]) (*connect.Response[workspacev1.GetWorkspaceResponse], error) {
	c := callerOf(ctx)
	w, err := s.q.Workspace(ctx, c.workspace)
	if err != nil {
		return nil, internal(err)
	}
	info, err := s.workspaceInfo(ctx, s.q, w, string(c.subject.Role))
	if err != nil {
		return nil, internal(err)
	}
	resp := &workspacev1.GetWorkspaceResponse{Workspace: info, Permissions: names(c.subject.In(""))}
	for _, p := range sortedKeys(c.subject.Projects) {
		resp.Projects = append(resp.Projects, &workspacev1.ProjectPermissions{Project: p, Permissions: names(c.subject.In(p))})
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) RenameWorkspace(ctx context.Context, req *connect.Request[workspacev1.RenameWorkspaceRequest]) (*connect.Response[workspacev1.RenameWorkspaceResponse], error) {
	name, err := workspaceName(req.Msg.Name)
	if err != nil {
		return nil, err
	}
	c := callerOf(ctx)
	var out *workspacev1.Workspace
	err = s.tx(ctx, func(q *store.Queries) error {
		w, err := q.RenameWorkspace(ctx, store.RenameWorkspaceParams{ID: c.workspace, Name: name})
		if err != nil {
			return err
		}
		if err := s.audit(ctx, q, c.workspace, "workspace.renamed", name, ""); err != nil {
			return err
		}
		out, err = s.workspaceInfo(ctx, q, w, string(c.subject.Role))
		return err
	})
	if err != nil {
		return nil, internal(err)
	}
	return connect.NewResponse(&workspacev1.RenameWorkspaceResponse{Workspace: out}), nil
}

// memberByEmail finds a member of the caller's workspace.
func (s *Service) memberByEmail(ctx context.Context, q *store.Queries, email string) (store.Member, error) {
	email = normalizeEmail(email)
	if email == "" {
		return store.Member{}, invalid("the email is missing")
	}
	u, err := q.UserByEmail(ctx, email)
	if noRows(err) {
		return store.Member{}, notFound("%s is not a member of this workspace", email)
	}
	if err != nil {
		return store.Member{}, internal(err)
	}
	m, err := q.Member(ctx, store.MemberParams{WorkspaceID: callerOf(ctx).workspace, UserID: u.ID})
	if noRows(err) {
		return m, notFound("%s is not a member of this workspace", email)
	}
	if err != nil {
		return m, internal(err)
	}
	return m, nil
}

func (s *Service) TransferWorkspace(ctx context.Context, req *connect.Request[workspacev1.TransferWorkspaceRequest]) (*connect.Response[workspacev1.TransferWorkspaceResponse], error) {
	c := callerOf(ctx)
	err := s.tx(ctx, func(q *store.Queries) error {
		m, err := s.memberByEmail(ctx, q, req.Msg.Email)
		if err != nil {
			return err
		}
		if m.UserID == c.userID {
			return invalid("you are the admin already")
		}
		// One admin per workspace: step down first.
		if err := q.SetMemberRole(ctx, store.SetMemberRoleParams{WorkspaceID: c.workspace, UserID: c.userID, Role: string(authz.Teamlead)}); err != nil {
			return internal(err)
		}
		if err := q.SetMemberRole(ctx, store.SetMemberRoleParams{WorkspaceID: c.workspace, UserID: m.UserID, Role: string(authz.Admin)}); err != nil {
			return internal(err)
		}
		return s.audit(ctx, q, c.workspace, "workspace.transferred", normalizeEmail(req.Msg.Email), "")
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.TransferWorkspaceResponse{}), nil
}

func (s *Service) DeleteWorkspace(ctx context.Context, req *connect.Request[workspacev1.DeleteWorkspaceRequest]) (*connect.Response[workspacev1.DeleteWorkspaceResponse], error) {
	c := callerOf(ctx)
	w, err := s.q.Workspace(ctx, c.workspace)
	if err != nil {
		return nil, internal(err)
	}
	if req.Msg.ConfirmName != w.Name {
		return nil, invalid("to delete the workspace, repeat its name %q", w.Name)
	}
	if err := s.q.DeleteWorkspace(ctx, w.ID); err != nil {
		return nil, internal(err)
	}
	s.cfg.Log.Info("workspace deleted", "workspace", w.ID, "name", w.Name, "by", c.email)
	return connect.NewResponse(&workspacev1.DeleteWorkspaceResponse{}), nil
}

func (s *Service) ListAudit(ctx context.Context, req *connect.Request[workspacev1.ListAuditRequest]) (*connect.Response[workspacev1.ListAuditResponse], error) {
	limit := req.Msg.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.q.Audit(ctx, store.AuditParams{WorkspaceID: callerOf(ctx).workspace, Limit: limit})
	if err != nil {
		return nil, internal(err)
	}
	resp := &workspacev1.ListAuditResponse{}
	for _, r := range rows {
		resp.Events = append(resp.Events, &workspacev1.AuditEvent{Actor: r.Actor, Action: r.Action, Target: r.Target, Detail: r.Detail, At: timestamppb.New(r.CreatedAt)})
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) CreateProject(ctx context.Context, req *connect.Request[workspacev1.CreateProjectRequest]) (*connect.Response[workspacev1.CreateProjectResponse], error) {
	name := strings.TrimSpace(req.Msg.Name)
	if !projectNameRe.MatchString(name) {
		return nil, invalid("project name %q: lowercase letters, digits and dashes, starting with a letter, up to 40", name)
	}
	c := callerOf(ctx)
	var p store.Project
	role := ""
	err := s.tx(ctx, func(q *store.Queries) error {
		_, err := q.ProjectByName(ctx, store.ProjectByNameParams{WorkspaceID: c.workspace, Name: name})
		if err == nil {
			return connect.NewError(connect.CodeAlreadyExists, errorf("project %q exists", name))
		}
		if !noRows(err) {
			return internal(err)
		}
		if p, err = q.CreateProject(ctx, store.CreateProjectParams{ID: newID("prj"), WorkspaceID: c.workspace, Name: name, CreatedBy: c.userID, CreatedAt: s.now()}); err != nil {
			return internal(err)
		}
		// A teamlead who creates a project leads it; the admin has every project anyway.
		if c.subject.Role != authz.Admin {
			role = string(authz.Teamlead)
			if err := q.SetProjectRole(ctx, store.SetProjectRoleParams{WorkspaceID: c.workspace, ProjectID: p.ID, UserID: c.userID, Role: role}); err != nil {
				return internal(err)
			}
		}
		return s.audit(ctx, q, c.workspace, "project.created", name, "")
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.CreateProjectResponse{Project: &workspacev1.Project{Id: p.ID, Name: p.Name, CreatedAt: timestamppb.New(p.CreatedAt), Role: role}}), nil
}

// ListProjects shows the admin every project and others the projects they have a role in.
func (s *Service) ListProjects(ctx context.Context, _ *connect.Request[workspacev1.ListProjectsRequest]) (*connect.Response[workspacev1.ListProjectsResponse], error) {
	c := callerOf(ctx)
	rows, err := s.q.Projects(ctx, c.workspace)
	if err != nil {
		return nil, internal(err)
	}
	resp := &workspacev1.ListProjectsResponse{}
	for _, p := range rows {
		role, ok := c.subject.Projects[p.Name]
		if !ok && c.subject.Role != authz.Admin {
			continue
		}
		resp.Projects = append(resp.Projects, &workspacev1.Project{Id: p.ID, Name: p.Name, CreatedAt: timestamppb.New(p.CreatedAt), Role: string(role)})
	}
	return connect.NewResponse(resp), nil
}

func names(ps []authz.Permission) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = string(p)
	}
	return out
}
