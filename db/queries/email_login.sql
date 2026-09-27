-- name: CountEmailRequests :one
INSERT INTO email_login_limits (key) VALUES ($1)
ON CONFLICT (key) DO UPDATE SET
    request_count = CASE WHEN email_login_limits.window_start <= now() - interval '1 hour'
                         THEN 1 ELSE email_login_limits.request_count + 1 END,
    window_start = CASE WHEN email_login_limits.window_start <= now() - interval '1 hour'
                        THEN now() ELSE email_login_limits.window_start END
RETURNING request_count;

-- name: PruneEmailLoginLimits :exec
DELETE FROM email_login_limits WHERE window_start < now() - interval '2 hours';

-- name: CreateEmailChallenge :one
INSERT INTO email_login_challenges (id, email, purpose, code_mac, expires_at)
VALUES ($1, $2, 'sign_in', $3, now() + interval '10 minutes')
RETURNING id;

-- name: LockEmailChallenge :one
SELECT id, email, code_mac, attempt_count, expires_at, verified_at, completed_at
FROM email_login_challenges WHERE id = $1 AND purpose = 'sign_in' FOR UPDATE;

-- name: FailEmailAttempt :exec
UPDATE email_login_challenges SET attempt_count = attempt_count + 1 WHERE id = $1;

-- name: DeleteEmailChallenge :exec
DELETE FROM email_login_challenges WHERE id = $1 AND completed_at IS NULL;

-- name: CompleteEmailChallenge :exec
UPDATE email_login_challenges
SET verified_at = now(), completion_token_hash = $2, completed_at = now()
WHERE id = $1;

-- name: LockRecoveryTicket :one
SELECT id, email FROM email_login_challenges
WHERE completion_token_hash = $1 AND purpose = 'sign_in'
  AND completed_at > now() - interval '10 minutes' FOR UPDATE;

-- name: RestoreEmailAccount :one
UPDATE accounts AS a SET deletion_requested_at = NULL
FROM login_identities AS i
WHERE i.subject = $1 AND i.provider = 'email' AND i.account_id = a.id
  AND a.deletion_requested_at IS NOT NULL
  AND a.deletion_requested_at > now() - interval '30 days'
RETURNING a.id;

-- name: ConsumeRecoveryTicket :execrows
UPDATE email_login_challenges SET completion_token_hash = $2
WHERE completion_token_hash = $1 AND completed_at > now() - interval '10 minutes';
