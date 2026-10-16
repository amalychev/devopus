-- +goose Up

-- A project's settings live in versions that never change: each change adds one,
-- a rollback adds a copy of an older one. version is the current one.
ALTER TABLE projects ADD COLUMN version integer NOT NULL DEFAULT 0;

CREATE TABLE project_versions (
    workspace_id text NOT NULL,
    project_id   text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    version      integer NOT NULL,
    -- The project in its YAML form (internal/spec).
    config       text NOT NULL,
    author_id    text NOT NULL REFERENCES users (id),
    message      text NOT NULL,
    created_at   timestamptz NOT NULL,
    PRIMARY KEY (project_id, version)
);

-- +goose Down
DROP TABLE project_versions;
ALTER TABLE projects DROP COLUMN version;
