-- name: CreateNote :one
INSERT INTO notes (account_id, project_id, content)
VALUES ($1, sqlc.narg(project_id)::uuid, $2)
RETURNING id, account_id, project_id, content, created_at, updated_at;

-- name: GetNote :one
SELECT id, account_id, project_id, content, created_at, updated_at
FROM notes WHERE id = $1 AND account_id = $2;

-- name: LockNote :one
SELECT id, account_id, project_id, content, created_at, updated_at
FROM notes WHERE id = $1 AND account_id = $2 FOR UPDATE;

-- name: LockNoteProject :one
SELECT id, account_id, name, description, created_at, updated_at, archived_at
FROM projects WHERE id = $1 AND account_id = $2 FOR SHARE;

-- name: UpdateNote :one
UPDATE notes SET content = $3, project_id = sqlc.narg(project_id)::uuid, updated_at = now()
WHERE id = $1 AND account_id = $2
RETURNING id, account_id, project_id, content, created_at, updated_at;

-- name: DeleteNote :execrows
DELETE FROM notes WHERE id = $1 AND account_id = $2;

-- name: ListInboxNotes :many
SELECT id, account_id, project_id, content, created_at, updated_at
FROM notes
WHERE account_id = sqlc.arg(account_id) AND project_id IS NULL
  AND (sqlc.narg(cursor_at)::timestamptz IS NULL
       OR (created_at, id) < (sqlc.narg(cursor_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_size)::int;

-- name: ListProjectNotes :many
SELECT id, account_id, project_id, content, created_at, updated_at
FROM notes
WHERE account_id = sqlc.arg(account_id) AND project_id = sqlc.arg(project_id)
  AND (sqlc.narg(cursor_at)::timestamptz IS NULL
       OR (created_at, id) < (sqlc.narg(cursor_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_size)::int;
