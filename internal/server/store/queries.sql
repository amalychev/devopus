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

-- name: UpsertDevice :exec
INSERT INTO devices (workspace_id, device_id, user_id, name, agent_version, last_seen)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (workspace_id, device_id, user_id)
DO UPDATE SET name = excluded.name, agent_version = excluded.agent_version, last_seen = excluded.last_seen;

-- name: ClearDeviceEnvironments :exec
DELETE FROM device_environments WHERE workspace_id = $1 AND device_id = $2 AND user_id = $3;

-- name: AddDeviceEnvironment :exec
INSERT INTO device_environments (workspace_id, device_id, user_id, env_id, project, kind, name, branch, phase, url, error, config_version,
    overrides, rejected_overrides, pending_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15);

-- name: Devices :many
SELECT d.*, u.email FROM devices d JOIN users u ON u.id = d.user_id
WHERE d.workspace_id = $1
ORDER BY u.email, d.name;

-- name: DeviceEnvironments :many
SELECT * FROM device_environments WHERE workspace_id = $1 ORDER BY project, kind, name;

-- name: WorkspaceKey :one
SELECT k.* FROM workspace_keys k JOIN workspaces w ON w.id = k.workspace_id AND k.generation = w.key_generation
WHERE w.id = $1;

-- name: WorkspaceForUpdate :one
SELECT * FROM workspaces WHERE id = $1 FOR UPDATE;

-- name: AddWorkspaceKey :exec
INSERT INTO workspace_keys (workspace_id, generation, public_key, created_by, created_at) VALUES ($1, $2, $3, $4, $5);

-- name: SetKeyGeneration :exec
UPDATE workspaces SET key_generation = $2 WHERE id = $1;

-- name: DeviceKey :one
SELECT * FROM device_keys WHERE workspace_id = $1 AND device_id = $2 AND user_id = $3;

-- name: UpsertDeviceKey :exec
INSERT INTO device_keys (workspace_id, device_id, user_id, name, public_key, created_at) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (workspace_id, device_id, user_id) DO UPDATE SET name = excluded.name, public_key = excluded.public_key;

-- name: DeleteDeviceWraps :exec
DELETE FROM key_wraps WHERE workspace_id = $1 AND device_id = $2 AND user_id = $3;

-- name: KeyWrap :one
SELECT * FROM key_wraps WHERE workspace_id = $1 AND generation = $2 AND device_id = $3 AND user_id = $4;

-- name: AddKeyWrap :exec
INSERT INTO key_wraps (workspace_id, generation, device_id, user_id, wrapped, wrapped_by, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (workspace_id, generation, device_id, user_id) DO UPDATE SET wrapped = excluded.wrapped, wrapped_by = excluded.wrapped_by, created_at = excluded.created_at;

-- name: DeleteOldWraps :exec
DELETE FROM key_wraps WHERE workspace_id = $1 AND generation < $2;

-- name: PendingDevices :many
SELECT d.device_id, d.user_id, d.name, d.public_key, u.email FROM device_keys d
JOIN users u ON u.id = d.user_id
LEFT JOIN key_wraps k ON k.workspace_id = d.workspace_id AND k.device_id = d.device_id AND k.user_id = d.user_id AND k.generation = $2
WHERE d.workspace_id = $1 AND k.wrapped IS NULL
ORDER BY u.email, d.name;

-- name: WorkspaceDeviceKeys :many
SELECT * FROM device_keys WHERE workspace_id = $1;

-- name: SetSecret :exec
INSERT INTO secrets (workspace_id, project_id, name, generation, ciphertext, updated_by, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (project_id, name) DO UPDATE SET generation = excluded.generation, ciphertext = excluded.ciphertext,
    updated_by = excluded.updated_by, updated_at = excluded.updated_at;

-- name: DeleteSecret :execrows
DELETE FROM secrets WHERE project_id = $1 AND name = $2;

-- name: ProjectSecrets :many
SELECT s.*, u.email AS updated_by_email FROM secrets s JOIN users u ON u.id = s.updated_by
WHERE s.project_id = $1 ORDER BY s.name;

-- name: WorkspaceSecrets :many
SELECT s.*, p.name AS project FROM secrets s JOIN projects p ON p.id = s.project_id
WHERE s.workspace_id = $1 ORDER BY p.name, s.name;
