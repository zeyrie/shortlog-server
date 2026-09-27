-- name: CreateTelegramAttempt :one
INSERT INTO telegram_login_attempts (state_hash, pkce_verifier_ciphertext, nonce, poll_secret_hash, purpose, expires_at)
VALUES ($1, $2, $3, $4, 'sign_in', now() + interval '10 minutes')
RETURNING id;

-- name: PruneTelegramAttempts :exec
DELETE FROM telegram_login_attempts WHERE expires_at < now() - interval '1 day';

-- name: ClaimTelegramCallback :one
UPDATE telegram_login_attempts SET callback_claimed_at = now()
WHERE state_hash = $1 AND purpose = 'sign_in' AND expires_at > now()
  AND callback_claimed_at IS NULL AND approved_at IS NULL
RETURNING id, pkce_verifier_ciphertext, nonce;

-- name: ApproveTelegramAttempt :execrows
UPDATE telegram_login_attempts SET approved_at = now(), telegram_subject = $2
WHERE id = $1 AND callback_claimed_at IS NOT NULL AND approved_at IS NULL AND expires_at > now();

-- name: LockTelegramAttempt :one
SELECT id, telegram_subject, approved_at, completed_at, expires_at
FROM telegram_login_attempts
WHERE id = $1 AND poll_secret_hash = $2 AND purpose = 'sign_in' FOR UPDATE;

-- name: CompleteTelegramAttempt :execrows
UPDATE telegram_login_attempts SET completed_at = now(), recovery_ticket_hash = $2
WHERE id = $1 AND approved_at IS NOT NULL AND completed_at IS NULL AND expires_at > now();

-- name: LockTelegramRecovery :one
SELECT id, telegram_subject FROM telegram_login_attempts
WHERE recovery_ticket_hash = $1 AND completed_at > now() - interval '10 minutes' FOR UPDATE;

-- name: RestoreTelegramAccount :one
UPDATE accounts AS a SET deletion_requested_at = NULL
FROM login_identities AS i
WHERE i.subject = $1 AND i.provider = 'telegram' AND i.account_id = a.id
  AND a.deletion_requested_at IS NOT NULL
  AND a.deletion_requested_at > now() - interval '30 days'
RETURNING a.id;

-- name: ConsumeTelegramRecovery :execrows
UPDATE telegram_login_attempts SET recovery_ticket_hash = $2
WHERE recovery_ticket_hash = $1 AND completed_at > now() - interval '10 minutes';
