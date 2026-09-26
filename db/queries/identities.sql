-- name: GetLoginIdentity :one
SELECT id, account_id, provider, subject, linked_at
FROM login_identities
WHERE provider = $1 AND subject = $2;

-- name: CreateLoginIdentity :one
INSERT INTO login_identities (account_id, provider, subject)
VALUES ($1, $2, $3)
RETURNING id, account_id, provider, subject, linked_at;

-- name: ListLoginIdentities :many
SELECT id, account_id, provider, subject, linked_at
FROM login_identities
WHERE account_id = $1
ORDER BY linked_at, provider;
