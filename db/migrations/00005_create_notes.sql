-- +goose Up
CREATE TABLE notes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    project_id UUID,
    content TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT notes_content_length CHECK (char_length(content) BETWEEN 1 AND 20000),
    CONSTRAINT notes_account_project_fk FOREIGN KEY (account_id, project_id)
        REFERENCES projects (account_id, id) ON DELETE CASCADE
);

CREATE INDEX notes_inbox_account_idx
    ON notes (account_id, created_at DESC, id DESC) WHERE project_id IS NULL;
CREATE INDEX notes_project_account_idx
    ON notes (account_id, project_id, created_at DESC, id DESC) WHERE project_id IS NOT NULL;

-- +goose Down
DROP TABLE notes;
