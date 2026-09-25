-- name: CreateAccount :one
INSERT INTO accounts DEFAULT VALUES
RETURNING id, username, time_zone, created_at, deletion_requested_at;

-- name: GetAccount :one
SELECT id, username, time_zone, created_at, deletion_requested_at
FROM accounts
WHERE id = $1;
