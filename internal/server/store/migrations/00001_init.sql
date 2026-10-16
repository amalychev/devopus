-- +goose Up

-- Users as the server knows them from access tokens: id and email.
CREATE TABLE users (
    id      text PRIMARY KEY,
    email   text NOT NULL,
    seen_at timestamptz NOT NULL
);
CREATE INDEX users_email ON users (email);

-- One installation keeps many workspaces: every other table carries workspace_id.
CREATE TABLE workspaces (
    id         text PRIMARY KEY,
    name       text NOT NULL,
    created_at timestamptz NOT NULL
);

-- Membership with the role in the workspace. Exactly one admin per workspace.
CREATE TABLE members (
    workspace_id text NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    user_id      text NOT NULL REFERENCES users (id),
    role         text NOT NULL CHECK (role IN ('admin', 'teamlead', 'developer')),
    joined_at    timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, user_id)
);
CREATE UNIQUE INDEX members_one_admin ON members (workspace_id) WHERE role = 'admin';
CREATE INDEX members_user ON members (user_id);

CREATE TABLE projects (
    id           text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    name         text NOT NULL,
    created_by   text NOT NULL REFERENCES users (id),
    created_at   timestamptz NOT NULL,
    UNIQUE (workspace_id, name)
);

-- A member's role in a project.
CREATE TABLE project_members (
    workspace_id text NOT NULL,
    project_id   text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    user_id      text NOT NULL,
    role         text NOT NULL CHECK (role IN ('teamlead', 'developer')),
    PRIMARY KEY (project_id, user_id),
    FOREIGN KEY (workspace_id, user_id) REFERENCES members (workspace_id, user_id) ON DELETE CASCADE
);

-- Permissions given to a member in a project on top of the role.
CREATE TABLE grants (
    workspace_id text NOT NULL,
    project_id   text NOT NULL,
    user_id      text NOT NULL,
    permission   text NOT NULL,
    granted_by   text NOT NULL,
    granted_at   timestamptz NOT NULL,
    PRIMARY KEY (project_id, user_id, permission),
    FOREIGN KEY (project_id, user_id) REFERENCES project_members (project_id, user_id) ON DELETE CASCADE
);

-- Invites by email with a role and projects. The code is stored as a hash.
CREATE TABLE invites (
    id           text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    code_hash    bytea NOT NULL UNIQUE,
    email        text NOT NULL,
    role         text NOT NULL CHECK (role IN ('teamlead', 'developer')),
    project_ids  text[] NOT NULL,
    created_by   text NOT NULL REFERENCES users (id),
    created_at   timestamptz NOT NULL,
    expires_at   timestamptz NOT NULL,
    accepted_at  timestamptz,
    accepted_by  text,
    revoked_at   timestamptz
);
CREATE INDEX invites_workspace ON invites (workspace_id);

-- Who changed members, roles, permissions and the workspace, and when.
CREATE TABLE audit (
    id           bigserial PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    actor_id     text NOT NULL,
    action       text NOT NULL,
    target       text NOT NULL,
    detail       text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL
);
CREATE INDEX audit_workspace ON audit (workspace_id, id);

-- +goose Down
DROP TABLE audit;
DROP TABLE invites;
DROP TABLE grants;
DROP TABLE project_members;
DROP TABLE projects;
DROP TABLE members;
DROP TABLE workspaces;
DROP TABLE users;
