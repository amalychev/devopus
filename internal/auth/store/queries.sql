-- name: CreateUser :one
INSERT INTO users (id, email, plan, created_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: UserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: UserByID :one
SELECT * FROM users WHERE id = $1;

-- name: CreateChallenge :exec
INSERT INTO email_challenges (id, email, purpose, code_hash, ip, device_id, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: CountChallengesByEmail :one
SELECT count(*) FROM email_challenges WHERE email = $1 AND created_at > $2;

-- name: CountChallengesByIP :one
SELECT count(*) FROM email_challenges WHERE ip = $1 AND created_at > $2;

-- name: CountChallengesByDevice :one
SELECT count(*) FROM email_challenges WHERE device_id = $1 AND created_at > $2;

-- name: ChallengeForUpdate :one
SELECT * FROM email_challenges WHERE id = $1 FOR UPDATE;

-- name: AddChallengeAttempt :exec
UPDATE email_challenges SET attempts = attempts + 1 WHERE id = $1;

-- name: ConsumeChallenge :exec
UPDATE email_challenges SET consumed_at = $2 WHERE id = $1;

-- name: CreateSession :one
INSERT INTO sessions (id, user_id, device_name, ip, created_at, last_used_at, idle_expires_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $5, $6, $7)
RETURNING *;

-- name: SessionByID :one
SELECT * FROM sessions WHERE id = $1;

-- name: SessionForUpdate :one
SELECT * FROM sessions WHERE id = $1 FOR UPDATE;

-- name: TouchSession :one
UPDATE sessions SET last_used_at = $2, idle_expires_at = $3 WHERE id = $1
RETURNING *;

-- name: RevokeSession :exec
UPDATE sessions SET revoked_at = $2, revoke_reason = $3 WHERE id = $1 AND revoked_at IS NULL;

-- name: LiveSessions :many
SELECT * FROM sessions
WHERE user_id = $1 AND revoked_at IS NULL AND idle_expires_at > $2 AND expires_at > $2
ORDER BY last_used_at DESC;

-- name: CreateRefreshToken :exec
INSERT INTO refresh_tokens (token_hash, session_id, created_at) VALUES ($1, $2, $3);

-- name: RefreshTokenForUpdate :one
SELECT * FROM refresh_tokens WHERE token_hash = $1 FOR UPDATE;

-- name: UseRefreshToken :exec
UPDATE refresh_tokens SET used_at = $2 WHERE token_hash = $1;

-- name: CreateAuthorizationCode :exec
INSERT INTO authorization_codes (code_hash, user_id, code_challenge, redirect_uri, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: AuthorizationCodeForUpdate :one
SELECT * FROM authorization_codes WHERE code_hash = $1 FOR UPDATE;

-- name: UseAuthorizationCode :exec
UPDATE authorization_codes SET used_at = $2 WHERE code_hash = $1;

-- name: CreateDeviceCode :exec
INSERT INTO device_codes (device_code_hash, user_code, device_name, ip, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: DeviceCodeForUpdate :one
SELECT * FROM device_codes WHERE device_code_hash = $1 FOR UPDATE;

-- name: DeviceCodeByUserCode :one
SELECT * FROM device_codes WHERE user_code = $1;

-- name: PollDeviceCode :exec
UPDATE device_codes SET last_polled_at = $2 WHERE device_code_hash = $1;

-- name: ApproveDeviceCode :execrows
UPDATE device_codes SET user_id = $2, approved_at = $3
WHERE user_code = $1 AND approved_at IS NULL AND expires_at > $3;

-- name: ConsumeDeviceCode :exec
UPDATE device_codes SET consumed_at = $2 WHERE device_code_hash = $1;

-- name: CreateSigningKey :exec
INSERT INTO signing_keys (id, private_key, created_at) VALUES ($1, $2, $3);

-- name: SigningKeys :many
SELECT * FROM signing_keys ORDER BY created_at DESC;

-- name: CreateWorkspace :one
INSERT INTO workspaces (id, name, server_url, created_by, created_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: AddWorkspaceMember :exec
INSERT INTO workspace_members (workspace_id, user_id, added_at) VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;

-- name: UserWorkspaces :many
SELECT w.* FROM workspaces w
JOIN workspace_members m ON m.workspace_id = w.id
WHERE m.user_id = $1
ORDER BY w.name;

-- name: AddEvent :exec
INSERT INTO security_events (user_id, session_id, kind, ip, detail, created_at)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: UserEvents :many
SELECT * FROM security_events WHERE user_id = $1 ORDER BY id;

-- name: DeleteExpired :exec
WITH c AS (DELETE FROM email_challenges WHERE email_challenges.expires_at < $1),
     a AS (DELETE FROM authorization_codes WHERE authorization_codes.expires_at < $1),
     d AS (DELETE FROM device_codes WHERE device_codes.expires_at < $1)
DELETE FROM sessions WHERE sessions.expires_at < $1 OR sessions.idle_expires_at < $1;
