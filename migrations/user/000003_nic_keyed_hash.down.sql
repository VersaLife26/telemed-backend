-- Reverse 000003_nic_keyed_hash.
--
-- This removes the constraints and the indexes, restoring the column to the
-- unconstrained TEXT it was before. It does NOT restore the bcrypt digests the
-- up-migration destroyed, and it cannot: bcrypt is one-way and the plaintext
-- NIC was never stored, so there is no input from which to recompute them.
-- That is stated here rather than discovered at 3am -- rolling this back
-- returns the SCHEMA to its previous shape, not the data.
--
-- Rolling back also does not reintroduce the vulnerability by itself, because
-- the values that carried it are already gone. What it does reintroduce is the
-- ability to WRITE a bcrypt digest into the column again, which is why the
-- constraint exists in the first place.

DROP INDEX IF EXISTS idx_users_nic_hash;
DROP INDEX IF EXISTS idx_family_members_nic_hash;

ALTER TABLE family_members DROP CONSTRAINT IF EXISTS family_members_nic_hash_is_keyed_digest;
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_nic_hash_is_keyed_digest;

COMMENT ON COLUMN users.nic_hash IS NULL;
COMMENT ON COLUMN family_members.nic_hash IS NULL;
