-- name: CreateProject :one
INSERT INTO projects (account_id, name, description)
VALUES ($1, $2, sqlc.narg(description)::text)
RETURNING id, account_id, name, description, created_at, updated_at, archived_at;

-- name: ListActiveProjects :many
SELECT id, account_id, name, description, created_at, updated_at, archived_at
FROM projects WHERE account_id = $1 AND archived_at IS NULL
ORDER BY created_at DESC, id DESC;

-- name: ListArchivedProjects :many
SELECT id, account_id, name, description, created_at, updated_at, archived_at
FROM projects WHERE account_id = $1 AND archived_at IS NOT NULL
ORDER BY archived_at DESC, id DESC;

-- name: GetProject :one
SELECT id, account_id, name, description, created_at, updated_at, archived_at
FROM projects WHERE id = $1 AND account_id = $2;

-- name: UpdateProject :one
UPDATE projects SET
    name = CASE WHEN sqlc.arg(name_set)::boolean THEN sqlc.arg(name)::text ELSE projects.name END,
    description = CASE WHEN sqlc.arg(description_set)::boolean THEN sqlc.narg(description)::text ELSE projects.description END,
    updated_at = now()
WHERE id = sqlc.arg(id) AND account_id = sqlc.arg(account_id) AND archived_at IS NULL
RETURNING id, account_id, name, description, created_at, updated_at, archived_at;

-- name: ArchiveProject :execrows
UPDATE projects SET archived_at = now(), updated_at = now()
WHERE id = $1 AND account_id = $2 AND archived_at IS NULL;

-- name: UnarchiveProject :execrows
UPDATE projects SET archived_at = NULL, updated_at = now()
WHERE id = $1 AND account_id = $2 AND archived_at IS NOT NULL;
