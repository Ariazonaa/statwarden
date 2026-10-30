-- 0003_api_keys.down.sql — drop the API keys table.
BEGIN;
DROP TABLE IF EXISTS api_keys;
COMMIT;
