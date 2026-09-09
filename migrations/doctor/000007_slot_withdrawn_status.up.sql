-- doctor_slot_state gains a third status: WITHDRAWN.
--
-- WHY IT IS NEEDED
-- scheduling-service now publishes slot.withdrawn when a doctor registers leave
-- over a day whose slots were already generated. That is a different fact from
-- slot.released -- released means "bookable again", withdrawn means "never
-- again" -- and this projection has to be able to record the difference.
--
-- Without it, a slot that had entered this mirror as AVAILABLE (because it was
-- released by an earlier cancellation) would stay AVAILABLE forever, and doctor
-- search would keep advertising a doctor as available on the exact day they are
-- on leave. Mapping withdrawal onto BOOKED would hide it from search too, but
-- it would be a lie in a table an operator reads during an incident.
--
-- Deleting the row instead was considered and rejected: last_event_id /
-- last_event_at are what make this projection converge under at-least-once,
-- unordered delivery, and a deleted row loses that guard entirely. A late
-- redelivery of the slot.booked that preceded the withdrawal would re-insert
-- the slot as if nothing had happened.
--
-- doctor_availability_summary needs no change: its recompute already counts
-- only rows with status = 'AVAILABLE', so a WITHDRAWN row drops out of the
-- count and out of next_available_at automatically.
ALTER TABLE doctor_slot_state
    DROP CONSTRAINT IF EXISTS doctor_slot_state_status_check;

ALTER TABLE doctor_slot_state
    ADD CONSTRAINT doctor_slot_state_status_check
    CHECK (status IN ('AVAILABLE', 'BOOKED', 'WITHDRAWN'));
