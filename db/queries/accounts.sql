-- name: CreateAccount :one
INSERT INTO accounts (username, time_zone) VALUES ($1, $2)
RETURNING id, username, time_zone, created_at, deletion_requested_at;

-- name: GetAccount :one
SELECT id, username, time_zone, created_at, deletion_requested_at
FROM accounts
WHERE id = $1;

-- name: UpdateAccountProfile :one
UPDATE accounts SET username = $2, time_zone = $3
WHERE id = $1 AND deletion_requested_at IS NULL
RETURNING id, username, time_zone, created_at, deletion_requested_at;

-- name: RequestAccountDeletion :one
UPDATE accounts SET deletion_requested_at = now()
WHERE id = $1 AND deletion_requested_at IS NULL
RETURNING deletion_requested_at;

-- name: LockExpiredAccount :one
SELECT id FROM accounts
WHERE deletion_requested_at <= now() - interval '30 days'
ORDER BY deletion_requested_at, id
LIMIT 1 FOR UPDATE SKIP LOCKED;

-- name: DeleteAccountEmailChallenges :exec
DELETE FROM email_login_challenges
WHERE email IN (SELECT subject FROM login_identities WHERE account_id = $1 AND provider = 'email');

-- name: DeleteAccountTelegramAttempts :exec
DELETE FROM telegram_login_attempts
WHERE telegram_subject IN (SELECT subject FROM login_identities WHERE account_id = $1 AND provider = 'telegram');

-- name: PurgeExpiredAccount :execrows
DELETE FROM accounts WHERE id = $1 AND deletion_requested_at <= now() - interval '30 days';
