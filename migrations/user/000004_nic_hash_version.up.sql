-- Give every stored NIC digest a key version.
--
-- WHY THIS EXISTS
--
-- `nic_hash` is HMAC-SHA256(NIC, NIC_HASH_PEPPER). A keyed digest only means
-- anything next to the key that produced it, and the pepper is a secret with a
-- lifecycle: it lives in the environment, it leaks like any other environment
-- variable, and sooner or later it gets rotated.
--
-- Before this migration a rotation destroyed every stored digest, and did it
-- SILENTLY. Every value in the column still looked exactly right -- 64 hex
-- characters, passing its CHECK -- while matching nothing. Duplicate detection
-- would report "no duplicate"; a NIC confirmation would report "does not
-- match". Both are the *legitimate* answers to those questions, so nothing
-- would error, nothing would log, and the loss would surface as a slow drift
-- in data quality months later. There was no query that could even ask how
-- many rows were affected.
--
-- Storing the version turns that into an ordinary operation: old rows are
-- verifiable while the old pepper is still held (NIC_HASH_PEPPER_PREVIOUS),
-- rows still to be rewritten are countable, and a row whose version this
-- deployment has no pepper for is DISTINGUISHABLE from a row whose NIC simply
-- does not match. See internal/user/nic.go for the rotation procedure.
--
-- This lands before anything reads the digest for equality. Adding it after
-- would mean backfilling a version onto rows whose provenance is no longer
-- knowable -- which is to say, guessing.

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS nic_hash_version SMALLINT;

ALTER TABLE family_members
    ADD COLUMN IF NOT EXISTS nic_hash_version SMALLINT;

-- Backfill. Migration 000003 nulled every nic_hash, and nothing has written
-- one since under any pepper but the first, so every non-null digest in the
-- table is version 1 by construction. Written as a statement rather than
-- assumed, for the same reason 000003 nulled `users.nic_hash` it believed to
-- be empty: an assumption is not a guarantee.
UPDATE users
   SET nic_hash_version = 1,
       updated_at       = NOW()
 WHERE nic_hash IS NOT NULL AND nic_hash_version IS NULL;

UPDATE family_members
   SET nic_hash_version = 1,
       updated_at       = NOW()
 WHERE nic_hash IS NOT NULL AND nic_hash_version IS NULL;

-- The digest and its version travel together or not at all.
--
-- This is the whole point of the migration, and it is enforced here rather
-- than in Go because a Go-side invariant lasts until the next refactor. A
-- digest with no version is a value nobody can ever verify again; a version
-- with no digest is a lie about a column that is empty. The database refuses
-- both.
--
-- Version must be positive: 0 is what an unset integer looks like, and "unset"
-- must never be readable as "version 0".
ALTER TABLE users
    ADD CONSTRAINT users_nic_hash_version_travels_with_digest
    CHECK (
        (nic_hash IS NULL     AND nic_hash_version IS NULL)
     OR (nic_hash IS NOT NULL AND nic_hash_version IS NOT NULL AND nic_hash_version > 0)
    );

ALTER TABLE family_members
    ADD CONSTRAINT family_members_nic_hash_version_travels_with_digest
    CHECK (
        (nic_hash IS NULL     AND nic_hash_version IS NULL)
     OR (nic_hash IS NOT NULL AND nic_hash_version IS NOT NULL AND nic_hash_version > 0)
    );

-- Equality on this column is always scoped to a version -- a digest from
-- pepper generation 1 and one from generation 2 are different functions of
-- possibly the same NIC, and comparing them across versions is meaningless.
-- Index the pair so the query that duplicate detection will actually run
-- ("this digest, at this version") is a single index probe, and so counting
-- rows still awaiting a rewrite during a rotation is cheap.
DROP INDEX IF EXISTS idx_family_members_nic_hash;
DROP INDEX IF EXISTS idx_users_nic_hash;

CREATE INDEX IF NOT EXISTS idx_family_members_nic_hash_versioned
    ON family_members (nic_hash_version, nic_hash) WHERE nic_hash IS NOT NULL AND deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_users_nic_hash_versioned
    ON users (nic_hash_version, nic_hash) WHERE nic_hash IS NOT NULL AND deleted_at IS NULL;

COMMENT ON COLUMN users.nic_hash_version IS
    'Which NIC_HASH_PEPPER generation produced users.nic_hash. Written together with the digest, enforced by CHECK. Rotating the pepper without bumping this silently invalidates every stored digest with no error and no log line.';

COMMENT ON COLUMN family_members.nic_hash_version IS
    'Which NIC_HASH_PEPPER generation produced family_members.nic_hash. See users.nic_hash_version.';
