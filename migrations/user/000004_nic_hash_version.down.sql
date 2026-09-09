-- Reverse 000004_nic_hash_version.
--
-- This drops the version column and restores the single-column indexes.
--
-- Read this before running it: the digests SURVIVE, and after this rollback
-- nothing records which pepper produced them. If the deployment being rolled
-- back had already rotated, that provenance is not recoverable from the data
-- -- the digests are indistinguishable from each other. Rotate back to the
-- original pepper before rolling this back, or accept that every row written
-- under a later generation is now unverifiable.
--
-- The indexes are restored to their 000003 shape so a rollback leaves the
-- schema exactly as 000003 left it, not merely close to it.

DROP INDEX IF EXISTS idx_users_nic_hash_versioned;
DROP INDEX IF EXISTS idx_family_members_nic_hash_versioned;

ALTER TABLE family_members DROP CONSTRAINT IF EXISTS family_members_nic_hash_version_travels_with_digest;
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_nic_hash_version_travels_with_digest;

ALTER TABLE family_members DROP COLUMN IF EXISTS nic_hash_version;
ALTER TABLE users DROP COLUMN IF EXISTS nic_hash_version;

CREATE INDEX IF NOT EXISTS idx_family_members_nic_hash
    ON family_members (nic_hash) WHERE nic_hash IS NOT NULL AND deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_users_nic_hash
    ON users (nic_hash) WHERE nic_hash IS NOT NULL AND deleted_at IS NULL;
