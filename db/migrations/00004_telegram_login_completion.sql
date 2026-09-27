-- +goose Up
ALTER TABLE telegram_login_attempts
    ADD COLUMN callback_claimed_at TIMESTAMPTZ,
    ADD COLUMN recovery_ticket_hash BYTEA UNIQUE,
    ADD CONSTRAINT telegram_recovery_ticket_length
        CHECK (recovery_ticket_hash IS NULL OR octet_length(recovery_ticket_hash) = 32),
    ADD CONSTRAINT telegram_recovery_ticket_completed
        CHECK (recovery_ticket_hash IS NULL OR completed_at IS NOT NULL);

-- +goose Down
ALTER TABLE telegram_login_attempts
    DROP CONSTRAINT telegram_recovery_ticket_completed,
    DROP CONSTRAINT telegram_recovery_ticket_length,
    DROP COLUMN recovery_ticket_hash,
    DROP COLUMN callback_claimed_at;
