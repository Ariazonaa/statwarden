-- name: ListMonitorAssignments :many
SELECT n.id, n.name, n.type, n.config, n.is_default, n.active, n.templates,
       mn.escalation_after_minutes
FROM monitor_notifications mn
JOIN notifications n ON n.id = mn.notification_id
WHERE mn.monitor_id = $1 AND n.active = true
ORDER BY mn.escalation_after_minutes, n.id;

-- name: GetMonitorNotifySettings :one
SELECT id, name, link_url, config, resend_interval_minutes
FROM monitors
WHERE id = $1;

-- name: InsertNotificationDelivery :exec
INSERT INTO notification_deliveries (notification_id, monitor_id, provider_type,
                                     event_kind, status, error, attempts,
                                     started_at, finished_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);
