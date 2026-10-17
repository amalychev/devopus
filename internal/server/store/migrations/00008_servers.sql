-- +goose Up

-- Servers of the admin (R13, ADR-0019): machines with an agent that run projects of
-- the workspace — staging, production. A server is a member of its own kind: a user
-- row (id srv_…) with the role "server" in the workspace and in the projects it
-- runs, so devices, keys and statuses work for it as for people. It signs in with a
-- device token, not through Auth.
ALTER TABLE members DROP CONSTRAINT members_role_check;
ALTER TABLE members ADD CONSTRAINT members_role_check CHECK (role IN ('admin', 'teamlead', 'developer', 'server'));
ALTER TABLE project_members DROP CONSTRAINT project_members_role_check;
ALTER TABLE project_members ADD CONSTRAINT project_members_role_check CHECK (role IN ('teamlead', 'developer', 'server'));

CREATE TABLE servers (
    workspace_id text NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    -- The server's user id: srv_….
    id           text NOT NULL,
    name         text NOT NULL,
    created_by   text NOT NULL REFERENCES users (id),
    created_at   timestamptz NOT NULL,
    -- sha256 of the one-time join code, until the server joins.
    join_hash    text,
    join_expires timestamptz,
    -- sha256 of the device token the server got when it joined.
    token_hash   text,
    joined_at    timestamptz,
    PRIMARY KEY (workspace_id, id),
    UNIQUE (workspace_id, name),
    FOREIGN KEY (workspace_id, id) REFERENCES members (workspace_id, user_id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX servers_join ON servers (join_hash) WHERE join_hash IS NOT NULL;
CREATE UNIQUE INDEX servers_token ON servers (token_hash) WHERE token_hash IS NOT NULL;

-- +goose Down
DROP TABLE servers;
DELETE FROM project_members WHERE role = 'server';
DELETE FROM members WHERE role = 'server';
ALTER TABLE project_members DROP CONSTRAINT project_members_role_check;
ALTER TABLE project_members ADD CONSTRAINT project_members_role_check CHECK (role IN ('teamlead', 'developer'));
ALTER TABLE members DROP CONSTRAINT members_role_check;
ALTER TABLE members ADD CONSTRAINT members_role_check CHECK (role IN ('admin', 'teamlead', 'developer'));
