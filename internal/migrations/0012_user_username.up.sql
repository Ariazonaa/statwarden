-- 0012_user_username.up.sql — give every account a real, unique login username.
-- Until now users authenticated by e-mail only; the login form said
-- "Benutzername" but only the address worked. This adds a first-class username
-- that a login may use interchangeably with the e-mail address.
--
-- Every existing row is populated BEFORE the NOT NULL and the unique index go
-- on, so the backfill cannot fail either constraint:
--   1. slugify display_name (lowercased, keep [a-z0-9], drop everything else),
--   2. fall back to the e-mail local part slugified the same way,
--   3. fall back to the literal 'user' when both are empty,
--   4. de-duplicate collisions by appending '-' and the row's own (unique) id.
--      A slug never contains '-' (it is stripped in step 1/2), so a suffixed
--      value can never equal a plain slug, and the id makes suffixed values
--      unique among themselves — the unique index is therefore guaranteed to
--      hold for any pre-existing data.
-- The index mirrors users_email_lower_idx: case-insensitive, so 'Admin' and
-- 'admin' are the same login and cannot both exist.

BEGIN;

ALTER TABLE users ADD COLUMN username text;

WITH slugged AS (
    SELECT
        id,
        COALESCE(
            NULLIF(regexp_replace(lower(display_name), '[^a-z0-9]+', '', 'g'), ''),
            NULLIF(regexp_replace(lower(split_part(email, '@', 1)), '[^a-z0-9]+', '', 'g'), ''),
            'user'
        ) AS base
    FROM users
),
counted AS (
    SELECT id, base, count(*) OVER (PARTITION BY base) AS dupes
    FROM slugged
)
UPDATE users u
SET username = CASE
        WHEN c.dupes > 1 THEN c.base || '-' || c.id::text
        ELSE c.base
    END
FROM counted c
WHERE u.id = c.id;

ALTER TABLE users ALTER COLUMN username SET NOT NULL;

CREATE UNIQUE INDEX users_username_lower_idx ON users (lower(username));

COMMIT;
