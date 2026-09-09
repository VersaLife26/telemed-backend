-- Reverse of 000003_fee_cents_and_currency.up.sql.
--
-- The column rename is reversed exactly. No value rescaling happens in either
-- direction, because none happened on the way up: the column was always cents.
CREATE INDEX IF NOT EXISTS idx_doctors_search_specialty
    ON doctors (specialty, fee_cents)
    WHERE verification_status = 'approved' AND deleted_at IS NULL;

DROP INDEX IF EXISTS idx_doctors_search_specialty_fee;

ALTER TABLE doctors DROP CONSTRAINT IF EXISTS doctors_currency_chk;
ALTER TABLE doctors DROP COLUMN IF EXISTS currency;

ALTER TABLE doctors RENAME CONSTRAINT doctors_fee_cents_check TO doctors_fee_lkr_check;
ALTER TABLE doctors RENAME COLUMN fee_cents TO fee_lkr;
