-- name: UpsertCertInfo :exec
INSERT INTO cert_info (monitor_id, subject, issuer, serial, fingerprint_sha256,
                       valid_from, valid_to, days_remaining, details, checked_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
ON CONFLICT (monitor_id) DO UPDATE
SET subject            = EXCLUDED.subject,
    issuer             = EXCLUDED.issuer,
    serial             = EXCLUDED.serial,
    fingerprint_sha256 = EXCLUDED.fingerprint_sha256,
    valid_from         = EXCLUDED.valid_from,
    valid_to           = EXCLUDED.valid_to,
    days_remaining     = EXCLUDED.days_remaining,
    details            = EXCLUDED.details,
    checked_at         = now();

-- name: ListCertInfo :many
SELECT c.monitor_id, m.name AS monitor_name, c.issuer, c.valid_to
FROM cert_info c
JOIN monitors m ON m.id = c.monitor_id
WHERE m.active = true AND c.valid_to IS NOT NULL
ORDER BY c.valid_to;
