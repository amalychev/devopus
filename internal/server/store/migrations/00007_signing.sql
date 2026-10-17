-- +goose Up

-- Signatures of settings (ADR-0018). The server keeps public keys, certificates and
-- signatures; agents check them, so the server can forge none of them.

-- The root of trust: a signing key of the admin's device, set once.
ALTER TABLE workspaces ADD COLUMN trust_root bytea;
ALTER TABLE workspaces ADD COLUMN trust_root_device text NOT NULL DEFAULT '';
ALTER TABLE workspaces ADD COLUMN trust_root_user text NOT NULL DEFAULT '';

-- Devices' signing keys, with the root's certificate once the admin gives it.
CREATE TABLE signing_keys (
    workspace_id text NOT NULL,
    device_id    text NOT NULL,
    user_id      text NOT NULL,
    name         text NOT NULL,
    public_key   bytea NOT NULL,
    certificate  bytea,
    created_at   timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, device_id, user_id),
    FOREIGN KEY (workspace_id, user_id) REFERENCES members (workspace_id, user_id) ON DELETE CASCADE
);

-- The author's signature of a version.
ALTER TABLE project_versions ADD COLUMN signature bytea;
ALTER TABLE project_versions ADD COLUMN signer_device text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE project_versions DROP COLUMN signer_device;
ALTER TABLE project_versions DROP COLUMN signature;
DROP TABLE signing_keys;
ALTER TABLE workspaces DROP COLUMN trust_root_user;
ALTER TABLE workspaces DROP COLUMN trust_root_device;
ALTER TABLE workspaces DROP COLUMN trust_root;
