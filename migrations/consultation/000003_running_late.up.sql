-- When a live consult runs past its booked end, notify the doctor's next
-- waiting patient once. scheduled_end_at is the booked slot end (from
-- appointment.confirmed); running_late_notified_at throttles the courtesy ping.

ALTER TABLE consultations
    ADD COLUMN IF NOT EXISTS scheduled_end_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS running_late_notified_at TIMESTAMPTZ;

UPDATE consultations
SET scheduled_end_at = scheduled_at + INTERVAL '15 minutes'
WHERE scheduled_end_at IS NULL;

ALTER TABLE consultations
    ALTER COLUMN scheduled_end_at SET NOT NULL;

ALTER TABLE consultations
    DROP CONSTRAINT IF EXISTS ck_consultations_scheduled_range;
ALTER TABLE consultations
    ADD CONSTRAINT ck_consultations_scheduled_range
        CHECK (scheduled_end_at > scheduled_at);

CREATE INDEX IF NOT EXISTS idx_consultations_running_late
    ON consultations (scheduled_end_at)
    WHERE status = 'active'
      AND deleted_at IS NULL
      AND running_late_notified_at IS NULL;
