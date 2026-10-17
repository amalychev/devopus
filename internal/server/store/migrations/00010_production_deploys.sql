-- +goose Up

-- Deploys to production environments (R14b, ADR-0020): production runs the version
-- someone with production.deploy chose and signed, not the latest. Every deploy stays:
-- the newest is what runs; a rollback is a deploy of an older version.
CREATE TABLE production_deploys (
    workspace_id  text NOT NULL,
    server_id     text NOT NULL,
    project_id    text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    seq           bigserial,
    version       integer NOT NULL,
    -- signing.DeployMessage signed by the deployer's device.
    signature     bytea NOT NULL,
    signer_device text NOT NULL,
    deployed_by   text NOT NULL REFERENCES users (id),
    deployed_at   timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, server_id, project_id, seq),
    FOREIGN KEY (workspace_id, server_id, project_id) REFERENCES server_environments (workspace_id, server_id, project_id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE production_deploys;
