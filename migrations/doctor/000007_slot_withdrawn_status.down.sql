-- Reverse of 000007. Any WITHDRAWN row would violate the narrower constraint,
-- so they are folded back to BOOKED first: both are "not bookable", which is
-- the only distinction the summary recompute makes, so the projection stays
-- correct across the rollback even though it loses the reason.
UPDATE doctor_slot_state SET status = 'BOOKED', updated_at = NOW()
WHERE status = 'WITHDRAWN';

ALTER TABLE doctor_slot_state
    DROP CONSTRAINT IF EXISTS doctor_slot_state_status_check;

ALTER TABLE doctor_slot_state
    ADD CONSTRAINT doctor_slot_state_status_check
    CHECK (status IN ('AVAILABLE', 'BOOKED'));
