-- +goose Up

-- The registry is per user: the workspace server owns workspaces and their members;
-- Auth only remembers which workspaces a user has and where their servers are.
DROP TABLE workspace_members;
DROP TABLE workspaces;

CREATE TABLE user_workspaces (
    user_id      text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    workspace_id text NOT NULL,
    name         text NOT NULL,
    server_url   text NOT NULL,
    added_at     timestamptz NOT NULL,
    PRIMARY KEY (user_id, workspace_id)
);

-- +goose Down
DROP TABLE user_workspaces;

CREATE TABLE workspaces (
    id         text PRIMARY KEY,
    name       text NOT NULL,
    server_url text NOT NULL,
    created_by text NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL
);

CREATE TABLE workspace_members (
    workspace_id text NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    user_id      text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    added_at     timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, user_id)
);
