-- Replace the bcrypt NIC hash with a keyed HMAC-SHA256 digest.
--
-- WHY THE OLD VALUES ARE DESTROYED RATHER THAN MIGRATED
--
-- They cannot be converted. bcrypt is one-way and the plaintext NIC was never
-- stored, so there is no input from which to compute the new digest. That
-- leaves two options, and only one of them is defensible:
--
--   (a) leave the bcrypt digests in place until each row is next edited, or
--   (b) delete them now and require the NIC to be re-entered.
--
-- (a) keeps the vulnerability. A Sri Lankan NIC encodes its own birth date --
-- old format YYDDDNNNNC, new format YYYYDDDNNNNN -- and `family_members`
-- stores the plaintext `dob` in the SAME ROW. The year and day-of-year are
-- therefore known to anyone holding a dump, and what is left to guess is a
-- 4-5 digit serial: on the order of 10^4-10^5 candidates, which is seconds on
-- a GPU regardless of bcrypt's cost factor. Every row left under the old
-- scheme stays recoverable forever, and the table only grows.
--
-- (b) costs a dependant profile its NIC until someone re-enters it. `nic_hash`
-- is nullable, no code path requires it, and `has_nic` on the API already
-- reports its absence honestly. A patient re-typing a number is a smaller harm
-- than a permanent, undetectable disclosure of every dependant's national
-- identity number.
--
-- So: (b). This is destructive and deliberate.

-- users.nic_hash is written by no code path today (Repository.CreateUser
-- passes it through, and nothing ever populates it), so this is expected to
-- affect zero rows there. It is included anyway rather than assumed: the point
-- of the statement is that no value under the old scheme survives, and an
-- assumption is not a guarantee.
UPDATE users
   SET nic_hash  = NULL,
       updated_at = NOW()
 WHERE nic_hash IS NOT NULL;

UPDATE family_members
   SET nic_hash  = NULL,
       updated_at = NOW()
 WHERE nic_hash IS NOT NULL;

-- The stored form is now lowercase hex of a SHA-256 HMAC: exactly 64 hex
-- characters. The CHECK is the structural half of the fix -- a bcrypt digest
-- starts "$2a$" and can never be written into this column again, no matter
-- what a future refactor does to the Go side. A control the database enforces
-- outlives the comment that explains it.
ALTER TABLE users
    ADD CONSTRAINT users_nic_hash_is_keyed_digest
    CHECK (nic_hash IS NULL OR nic_hash ~ '^[0-9a-f]{64}$');

ALTER TABLE family_members
    ADD CONSTRAINT family_members_nic_hash_is_keyed_digest
    CHECK (nic_hash IS NULL OR nic_hash ~ '^[0-9a-f]{64}$');

-- The digest is deterministic now, which is the whole point: equality is the
-- only question ever asked of this column ("is this the NIC on file", "has
-- this NIC been seen before"). A random per-row salt could not answer the
-- second at all, which is why one NIC could previously register unlimited
-- dependants.
--
-- The index is NOT unique, and that is a decision rather than an omission. Two
-- rows legitimately share a NIC: a child listed by both parents' accounts is
-- the common Sri Lankan case this table exists for, and a UNIQUE constraint
-- would reject the second parent with a database error at profile-creation
-- time. Making duplicates *detectable* is a schema concern; deciding what to
-- do about one is a policy concern and belongs in the service.
CREATE INDEX IF NOT EXISTS idx_family_members_nic_hash
    ON family_members (nic_hash) WHERE nic_hash IS NOT NULL AND deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_users_nic_hash
    ON users (nic_hash) WHERE nic_hash IS NOT NULL AND deleted_at IS NULL;

COMMENT ON COLUMN users.nic_hash IS
    'HMAC-SHA256(NIC, NIC_HASH_PEPPER), lowercase hex. The pepper is held outside the database (env/KMS). Never bcrypt: the NIC encodes its own birth date and dob sits in the same row, so the search space is ~10^4 and a work factor buys nothing.';

COMMENT ON COLUMN family_members.nic_hash IS
    'HMAC-SHA256(NIC, NIC_HASH_PEPPER), lowercase hex. See users.nic_hash. Deterministic on purpose so duplicate NICs are detectable; the index is deliberately not unique because a dependant can be listed by two parents.';
