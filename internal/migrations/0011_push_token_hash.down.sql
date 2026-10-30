-- Irreversible data transform: the plaintext push tokens were replaced by their
-- SHA-256 hash, which cannot be reversed. This down migration only restores the
-- column and index names; the stored values remain hashes, so operators must
-- regenerate push tokens after a rollback.
ALTER INDEX monitors_push_token_hash_idx RENAME TO monitors_push_token_idx;
ALTER TABLE monitors RENAME COLUMN push_token_hash TO push_token;
