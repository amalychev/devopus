// Package authz is the list of permissions, the roles that bundle them and the check
// (ADR-0015, docs/product/roles.md). The workspace server decides with it on every
// call; the CLI uses the same list for hints.
package authz

import (
	"fmt"
	"slices"
	"sort"
)

// Permission is something a member may do.
type Permission string

// Permissions of docs/product/roles.md.
const (
	WorkspaceView    Permission = "workspace.view"   // see the workspace, its projects and members
	WorkspaceManage  Permission = "workspace.manage" // rename, transfer, delete, audit
	ProjectCreate    Permission = "project.create"
	ProjectConfigure Permission = "project.configure"
	AppsManage       Permission = "apps.manage"
	ServicesManage   Permission = "services.manage"
	ServersManage    Permission = "servers.manage"
	SecretsManage    Permission = "secrets.manage"
	SecretsRead      Permission = "secrets.read"
	MembersInvite    Permission = "members.invite"
	MembersManage    Permission = "members.manage"
	OverridesUse     Permission = "overrides.use"
	EnvUse           Permission = "env.use"
)

// All is every permission.
var All = []Permission{
	WorkspaceView, WorkspaceManage, ProjectCreate, ProjectConfigure, AppsManage, ServicesManage, ServersManage,
	SecretsManage, SecretsRead, MembersInvite, MembersManage, OverridesUse, EnvUse,
}

// Grantable are the permissions an admin or a teamlead gives a developer in a project,
// on top of the developer's role.
var Grantable = []Permission{ProjectConfigure, AppsManage, ServicesManage, ServersManage, SecretsManage}

// Valid reports whether p is a known permission.
func Valid(p Permission) bool { return slices.Contains(All, p) }

// Role is a bundle of permissions by default.
type Role string

const (
	Admin     Role = "admin"     // the workspace's one owner: everything
	Teamlead  Role = "teamlead"  // a project's lead; in the workspace, may create projects
	Developer Role = "developer" // works with a project's environments
	// Server is an admin's server (R13, ADR-0019): it runs the projects it is given
	// and nothing else; it signs in with a device token, not as a person.
	Server Role = "server"
)

// Rank orders roles: a higher role includes what a lower one may do.
func (r Role) Rank() int {
	switch r {
	case Admin:
		return 3
	case Teamlead:
		return 2
	case Developer:
		return 1
	}
	return 0
}

// ParseRole accepts the roles an invite can give.
func ParseRole(s string) (Role, error) {
	switch Role(s) {
	case Teamlead, Developer:
		return Role(s), nil
	case Admin:
		return "", fmt.Errorf("a workspace has one admin; hand it over with 'devopus workspace transfer'")
	}
	return "", fmt.Errorf("unknown role %q: teamlead or developer", s)
}

// Scope is where a permission applies.
type Scope int

const (
	ScopeWorkspace Scope = iota + 1
	ScopeProject
)

// workspaceDefaults are what a member's workspace role allows in the workspace itself.
var workspaceDefaults = map[Role][]Permission{
	Teamlead:  {WorkspaceView, ProjectCreate, MembersInvite},
	Developer: {WorkspaceView},
	Server:    {WorkspaceView},
}

// projectDefaults are what a role in a project allows in it.
var projectDefaults = map[Role][]Permission{
	Teamlead: {WorkspaceView, ProjectConfigure, AppsManage, ServicesManage, ServersManage, SecretsManage, SecretsRead,
		MembersInvite, MembersManage, OverridesUse, EnvUse},
	Developer: {WorkspaceView, SecretsRead, OverridesUse, EnvUse},
	Server:    {WorkspaceView, SecretsRead, EnvUse},
}

// Subject is a member as the check sees them.
type Subject struct {
	// Role in the workspace: admin, teamlead or developer; empty for a non-member.
	Role Role
	// Projects maps a project to the member's role in it.
	Projects map[string]Role
	// Grants maps a project to permissions given on top of the role.
	Grants map[string][]Permission
}

// Member reports whether the subject is in the workspace.
func (s Subject) Member() bool { return s.Role != "" }

// Can reports whether the subject may do p: in the workspace when project is empty,
// otherwise in that project.
func (s Subject) Can(p Permission, project string) bool {
	if !s.Member() {
		return false
	}
	if s.Role == Admin {
		return true
	}
	if project == "" {
		return slices.Contains(workspaceDefaults[s.Role], p)
	}
	role, ok := s.Projects[project]
	if !ok {
		return false
	}
	return slices.Contains(projectDefaults[role], p) || slices.Contains(s.Grants[project], p)
}

// In lists the permissions the subject has in a project (or the workspace), sorted.
func (s Subject) In(project string) []Permission {
	var out []Permission
	for _, p := range All {
		if s.Can(p, project) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// CanInvite checks an invite of a role into projects: an admin invites teamleads and
// developers anywhere; a teamlead invites developers into the projects they lead.
func (s Subject) CanInvite(role Role, projects []string) error {
	if s.Role == Admin {
		return nil
	}
	if !s.Can(MembersInvite, "") {
		return fmt.Errorf("inviting needs %s", MembersInvite)
	}
	if role != Developer {
		return fmt.Errorf("a teamlead invites developers; teamleads are invited by the admin")
	}
	if len(projects) == 0 {
		return fmt.Errorf("a teamlead invites into their projects: name them with --project")
	}
	for _, p := range projects {
		if !s.Can(MembersInvite, p) {
			return fmt.Errorf("you do not lead project %q", p)
		}
	}
	return nil
}

// CanGrant checks giving a permission to a developer of a project: it must be
// grantable, and the one who gives it must have it there.
func (s Subject) CanGrant(p Permission, project string) error {
	if !slices.Contains(Grantable, p) {
		return fmt.Errorf("%s cannot be granted; grantable: %v", p, Grantable)
	}
	if !s.Can(MembersManage, project) || !s.Can(p, project) {
		return fmt.Errorf("granting %s in %q needs %s and %s there", p, project, MembersManage, p)
	}
	return nil
}
