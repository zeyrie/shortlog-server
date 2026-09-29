-- +goose Up
CREATE TABLE login_identities (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    subject TEXT NOT NULL,
    linked_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT login_identities_provider_valid
        CHECK (provider IN ('email', 'telegram', 'google', 'apple')),
    CONSTRAINT login_identities_subject_not_empty
        CHECK (char_length(subject) BETWEEN 1 AND 512),
    CONSTRAINT login_identities_email_normalized
        CHECK (provider <> 'email' OR subject = lower(subject)),
    CONSTRAINT login_identities_one_provider_per_account
        UNIQUE (account_id, provider),
    CONSTRAINT login_identities_one_account_per_subject
        UNIQUE (provider, subject)
);

CREATE TABLE sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    token_hash BYTEA NOT NULL UNIQUE,
    user_agent TEXT NOT NULL,
    device_label TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    authenticated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ,
    CONSTRAINT sessions_token_hash_length CHECK (octet_length(token_hash) = 32),
    CONSTRAINT sessions_user_agent_length CHECK (char_length(user_agent) BETWEEN 1 AND 512),
    CONSTRAINT sessions_device_label_length CHECK (char_length(device_label) BETWEEN 1 AND 128)
);

CREATE INDEX sessions_active_account_idx
    ON sessions (account_id, last_used_at DESC)
    WHERE revoked_at IS NULL;

CREATE TABLE email_login_challenges (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email TEXT NOT NULL,
    purpose TEXT NOT NULL,
    initiated_by_session_id UUID REFERENCES sessions (id) ON DELETE CASCADE,
    code_mac BYTEA NOT NULL,
    attempt_count SMALLINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    sent_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    verified_at TIMESTAMPTZ,
    completion_token_hash BYTEA UNIQUE,
    completed_at TIMESTAMPTZ,
    CONSTRAINT email_login_challenges_email_length CHECK (char_length(email) BETWEEN 3 AND 320),
    CONSTRAINT email_login_challenges_email_normalized CHECK (email = lower(email)),
    CONSTRAINT email_login_challenges_purpose_valid
        CHECK (purpose IN ('sign_in', 'link_email', 'change_email')),
    CONSTRAINT email_login_challenges_session_for_change
        CHECK ((purpose = 'sign_in') = (initiated_by_session_id IS NULL)),
    CONSTRAINT email_login_challenges_code_mac_length CHECK (octet_length(code_mac) = 32),
    CONSTRAINT email_login_challenges_attempt_limit CHECK (attempt_count BETWEEN 0 AND 5),
    CONSTRAINT email_login_challenges_expiry_after_creation CHECK (expires_at > created_at),
    CONSTRAINT email_login_challenges_verified_has_token
        CHECK ((verified_at IS NULL) = (completion_token_hash IS NULL)),
    CONSTRAINT email_login_challenges_completed_is_verified
        CHECK (completed_at IS NULL OR verified_at IS NOT NULL),
    CONSTRAINT email_login_challenges_completion_token_length
        CHECK (completion_token_hash IS NULL OR octet_length(completion_token_hash) = 32)
);

CREATE INDEX email_login_challenges_email_created_idx
    ON email_login_challenges (email, created_at DESC);
CREATE INDEX email_login_challenges_expires_idx
    ON email_login_challenges (expires_at);

CREATE TABLE telegram_login_attempts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    state_hash BYTEA NOT NULL UNIQUE,
    -- Encrypt the verifier with AES-GCM in the application; store 12-byte nonce,
    -- ciphertext, and 16-byte authentication tag together.
    pkce_verifier_ciphertext BYTEA NOT NULL,
    nonce TEXT NOT NULL,
    poll_secret_hash BYTEA NOT NULL UNIQUE,
    purpose TEXT NOT NULL,
    initiated_by_session_id UUID REFERENCES sessions (id) ON DELETE CASCADE,
    telegram_subject TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    approved_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    callback_claimed_at TIMESTAMPTZ,
    recovery_ticket_hash BYTEA UNIQUE,
    CONSTRAINT telegram_login_attempts_hash_lengths
        CHECK (octet_length(state_hash) = 32 AND octet_length(poll_secret_hash) = 32),
    CONSTRAINT telegram_login_attempts_pkce_ciphertext_length
        CHECK (octet_length(pkce_verifier_ciphertext) BETWEEN 71 AND 512),
    CONSTRAINT telegram_login_attempts_nonce_length
        CHECK (char_length(nonce) BETWEEN 16 AND 256),
    CONSTRAINT telegram_login_attempts_purpose_valid
        CHECK (purpose IN ('sign_in', 'link_telegram')),
    CONSTRAINT telegram_login_attempts_session_for_link
        CHECK ((purpose = 'sign_in') = (initiated_by_session_id IS NULL)),
    CONSTRAINT telegram_login_attempts_subject_length
        CHECK (telegram_subject IS NULL OR char_length(telegram_subject) BETWEEN 1 AND 512),
    CONSTRAINT telegram_login_attempts_expiry_after_creation CHECK (expires_at > created_at),
    CONSTRAINT telegram_login_attempts_approval_has_subject
        CHECK ((approved_at IS NULL) = (telegram_subject IS NULL)),
    CONSTRAINT telegram_login_attempts_completed_is_approved
        CHECK (completed_at IS NULL OR approved_at IS NOT NULL),
    CONSTRAINT telegram_recovery_ticket_length
        CHECK (recovery_ticket_hash IS NULL OR octet_length(recovery_ticket_hash) = 32),
    CONSTRAINT telegram_recovery_ticket_completed
        CHECK (recovery_ticket_hash IS NULL OR completed_at IS NOT NULL)
);

CREATE INDEX telegram_login_attempts_expires_idx
    ON telegram_login_attempts (expires_at);

-- +goose Down
DROP TABLE telegram_login_attempts;
DROP TABLE email_login_challenges;
DROP TABLE sessions;
DROP TABLE login_identities;
