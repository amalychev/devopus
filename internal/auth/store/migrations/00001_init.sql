-- +goose Up

-- Accounts. Email is stored lowercased.
CREATE TABLE users (
    id         text PRIMARY KEY,
    email      text NOT NULL UNIQUE,
    plan       text NOT NULL,
    created_at timestamptz NOT NULL
);

-- One-time codes sent to an email. The code itself is not stored, only its hash.
CREATE TABLE email_challenges (
    id          text PRIMARY KEY,
    email       text NOT NULL,
    purpose     text NOT NULL,
    code_hash   bytea NOT NULL,
    attempts    integer NOT NULL DEFAULT 0,
    ip          text NOT NULL,
    device_id   text NOT NULL,
    created_at  timestamptz NOT NULL,
    expires_at  timestamptz NOT NULL,
    consumed_at timestamptz
);
-- Limits count the codes sent per email, IP and device in a window.
CREATE INDEX email_challenges_email ON email_challenges (email, created_at);
CREATE INDEX email_challenges_ip ON email_challenges (ip, created_at);
CREATE INDEX email_challenges_device ON email_challenges (device_id, created_at);

-- A session per device. It ends at idle_expires_at unless refreshed, at expires_at
-- in any case, or when revoked.
CREATE TABLE sessions (
    id              text PRIMARY KEY,
    user_id         text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    device_name     text NOT NULL,
    ip              text NOT NULL,
    created_at      timestamptz NOT NULL,
    last_used_at    timestamptz NOT NULL,
    idle_expires_at timestamptz NOT NULL,
    expires_at      timestamptz NOT NULL,
    revoked_at      timestamptz,
    revoke_reason   text
);
CREATE INDEX sessions_user ON sessions (user_id);

-- Refresh tokens by hash. A token is used once: used_at set on a token presented
-- again means it leaked, and the session is revoked.
CREATE TABLE refresh_tokens (
    token_hash bytea PRIMARY KEY,
    session_id text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL,
    used_at    timestamptz
);
CREATE INDEX refresh_tokens_session ON refresh_tokens (session_id);

-- Authorization codes of the browser sign-in, bound to the PKCE challenge and the
-- redirect URI.
CREATE TABLE authorization_codes (
    code_hash      bytea PRIMARY KEY,
    user_id        text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_challenge text NOT NULL,
    redirect_uri   text NOT NULL,
    created_at     timestamptz NOT NULL,
    expires_at     timestamptz NOT NULL,
    used_at        timestamptz
);

-- Device flow (RFC 8628): the device polls with device_code, the person confirms
-- user_code on another device.
CREATE TABLE device_codes (
    device_code_hash bytea PRIMARY KEY,
    user_code        text NOT NULL UNIQUE,
    device_name      text NOT NULL,
    ip               text NOT NULL,
    created_at       timestamptz NOT NULL,
    expires_at       timestamptz NOT NULL,
    last_polled_at   timestamptz,
    user_id          text REFERENCES users (id) ON DELETE CASCADE,
    approved_at      timestamptz,
    consumed_at      timestamptz
);

-- Ed25519 keys that sign access tokens; the newest signs, JWKS publishes the recent ones.
CREATE TABLE signing_keys (
    id          text PRIMARY KEY,
    private_key bytea NOT NULL,
    created_at  timestamptz NOT NULL
);

-- The workspace registry: which workspaces a user has and where their servers are.
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

-- The account's security log: registrations, sign-ins, sign-outs, revocations, plans.
CREATE TABLE security_events (
    id         bigserial PRIMARY KEY,
    user_id    text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    session_id text,
    kind       text NOT NULL,
    ip         text NOT NULL,
    detail     text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL
);
CREATE INDEX security_events_user ON security_events (user_id, created_at);

-- +goose Down
DROP TABLE security_events;
DROP TABLE workspace_members;
DROP TABLE workspaces;
DROP TABLE signing_keys;
DROP TABLE device_codes;
DROP TABLE authorization_codes;
DROP TABLE refresh_tokens;
DROP TABLE sessions;
DROP TABLE email_challenges;
DROP TABLE users;
