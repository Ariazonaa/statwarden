-- name: ListActiveMonitors :many
SELECT id, name, type, config, interval_seconds, timeout_seconds, max_retries,
       retry_interval_seconds, resend_interval_minutes, upside_down, active,
       status, parent_id, push_token_hash, description, link_url, created_by,
       created_at, updated_at
FROM monitors
WHERE active = true
ORDER BY id;

-- name: UpdateMonitorStatus :exec
UPDATE monitors
SET status = $2, updated_at = now()
WHERE id = $1;
