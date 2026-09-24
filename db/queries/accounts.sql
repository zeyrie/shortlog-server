-- name: CreateAccount :one
INSERT INTO accounts DEFAULT VALUES
RETURNING id, time_zone, created_at;

-- name: GetAccount :one
SELECT id, time_zone, created_at
FROM accounts
WHERE id = $1;
