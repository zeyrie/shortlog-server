-- +goose Up
CREATE TABLE auth_rate_limits (
    key BYTEA PRIMARY KEY CHECK (octet_length(key) = 32),
    window_start TIMESTAMPTZ NOT NULL DEFAULT now(),
    request_count INTEGER NOT NULL DEFAULT 1 CHECK (request_count > 0)
);
CREATE INDEX auth_rate_limits_window_idx ON auth_rate_limits (window_start);

-- +goose Down
DROP TABLE auth_rate_limits;
