-- name: GetUserByEmail :one
SELECT id, email, password_hash, display_name, role, totp_secret, totp_enabled,
       backup_codes, avatar_path, locale, is_active, created_at, updated_at
FROM users
WHERE lower(email) = lower($1);
