-- Make the consultation fee's unit unambiguous, and give it an explicit
-- currency.
--
-- WHAT WAS FOUND: `fee_lkr` was ALREADY in cents. The column comment, the Go
-- field comment, openapi.yaml ("LKR cents"), docs/API.md ("LKR 5,000.00 =
-- 500000") and the gRPC field comment all agree. No value migration is needed
-- and none is performed here -- rescaling would corrupt every existing row.
--
-- WHAT WAS WRONG: only the name. `fee_lkr` reads as rupees to anyone who has
-- not read the comment, and money bugs are exactly the class of bug where
-- "read the comment" is not a control. The platform stores money as BIGINT
-- cents everywhere else (payments.amount_cents, commission_cents, payout_cents),
-- so this column now says what it holds.
ALTER TABLE doctors RENAME COLUMN fee_lkr TO fee_cents;

-- The inline CHECK (fee_lkr >= 0) was auto-named by Postgres and survives the
-- column rename with a stale name. Rename it too, so \d doctors does not still
-- say "lkr".
ALTER TABLE doctors RENAME CONSTRAINT doctors_fee_lkr_check TO doctors_fee_cents_check;

-- Currency is a separate column, never encoded in the amount column's name.
-- The platform is LKR-only today; a doctor consulting from Dubai billing AED is
-- a pricing decision, not a schema migration.
ALTER TABLE doctors
    ADD COLUMN IF NOT EXISTS currency CHAR(3) NOT NULL DEFAULT 'LKR';

ALTER TABLE doctors
    ADD CONSTRAINT doctors_currency_chk CHECK (currency ~ '^[A-Z]{3}$');

-- Recreate the search index against the new column name. CREATE first, then
-- DROP, so the search hot path is never without an index mid-migration.
CREATE INDEX IF NOT EXISTS idx_doctors_search_specialty_fee
    ON doctors (specialty, fee_cents)
    WHERE verification_status = 'approved' AND deleted_at IS NULL;

DROP INDEX IF EXISTS idx_doctors_search_specialty;
