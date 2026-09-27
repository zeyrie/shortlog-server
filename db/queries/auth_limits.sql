-- name: CountAuthRequests :one
INSERT INTO auth_rate_limits (key) VALUES ($1)
ON CONFLICT (key) DO UPDATE SET
    request_count = CASE WHEN auth_rate_limits.window_start <= now() - interval '1 hour'
                         THEN 1 ELSE auth_rate_limits.request_count + 1 END,
    window_start = CASE WHEN auth_rate_limits.window_start <= now() - interval '1 hour'
                        THEN now() ELSE auth_rate_limits.window_start END
RETURNING request_count;

-- name: PruneAuthLimits :exec
DELETE FROM auth_rate_limits WHERE window_start < now() - interval '2 hours';
