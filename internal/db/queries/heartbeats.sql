-- name: InsertHeartbeat :one
INSERT INTO heartbeats (monitor_id, checked_at, status, latency_ms, message, important, retries, extra)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id;

-- name: DeleteHeartbeatsBefore :execrows
-- Retention pruning: drop every heartbeat recorded strictly before the cutoff.
-- Returns the number of rows deleted so the job can log its work.
DELETE FROM heartbeats
WHERE checked_at < $1;
