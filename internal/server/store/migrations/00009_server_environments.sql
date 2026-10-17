-- +goose Up

-- Environments of projects on servers of the admin (R14, ADR-0020): staging or
-- production at a public domain. The server runs the project because it is a member
-- of it (project_members, role server); this row says how. Its creator signs it: a
-- server's agent takes no domain on the workspace server's word.
CREATE TABLE server_environments (
    workspace_id text NOT NULL,
    server_id    text NOT NULL,
    project_id   text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    kind         text NOT NULL CHECK (kind IN ('staging', 'production')),
    -- The public host the project answers on: staging.shop.acme.com.
    domain       text NOT NULL,
    -- signing.EnvironmentMessage signed by the creator's device.
    signature    bytea NOT NULL,
    signer_device text NOT NULL,
    created_by   text NOT NULL REFERENCES users (id),
    created_at   timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, server_id, project_id),
    FOREIGN KEY (workspace_id, server_id) REFERENCES servers (workspace_id, id) ON DELETE CASCADE
);
-- A public host belongs to one environment of the installation.
CREATE UNIQUE INDEX server_environments_domain ON server_environments (domain);

-- +goose Down
DROP TABLE server_environments;
