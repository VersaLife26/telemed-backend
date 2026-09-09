-- The appointment reminder projection gains the quoted price, and a 'pending'
-- status for the window between a booking being created and being paid for.
--
-- Why: the booking_confirmed template renders "Fee: {{.FeeLKR}}". That number
-- used to arrive as a fee_cents field on appointment.confirmed. The canonical
-- events.AppointmentConfirmed does not carry it, and the doctor's CURRENT list
-- price is the wrong substitute -- a patient who booked at LKR 2,000 must be
-- told LKR 2,000 even if the doctor raised their fee ten minutes later.
--
-- The right source is events.AppointmentCreated.AmountCents, which is the
-- quote fixed at booking time and explicitly documented as such. So this
-- service now consumes appointment.created to record the quote, and
-- appointment.confirmed to confirm it.
ALTER TABLE appointment_reminder_state
    ADD COLUMN IF NOT EXISTS amount_cents BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS currency     TEXT   NOT NULL DEFAULT 'LKR';

-- 'pending' is a booking that exists but has not been paid for. The reminder
-- cron's partial index is on status = 'confirmed', so pending rows stay
-- invisible to it and no reminder is ever sent for an unpaid booking.
ALTER TABLE appointment_reminder_state
    DROP CONSTRAINT IF EXISTS appointment_reminder_state_status_check;

ALTER TABLE appointment_reminder_state
    ADD CONSTRAINT appointment_reminder_state_status_check
    CHECK (status IN ('pending', 'confirmed', 'cancelled', 'completed', 'no_show'));
