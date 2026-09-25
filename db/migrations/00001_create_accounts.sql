-- +goose Up
CREATE TABLE accounts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username TEXT CHECK (char_length(username) BETWEEN 1 AND 80),
    time_zone TEXT NOT NULL DEFAULT 'UTC',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deletion_requested_at TIMESTAMPTZ,
    CONSTRAINT accounts_time_zone_not_empty CHECK (char_length(time_zone) BETWEEN 1 AND 128)
);

CREATE INDEX accounts_pending_deletion_idx
    ON accounts (deletion_requested_at)
    WHERE deletion_requested_at IS NOT NULL;

-- +goose Down
DROP TABLE accounts;
