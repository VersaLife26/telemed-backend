-- Reverse of 000005_doctor_pricing_and_appointment_amount.up.sql.
--
-- This DROPS the quote recorded against every appointment. That is the honest
-- reverse of the up migration, but it destroys the evidence of what each
-- patient was told they would pay, so run it only on a database whose
-- appointments you are prepared to lose the pricing history for.
DROP INDEX IF EXISTS idx_appointments_amount;

ALTER TABLE appointments DROP CONSTRAINT IF EXISTS appointments_amount_currency_chk;
ALTER TABLE appointments DROP CONSTRAINT IF EXISTS appointments_currency_chk;
ALTER TABLE appointments DROP CONSTRAINT IF EXISTS appointments_amount_chk;

ALTER TABLE appointments
    DROP COLUMN IF EXISTS specialty,
    DROP COLUMN IF EXISTS currency,
    DROP COLUMN IF EXISTS amount_cents;

DROP INDEX IF EXISTS idx_doctor_pricing_status;
DROP TABLE IF EXISTS doctor_pricing;
