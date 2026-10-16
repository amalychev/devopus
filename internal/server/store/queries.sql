-- name: UpsertUser :exec
INSERT INTO users (id, email, seen_at) VALUES ($1, $2, $3)
ON CONFLICT (id) DO UPDATE SET email = excluded.email, seen_at = excluded.seen_at;

-- name: UserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: CreateWorkspace :one
INSERT INTO workspaces (id, name, created_at) VALUES ($1, $2, $3) RETURNING *;

-- name: Workspace :one
SELECT * FROM workspaces WHERE id = $1;

-- name: RenameWorkspace :one
UPDATE workspaces SET name = $2 WHERE id = $1 RETURNING *;

-- name: DeleteWorkspace :exec
DELETE FROM workspaces WHERE id = $1;

-- name: UserWorkspaces :many
SELECT w.*, m.role FROM workspaces w
JOIN members m ON m.workspace_id = w.id
WHERE m.user_id = $1
ORDER BY w.name;

-- name: WorkspaceAdmin :one
SELECT u.* FROM members m JOIN users u ON u.id = m.user_id
WHERE m.workspace_id = $1 AND m.role = 'admin';

-- name: AddMember :exec
INSERT INTO members (workspace_id, user_id, role, joined_at) VALUES ($1, $2, $3, $4);

-- name: SetMemberRole :exec
UPDATE members SET role = $3 WHERE workspace_id = $1 AND user_id = $2;

-- name: Member :one
SELECT * FROM members WHERE workspace_id = $1 AND user_id = $2;

-- name: RemoveMember :exec
DELETE FROM members WHERE workspace_id = $1 AND user_id = $2;

-- name: Members :many
SELECT m.*, u.email FROM members m JOIN users u ON u.id = m.user_id
WHERE m.workspace_id = $1
ORDER BY CASE m.role WHEN 'admin' THEN 0 WHEN 'teamlead' THEN 1 ELSE 2 END, u.email;

-- name: CreateProject :one
INSERT INTO projects (id, workspace_id, name, created_by, created_at) VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ProjectByName :one
SELECT * FROM projects WHERE workspace_id = $1 AND name = $2;

-- name: Projects :many
SELECT * FROM projects WHERE workspace_id = $1 ORDER BY name;

-- name: SetProjectRole :exec
INSERT INTO project_members (workspace_id, project_id, user_id, role) VALUES ($1, $2, $3, $4)
ON CONFLICT (project_id, user_id) DO UPDATE SET role = excluded.role;

-- name: ProjectRoles :many
SELECT pm.*, p.name AS project FROM project_members pm JOIN projects p ON p.id = pm.project_id
WHERE pm.workspace_id = $1;

-- name: UserProjectRoles :many
SELECT pm.*, p.name AS project FROM project_members pm JOIN projects p ON p.id = pm.project_id
WHERE pm.workspace_id = $1 AND pm.user_id = $2;

-- name: RemoveProjectRole :execrows
DELETE FROM project_members WHERE project_id = $1 AND user_id = $2;

-- name: Grant :exec
INSERT INTO grants (workspace_id, project_id, user_id, permission, granted_by, granted_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT DO NOTHING;

-- name: RevokeGrant :execrows
DELETE FROM grants WHERE project_id = $1 AND user_id = $2 AND permission = $3;

-- name: Grants :many
SELECT g.*, p.name AS project FROM grants g JOIN projects p ON p.id = g.project_id
WHERE g.workspace_id = $1;

-- name: UserGrants :many
SELECT g.*, p.name AS project FROM grants g JOIN projects p ON p.id = g.project_id
WHERE g.workspace_id = $1 AND g.user_id = $2;

-- name: CreateInvite :one
INSERT INTO invites (id, workspace_id, code_hash, email, role, project_ids, created_by, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: OpenInvites :many
SELECT i.*, u.email AS invited_by FROM invites i JOIN users u ON u.id = i.created_by
WHERE i.workspace_id = $1 AND i.accepted_at IS NULL AND i.revoked_at IS NULL AND i.expires_at > $2
ORDER BY i.created_at;

-- name: Invite :one
SELECT * FROM invites WHERE workspace_id = $1 AND id = $2;

-- name: InviteByCodeForUpdate :one
SELECT * FROM invites WHERE code_hash = $1 FOR UPDATE;

-- name: RevokeInvite :exec
UPDATE invites SET revoked_at = $3 WHERE workspace_id = $1 AND id = $2;

-- name: AcceptInvite :exec
UPDATE invites SET accepted_at = $2, accepted_by = $3 WHERE id = $1;

-- name: AddAudit :exec
INSERT INTO audit (workspace_id, actor_id, action, target, detail, created_at)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: Audit :many
SELECT a.*, coalesce(u.email, a.actor_id)::text AS actor FROM audit a LEFT JOIN users u ON u.id = a.actor_id
WHERE a.workspace_id = $1
ORDER BY a.id DESC
LIMIT $2;

-- name: ProjectByNameForUpdate :one
SELECT * FROM projects WHERE workspace_id = $1 AND name = $2 FOR UPDATE;

-- name: SetProjectVersion :exec
UPDATE projects SET version = $2 WHERE id = $1;

-- name: DeleteProject :exec
DELETE FROM projects WHERE id = $1;

-- name: AddProjectVersion :exec
INSERT INTO project_versions (workspace_id, project_id, version, config, author_id, message, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: ProjectVersion :one
SELECT v.*, u.email AS author FROM project_versions v JOIN users u ON u.id = v.author_id
WHERE v.project_id = $1 AND v.version = $2;

-- name: ProjectVersions :many
SELECT v.version, v.message, v.created_at, u.email AS author FROM project_versions v JOIN users u ON u.id = v.author_id
WHERE v.project_id = $1
ORDER BY v.version DESC
LIMIT $2;
