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
