package server

import (
	"context"
	"fmt"
	"net/mail"
	"sort"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	workspacev1 "github.com/amalychev/devopus/gen/devopus/workspace/v1"
	mailer "github.com/amalychev/devopus/internal/auth/mail"
	"github.com/amalychev/devopus/internal/authz"
	"github.com/amalychev/devopus/internal/server/store"
)

// projectIDs resolves project names of the caller's workspace to ids.
func (s *Service) projectIDs(ctx context.Context, q *store.Queries, names []string) ([]string, map[string]string, error) {
	all, err := q.Projects(ctx, callerOf(ctx).workspace)
	if err != nil {
		return nil, nil, internal(err)
	}
	byName, byID := map[string]string{}, map[string]string{}
	for _, p := range all {
		byName[p.Name], byID[p.ID] = p.ID, p.Name
	}
	var ids []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		id, ok := byName[n]
		if !ok {
			return nil, nil, notFound("no project %q in this workspace", n)
		}
		ids = append(ids, id)
	}
	return ids, byID, nil
}

func inviteInfo(i store.Invite, invitedBy string, projects map[string]string) *workspacev1.Invite {
	out := &workspacev1.Invite{Id: i.ID, Email: i.Email, Role: i.Role, InvitedBy: invitedBy,
		CreatedAt: timestamppb.New(i.CreatedAt), ExpiresAt: timestamppb.New(i.ExpiresAt)}
	for _, id := range i.ProjectIds {
		out.Projects = append(out.Projects, projects[id])
	}
	return out
}

func (s *Service) CreateInvite(ctx context.Context, req *connect.Request[workspacev1.CreateInviteRequest]) (*connect.Response[workspacev1.CreateInviteResponse], error) {
	m := req.Msg
	email := normalizeEmail(m.Email)
	if a, err := mail.ParseAddress(email); err != nil || a.Address != email {
		return nil, invalid("%q is not an email address", m.Email)
	}
	role, err := authz.ParseRole(m.Role)
	if err != nil {
		return nil, invalid("%v", err)
	}
	c := callerOf(ctx)
	if err := c.subject.CanInvite(role, m.Projects); err != nil {
		return nil, denied("%v", err)
	}
	if role == authz.Developer && len(m.Projects) == 0 {
		return nil, invalid("a developer works in projects: name them with --project")
	}
	code := secret("dvi_")
	var (
		inv      store.Invite
		projects map[string]string
		wsName   string
	)
	err = s.tx(ctx, func(q *store.Queries) error {
		ids, byID, err := s.projectIDs(ctx, q, m.Projects)
		if err != nil {
			return err
		}
		projects = byID
		w, err := q.Workspace(ctx, c.workspace)
		if err != nil {
			return internal(err)
		}
		wsName = w.Name
		now := s.now()
		inv, err = q.CreateInvite(ctx, store.CreateInviteParams{
			ID: newID("inv"), WorkspaceID: c.workspace, CodeHash: hash(code), Email: email, Role: string(role),
			ProjectIds: append([]string{}, ids...), CreatedBy: c.userID, CreatedAt: now, ExpiresAt: now.Add(InviteTTL),
		})
		if err != nil {
			return internal(err)
		}
		return s.audit(ctx, q, c.workspace, "member.invited", email, fmt.Sprintf("%s %s", role, strings.Join(m.Projects, ",")))
	})
	if err != nil {
		return nil, err
	}
	where := ""
	if len(m.Projects) > 0 {
		where = " in " + strings.Join(m.Projects, ", ")
	}
	if err := s.cfg.Mailer.Send(ctx, mailer.Message{
		To:      email,
		Subject: fmt.Sprintf("%s invited you to %s on Devopus", c.email, wsName),
		Text: fmt.Sprintf("%s invited you to the workspace %s as %s%s.\n\nTo join, install Devopus, sign in with this email and run:\n\n  devopus invite accept %s\n\nThe invite works for %d days.\n",
			c.email, wsName, role, where, code, int(InviteTTL.Hours()/24)),
	}); err != nil {
		// The invite stands: the inviter has the code and can pass it on.
		s.cfg.Log.Error("send invite", "to", email, "err", err)
	}
	return connect.NewResponse(&workspacev1.CreateInviteResponse{Invite: inviteInfo(inv, c.email, projects), Code: code}), nil
}

func (s *Service) ListInvites(ctx context.Context, _ *connect.Request[workspacev1.ListInvitesRequest]) (*connect.Response[workspacev1.ListInvitesResponse], error) {
	c := callerOf(ctx)
	rows, err := s.q.OpenInvites(ctx, store.OpenInvitesParams{WorkspaceID: c.workspace, ExpiresAt: s.now()})
	if err != nil {
		return nil, internal(err)
	}
	_, byID, err := s.projectIDs(ctx, s.q, nil)
	if err != nil {
		return nil, err
	}
	resp := &workspacev1.ListInvitesResponse{}
	for _, r := range rows {
		// A teamlead sees the invites they made.
		if c.subject.Role != authz.Admin && r.CreatedBy != c.userID {
			continue
		}
		i := store.Invite{ID: r.ID, Email: r.Email, Role: r.Role, ProjectIds: r.ProjectIds, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt}
		resp.Invites = append(resp.Invites, inviteInfo(i, r.InvitedBy, byID))
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) RevokeInvite(ctx context.Context, req *connect.Request[workspacev1.RevokeInviteRequest]) (*connect.Response[workspacev1.RevokeInviteResponse], error) {
	c := callerOf(ctx)
	err := s.tx(ctx, func(q *store.Queries) error {
		inv, err := q.Invite(ctx, store.InviteParams{WorkspaceID: c.workspace, ID: req.Msg.InviteId})
		if noRows(err) || (err == nil && (inv.AcceptedAt != nil || inv.RevokedAt != nil)) {
			return notFound("no open invite %q", req.Msg.InviteId)
		}
		if err != nil {
			return internal(err)
		}
		if c.subject.Role != authz.Admin && inv.CreatedBy != c.userID {
			return denied("only the admin revokes invites made by others")
		}
		now := s.now()
		if err := q.RevokeInvite(ctx, store.RevokeInviteParams{WorkspaceID: c.workspace, ID: inv.ID, RevokedAt: &now}); err != nil {
			return internal(err)
		}
		return s.audit(ctx, q, c.workspace, "invite.revoked", inv.Email, "")
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.RevokeInviteResponse{}), nil
}

func (s *Service) AcceptInvite(ctx context.Context, req *connect.Request[workspacev1.AcceptInviteRequest]) (*connect.Response[workspacev1.AcceptInviteResponse], error) {
	c := callerOf(ctx)
	var out *workspacev1.Workspace
	err := s.tx(ctx, func(q *store.Queries) error {
		inv, err := q.InviteByCodeForUpdate(ctx, hash(strings.TrimSpace(req.Msg.Code)))
		now := s.now()
		switch {
		case noRows(err):
			return notFound("no such invite; check the code")
		case err != nil:
			return internal(err)
		case inv.AcceptedAt != nil:
			return connect.NewError(connect.CodeFailedPrecondition, errorf("this invite was already accepted"))
		case inv.RevokedAt != nil:
			return connect.NewError(connect.CodeFailedPrecondition, errorf("this invite was revoked; ask for a new one"))
		case !now.Before(inv.ExpiresAt):
			return connect.NewError(connect.CodeFailedPrecondition, errorf("this invite expired; ask for a new one"))
		case inv.Email != c.email:
			return denied("this invite is for %s, and you are signed in as %s", inv.Email, c.email)
		}
		ctx := context.WithValue(ctx, callerKey{}, &caller{userID: c.userID, email: c.email, workspace: inv.WorkspaceID})
		role := authz.Role(inv.Role)
		m, err := q.Member(ctx, store.MemberParams{WorkspaceID: inv.WorkspaceID, UserID: c.userID})
		switch {
		case noRows(err):
			if err := q.AddMember(ctx, store.AddMemberParams{WorkspaceID: inv.WorkspaceID, UserID: c.userID, Role: string(role), JoinedAt: now}); err != nil {
				return internal(err)
			}
			m.Role = string(role)
		case err != nil:
			return internal(err)
		case role.Rank() > authz.Role(m.Role).Rank():
			if err := q.SetMemberRole(ctx, store.SetMemberRoleParams{WorkspaceID: inv.WorkspaceID, UserID: c.userID, Role: string(role)}); err != nil {
				return internal(err)
			}
			m.Role = string(role)
		}
		// Roles in projects only go up: an invite as a developer leaves a teamlead a teamlead.
		have, err := q.UserProjectRoles(ctx, store.UserProjectRolesParams{WorkspaceID: inv.WorkspaceID, UserID: c.userID})
		if err != nil {
			return internal(err)
		}
		current := map[string]authz.Role{}
		for _, r := range have {
			current[r.ProjectID] = authz.Role(r.Role)
		}
		for _, pid := range inv.ProjectIds {
			if current[pid].Rank() >= role.Rank() {
				continue
			}
			if err := q.SetProjectRole(ctx, store.SetProjectRoleParams{WorkspaceID: inv.WorkspaceID, ProjectID: pid, UserID: c.userID, Role: string(role)}); err != nil {
				return internal(err)
			}
		}
		if err := q.AcceptInvite(ctx, store.AcceptInviteParams{ID: inv.ID, AcceptedAt: &now, AcceptedBy: &c.userID}); err != nil {
			return internal(err)
		}
		if err := s.audit(ctx, q, inv.WorkspaceID, "member.joined", c.email, inv.Role); err != nil {
			return internal(err)
		}
		w, err := q.Workspace(ctx, inv.WorkspaceID)
		if err != nil {
			return internal(err)
		}
		out, err = s.workspaceInfo(ctx, q, w, m.Role)
		if err != nil {
			return internal(err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.AcceptInviteResponse{Workspace: out}), nil
}

func (s *Service) ListMembers(ctx context.Context, _ *connect.Request[workspacev1.ListMembersRequest]) (*connect.Response[workspacev1.ListMembersResponse], error) {
	ws := callerOf(ctx).workspace
	members, err := s.q.Members(ctx, ws)
	if err != nil {
		return nil, internal(err)
	}
	roles, err := s.q.ProjectRoles(ctx, ws)
	if err != nil {
		return nil, internal(err)
	}
	grants, err := s.q.Grants(ctx, ws)
	if err != nil {
		return nil, internal(err)
	}
	given := map[[2]string][]string{}
	for _, g := range grants {
		k := [2]string{g.UserID, g.Project}
		given[k] = append(given[k], g.Permission)
	}
	byUser := map[string][]*workspacev1.ProjectRole{}
	for _, r := range roles {
		g := given[[2]string{r.UserID, r.Project}]
		sort.Strings(g)
		byUser[r.UserID] = append(byUser[r.UserID], &workspacev1.ProjectRole{Project: r.Project, Role: r.Role, Grants: g})
	}
	resp := &workspacev1.ListMembersResponse{}
	for _, m := range members {
		ps := byUser[m.UserID]
		sort.Slice(ps, func(i, j int) bool { return ps[i].Project < ps[j].Project })
		resp.Members = append(resp.Members, &workspacev1.Member{UserId: m.UserID, Email: m.Email, Role: m.Role, Projects: ps, JoinedAt: timestamppb.New(m.JoinedAt)})
	}
	return connect.NewResponse(resp), nil
}

// projectMember finds a member's role in a project of the caller's workspace.
func (s *Service) projectMember(ctx context.Context, q *store.Queries, project, email string) (store.Member, string, authz.Role, error) {
	m, err := s.memberByEmail(ctx, q, email)
	if err != nil {
		return m, "", "", err
	}
	p, err := q.ProjectByName(ctx, store.ProjectByNameParams{WorkspaceID: callerOf(ctx).workspace, Name: project})
	if noRows(err) {
		return m, "", "", notFound("no project %q in this workspace", project)
	}
	if err != nil {
		return m, "", "", internal(err)
	}
	roles, err := q.UserProjectRoles(ctx, store.UserProjectRolesParams{WorkspaceID: m.WorkspaceID, UserID: m.UserID})
	if err != nil {
		return m, "", "", internal(err)
	}
	for _, r := range roles {
		if r.ProjectID == p.ID {
			return m, p.ID, authz.Role(r.Role), nil
		}
	}
	return m, p.ID, "", nil
}

func (s *Service) GrantPermission(ctx context.Context, req *connect.Request[workspacev1.GrantPermissionRequest]) (*connect.Response[workspacev1.GrantPermissionResponse], error) {
	return grantOrRevoke[workspacev1.GrantPermissionResponse](s, ctx, req.Msg.Project, req.Msg.Email, req.Msg.Permission, true)
}

func (s *Service) RevokePermission(ctx context.Context, req *connect.Request[workspacev1.RevokePermissionRequest]) (*connect.Response[workspacev1.RevokePermissionResponse], error) {
	return grantOrRevoke[workspacev1.RevokePermissionResponse](s, ctx, req.Msg.Project, req.Msg.Email, req.Msg.Permission, false)
}

func grantOrRevoke[R any](s *Service, ctx context.Context, project, email, permission string, grant bool) (*connect.Response[R], error) {
	p := authz.Permission(strings.TrimSpace(permission))
	if !authz.Valid(p) {
		return nil, invalid("unknown permission %q; grantable: %v", permission, authz.Grantable)
	}
	c := callerOf(ctx)
	if err := c.subject.CanGrant(p, project); err != nil {
		return nil, denied("%v", err)
	}
	err := s.tx(ctx, func(q *store.Queries) error {
		m, pid, role, err := s.projectMember(ctx, q, project, email)
		if err != nil {
			return err
		}
		if role != authz.Developer {
			return connect.NewError(connect.CodeFailedPrecondition, errorf("%s is not a developer of %q: permissions are given to developers", normalizeEmail(email), project))
		}
		action := "permission.granted"
		if grant {
			err = q.Grant(ctx, store.GrantParams{WorkspaceID: c.workspace, ProjectID: pid, UserID: m.UserID, Permission: string(p), GrantedBy: c.userID, GrantedAt: s.now()})
		} else {
			action = "permission.revoked"
			var n int64
			n, err = q.RevokeGrant(ctx, store.RevokeGrantParams{ProjectID: pid, UserID: m.UserID, Permission: string(p)})
			if err == nil && n == 0 {
				return notFound("%s has no %s in %q", normalizeEmail(email), p, project)
			}
		}
		if err != nil {
			return internal(err)
		}
		return s.audit(ctx, q, c.workspace, action, normalizeEmail(email), fmt.Sprintf("%s in %s", p, project))
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(new(R)), nil
}

func (s *Service) RemoveFromProject(ctx context.Context, req *connect.Request[workspacev1.RemoveFromProjectRequest]) (*connect.Response[workspacev1.RemoveFromProjectResponse], error) {
	c := callerOf(ctx)
	err := s.tx(ctx, func(q *store.Queries) error {
		m, pid, role, err := s.projectMember(ctx, q, req.Msg.Project, req.Msg.Email)
		if err != nil {
			return err
		}
		if role == "" {
			return notFound("%s has no role in %q", normalizeEmail(req.Msg.Email), req.Msg.Project)
		}
		if role == authz.Teamlead && c.subject.Role != authz.Admin {
			return denied("only the workspace admin removes teamleads")
		}
		if _, err := q.RemoveProjectRole(ctx, store.RemoveProjectRoleParams{ProjectID: pid, UserID: m.UserID}); err != nil {
			return internal(err)
		}
		return s.audit(ctx, q, c.workspace, "member.removed_from_project", normalizeEmail(req.Msg.Email), req.Msg.Project)
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.RemoveFromProjectResponse{}), nil
}

func (s *Service) RemoveMember(ctx context.Context, req *connect.Request[workspacev1.RemoveMemberRequest]) (*connect.Response[workspacev1.RemoveMemberResponse], error) {
	c := callerOf(ctx)
	err := s.tx(ctx, func(q *store.Queries) error {
		m, err := s.memberByEmail(ctx, q, req.Msg.Email)
		if err != nil {
			return err
		}
		if authz.Role(m.Role) == authz.Admin {
			return connect.NewError(connect.CodeFailedPrecondition, errorf("the admin cannot be removed; hand the workspace over first: devopus workspace transfer"))
		}
		if err := q.RemoveMember(ctx, store.RemoveMemberParams{WorkspaceID: c.workspace, UserID: m.UserID}); err != nil {
			return internal(err)
		}
		return s.audit(ctx, q, c.workspace, "member.removed", normalizeEmail(req.Msg.Email), "")
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.RemoveMemberResponse{}), nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
