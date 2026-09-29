-- +goose Up
CREATE TABLE projects (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    description TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    archived_at TIMESTAMPTZ,
    CONSTRAINT projects_name_length CHECK (char_length(name) BETWEEN 1 AND 120),
    CONSTRAINT projects_description_length CHECK (description IS NULL OR char_length(description) <= 4000),
    -- Future notes/threads can reference (account_id, project_id) to enforce ownership.
    CONSTRAINT projects_account_id_id_unique UNIQUE (account_id, id)
);

CREATE INDEX projects_active_account_idx
    ON projects (account_id, created_at DESC, id DESC) WHERE archived_at IS NULL;
CREATE INDEX projects_archived_account_idx
    ON projects (account_id, archived_at DESC, id DESC) WHERE archived_at IS NOT NULL;

-- +goose Down
DROP TABLE projects;
