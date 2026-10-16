-- +goose Up

-- Devices whose agents connected, per user: what they last reported.
CREATE TABLE devices (
    workspace_id  text NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    device_id     text NOT NULL,
    user_id       text NOT NULL,
    name          text NOT NULL,
    agent_version text NOT NULL,
    last_seen     timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, device_id, user_id),
    FOREIGN KEY (workspace_id, user_id) REFERENCES members (workspace_id, user_id) ON DELETE CASCADE
);

-- The environments a device runs, as it reported them last.
CREATE TABLE device_environments (
    workspace_id   text NOT NULL,
    device_id      text NOT NULL,
    user_id        text NOT NULL,
    env_id         text NOT NULL,
    project        text NOT NULL,
    kind           text NOT NULL,
    name           text NOT NULL,
    branch         text NOT NULL,
    phase          text NOT NULL,
    url            text NOT NULL,
    error          text NOT NULL,
    config_version integer NOT NULL,
    PRIMARY KEY (workspace_id, device_id, user_id, env_id),
    FOREIGN KEY (workspace_id, device_id, user_id) REFERENCES devices (workspace_id, device_id, user_id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE device_environments;
DROP TABLE devices;
