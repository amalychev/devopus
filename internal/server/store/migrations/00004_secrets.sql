-- +goose Up

-- End-to-end secrets (ADR-0009). The server keeps public keys, keys wrapped for
-- devices and ciphertexts: no value and no private key.

-- The workspace's public keys, one per generation; key_generation is the current one
-- (0: no key yet).
ALTER TABLE workspaces ADD COLUMN key_generation integer NOT NULL DEFAULT 0;

CREATE TABLE workspace_keys (
    workspace_id text NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    generation   integer NOT NULL,
    public_key   text NOT NULL,
    created_by   text NOT NULL REFERENCES users (id),
    created_at   timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, generation)
);

-- Devices' public keys. A removed member's go with them, and so do their wraps.
CREATE TABLE device_keys (
    workspace_id text NOT NULL,
    device_id    text NOT NULL,
    user_id      text NOT NULL,
    name         text NOT NULL,
    public_key   text NOT NULL,
    created_at   timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, device_id, user_id),
    FOREIGN KEY (workspace_id, user_id) REFERENCES members (workspace_id, user_id) ON DELETE CASCADE
);

-- The workspace's private key wrapped for a device.
CREATE TABLE key_wraps (
    workspace_id text NOT NULL,
    generation   integer NOT NULL,
    device_id    text NOT NULL,
    user_id      text NOT NULL,
    wrapped      bytea NOT NULL,
    wrapped_by   text NOT NULL,
    created_at   timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, generation, device_id, user_id),
    FOREIGN KEY (workspace_id, device_id, user_id) REFERENCES device_keys (workspace_id, device_id, user_id) ON DELETE CASCADE
);

CREATE TABLE secrets (
    workspace_id text NOT NULL,
    project_id   text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name         text NOT NULL,
    generation   integer NOT NULL,
    ciphertext   bytea NOT NULL,
    updated_by   text NOT NULL REFERENCES users (id),
    updated_at   timestamptz NOT NULL,
    PRIMARY KEY (project_id, name)
);
CREATE INDEX secrets_workspace ON secrets (workspace_id);

-- +goose Down
DROP TABLE secrets;
DROP TABLE key_wraps;
DROP TABLE device_keys;
DROP TABLE workspace_keys;
ALTER TABLE workspaces DROP COLUMN key_generation;
