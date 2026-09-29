-- name: CreateSession :one
INSERT INTO sessions (account_id, token_hash, user_agent, device_label)
SELECT accounts.id, sqlc.arg(token_hash), sqlc.arg(user_agent), sqlc.arg(device_label)
FROM accounts
WHERE accounts.id = sqlc.arg(account_id) AND accounts.deletion_requested_at IS NULL
RETURNING sessions.id, sessions.account_id, sessions.token_hash,
          sessions.user_agent, sessions.device_label, sessions.created_at,
          sessions.last_used_at, sessions.authenticated_at, sessions.revoked_at;

-- name: AuthenticateSession :one
SELECT s.id AS session_id, s.account_id, s.authenticated_at,
       s.last_used_at, now()::timestamptz AS checked_at,
       a.username, a.time_zone, a.created_at
FROM sessions AS s
JOIN accounts AS a ON s.account_id = a.id
WHERE s.token_hash = $1
  AND s.revoked_at IS NULL
  AND s.last_used_at > now() - interval '31 days'
  AND a.deletion_requested_at IS NULL;

-- name: TouchSession :execrows
UPDATE sessions AS s
SET last_used_at = now()
FROM accounts AS a
WHERE s.id = $1
  AND s.account_id = a.id
  AND s.revoked_at IS NULL
  AND s.last_used_at > now() - interval '31 days'
  AND s.last_used_at <= now() - interval '1 day'
  AND a.deletion_requested_at IS NULL;

-- name: ListActiveSessions :many
SELECT id, user_agent, device_label, created_at, last_used_at, authenticated_at
FROM sessions
WHERE account_id = $1
  AND revoked_at IS NULL
  AND last_used_at > now() - interval '31 days'
ORDER BY last_used_at DESC, id;

-- name: RevokeSession :execrows
UPDATE sessions
SET revoked_at = now()
WHERE id = $1 AND account_id = $2 AND revoked_at IS NULL;

-- name: RevokeAllSessions :execrows
UPDATE sessions
SET revoked_at = now()
WHERE account_id = $1 AND revoked_at IS NULL;
