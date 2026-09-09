-- Any 'pending' row must go before the narrower constraint can be restored;
-- an unpaid booking has produced no notification, so dropping it loses
-- nothing a replay cannot rebuild.
DELETE FROM appointment_reminder_state WHERE status = 'pending';

ALTER TABLE appointment_reminder_state
    DROP CONSTRAINT IF EXISTS appointment_reminder_state_status_check;

ALTER TABLE appointment_reminder_state
    ADD CONSTRAINT appointment_reminder_state_status_check
    CHECK (status IN ('confirmed', 'cancelled', 'completed', 'no_show'));

ALTER TABLE appointment_reminder_state
    DROP COLUMN IF EXISTS currency,
    DROP COLUMN IF EXISTS amount_cents;
