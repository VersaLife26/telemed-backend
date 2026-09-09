-- doctor_schedule_settings: the slot-shape preferences that turn a doctor's
-- declared working hours into bookable slots.
--
-- These belong here, not in scheduling-service, because they are a DECLARATION
-- the doctor makes about their own practice -- "I see patients for 20 minutes
-- with 10 minutes between" -- in the same way working_hours is. scheduling-
-- service holds a projection of them, fed by doctor.approved and
-- doctor.updated, and materialises the calendar from it (ADR-004: one owner,
-- events across the seam, no cross-service join).
--
-- Until this table existed, the doctor app's availability editor sent
-- slot_duration_minutes, buffer_minutes and max_per_day and doctor-service's
-- DTO accepted none of them. httpx.DecodeJSON rejects unknown fields, so the
-- save did not degrade -- it 400'd outright, and the availability editor is the
-- screen everything downstream depends on: no availability, no slots; no slots,
-- no bookings.
CREATE TABLE IF NOT EXISTS doctor_schedule_settings (
    doctor_id             UUID PRIMARY KEY REFERENCES doctors (id) ON DELETE CASCADE,

    -- How long one consultation is booked for.
    slot_duration_minutes INT         NOT NULL DEFAULT 30
                              CHECK (slot_duration_minutes BETWEEN 5 AND 240),

    -- NULLABLE ON PURPOSE, and it must stay that way end to end.
    --
    -- NULL means "this doctor has expressed no preference" and the consumer
    -- keeps its own default. 0 means "back-to-back, no gap at all", which is a
    -- real thing a busy clinic asks for. Collapsing the two -- treating 0 as
    -- unset -- silently hands that doctor the 5-minute default forever, and
    -- nothing anywhere reports it. The column is nullable, the Go field is a
    -- *int, and the event tag is omitempty so an absent preference is absent on
    -- the wire rather than transmitted as a zero.
    buffer_minutes        INT
                              CHECK (buffer_minutes IS NULL OR buffer_minutes BETWEEN 0 AND 120),

    -- 0 means no cap.
    max_per_day           INT         NOT NULL DEFAULT 0
                              CHECK (max_per_day BETWEEN 0 AND 100),

    -- The wall-clock zone the working hours are expressed in. Stored as an IANA
    -- name, never a fixed offset: Sri Lanka's +05:30 is stable today and that
    -- is not a promise anyone made about 2041.
    timezone              TEXT        NOT NULL DEFAULT 'Asia/Colombo',

    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
