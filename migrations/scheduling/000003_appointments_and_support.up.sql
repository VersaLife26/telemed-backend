-- ---------------------------------------------------------------------------
-- appointments
-- ---------------------------------------------------------------------------
-- patient_id and doctor_id are UUIDs that *mean* rows in telemed_user and
-- telemed_doctor. There is deliberately no foreign key: those tables live in
-- other databases owned by other services (ADR-004).
CREATE TABLE IF NOT EXISTS appointments (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    patient_id          UUID        NOT NULL,
    doctor_id           UUID        NOT NULL,

    -- The final backstop against double-booking. See the partial unique index
    -- below: uniqueness is scoped to *live* appointments so that cancelling
    -- one genuinely returns the slot to the market.
    slot_id             UUID        NOT NULL,

    -- Denormalised from slots. Two reasons, both load-bearing:
    --   1. start_at is the slots partition key, so carrying it lets every
    --      write back to slots prune to a single partition.
    --   2. reminder, cancellation-window and "my appointments" queries never
    --      need to touch the partitioned table at all.
    slot_start_at       TIMESTAMPTZ NOT NULL,
    slot_end_at         TIMESTAMPTZ NOT NULL,

    status              TEXT        NOT NULL DEFAULT 'pending_payment',
    intake              JSONB       NOT NULL DEFAULT '{}'::jsonb,

    -- The consultation is for a dependant (a child, an elderly parent) rather
    -- than the account holder. The id means a row in telemed_user's
    -- family_members; the booking, the payment and the no-show still belong to
    -- the account holder, which is why the overlap guard keys on patient_id.
    family_member_id    UUID,

    -- Set when the patient's historical no-show rate exceeded the policy
    -- threshold at booking time. The payment service reads this off
    -- appointment.created and refuses pay-on-completion flows for the booking.
    prepayment_required BOOLEAN     NOT NULL DEFAULT FALSE,

    payment_id          UUID,
    confirmed_at        TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ,
    no_show_at          TIMESTAMPTZ,
    cancelled_at        TIMESTAMPTZ,
    cancelled_by        UUID,
    cancelled_by_role   TEXT,
    cancellation_reason TEXT,
    -- FULL | PARTIAL | NONE. Computed from the cancellation policy at cancel
    -- time and published on appointment.cancelled; the payment service owns
    -- the money, this service owns the signal.
    refund_policy       TEXT,

    version             INT         NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at          TIMESTAMPTZ,

    CONSTRAINT appointments_status_chk CHECK (
        status IN ('pending_payment', 'confirmed', 'cancelled', 'completed', 'no_show')
    ),
    CONSTRAINT appointments_refund_chk CHECK (
        refund_policy IS NULL OR refund_policy IN ('FULL', 'PARTIAL', 'NONE')
    ),
    CONSTRAINT appointments_cancel_chk CHECK (
        (status = 'cancelled') = (cancelled_at IS NOT NULL)
    ),
    CONSTRAINT appointments_slot_range_chk CHECK (slot_end_at > slot_start_at)
);

-- THE BACKSTOP.
--
-- The source documentation writes this as a plain `slot_id UNIQUE`. That is a
-- bug: once an appointment is cancelled the row stays, so the slot it points
-- at can never be booked again -- the cancellation path frees the slot in
-- `slots` and then the next booker hits 23505 forever. Scoping uniqueness to
-- live statuses keeps the guarantee ("at most one live appointment per slot")
-- while letting a cancelled slot re-enter the market.
CREATE UNIQUE INDEX IF NOT EXISTS uq_appointments_slot_live
    ON appointments (slot_id)
    WHERE status IN ('pending_payment', 'confirmed', 'completed', 'no_show');

CREATE INDEX IF NOT EXISTS idx_appointments_patient
    ON appointments (patient_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_appointments_doctor_start
    ON appointments (doctor_id, slot_start_at DESC);

-- Drives the unpaid-booking expiry sweeper and the no-show detector.
CREATE INDEX IF NOT EXISTS idx_appointments_status_start
    ON appointments (status, slot_start_at);

CREATE INDEX IF NOT EXISTS idx_appointments_slot
    ON appointments (slot_id);

-- ---------------------------------------------------------------------------
-- doctor_schedule_settings
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS doctor_schedule_settings (
    doctor_id            UUID        PRIMARY KEY,
    slot_duration_minutes INT        NOT NULL DEFAULT 15,
    buffer_minutes        INT        NOT NULL DEFAULT 5,
    max_per_day           INT        NOT NULL DEFAULT 24,
    -- Per-doctor because a doctor consulting from Dubai still keeps Colombo
    -- clinic hours, or vice versa. Resolved through tzdata, never an offset.
    timezone              TEXT       NOT NULL DEFAULT 'Asia/Colombo',
    -- 0 normally; raised to 10 by the nightly job for doctors whose patients
    -- no-show heavily (the Mend overbooking model). See docs/DESIGN.md §5.
    overbooking_percent   INT        NOT NULL DEFAULT 0,
    -- How far ahead the generator materialises slots.
    advance_days          INT        NOT NULL DEFAULT 30,
    is_active             BOOLEAN    NOT NULL DEFAULT TRUE,

    version               INT        NOT NULL DEFAULT 0,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at            TIMESTAMPTZ,

    CONSTRAINT dss_duration_chk    CHECK (slot_duration_minutes BETWEEN 5 AND 240),
    CONSTRAINT dss_buffer_chk      CHECK (buffer_minutes BETWEEN 0 AND 120),
    CONSTRAINT dss_max_per_day_chk CHECK (max_per_day BETWEEN 1 AND 200),
    CONSTRAINT dss_overbook_chk    CHECK (overbooking_percent BETWEEN 0 AND 50),
    CONSTRAINT dss_advance_chk     CHECK (advance_days BETWEEN 1 AND 180)
);

-- ---------------------------------------------------------------------------
-- working_hours -- mirrored from doctor-service via the doctor.approved event
-- ---------------------------------------------------------------------------
-- This is a read model. It is never edited here; doctor-service owns the truth
-- and republishes on change. Storing TIME (not TIMESTAMPTZ) is correct: these
-- are wall-clock clinic hours in the doctor's timezone, not instants.
CREATE TABLE IF NOT EXISTS working_hours (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    doctor_id    UUID        NOT NULL,
    day_of_week  SMALLINT    NOT NULL,   -- 0 = Sunday .. 6 = Saturday, matching Go's time.Weekday
    start_time   TIME        NOT NULL,
    end_time     TIME        NOT NULL,
    is_available BOOLEAN     NOT NULL DEFAULT TRUE,

    version      INT         NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at   TIMESTAMPTZ,

    CONSTRAINT working_hours_uq       UNIQUE (doctor_id, day_of_week, start_time),
    CONSTRAINT working_hours_dow_chk  CHECK (day_of_week BETWEEN 0 AND 6),
    CONSTRAINT working_hours_span_chk CHECK (end_time > start_time)
);

CREATE INDEX IF NOT EXISTS idx_working_hours_doctor ON working_hours (doctor_id, day_of_week);

-- ---------------------------------------------------------------------------
-- holidays
-- ---------------------------------------------------------------------------
-- doctor_id NULL means a platform-wide holiday (Poya days, Independence Day).
-- NULLS NOT DISTINCT (Postgres 15+) is what makes the unique constraint
-- actually stop two rows both claiming "2026-04-14, everyone".
CREATE TABLE IF NOT EXISTS holidays (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    doctor_id    UUID,
    holiday_date DATE        NOT NULL,
    reason       TEXT        NOT NULL DEFAULT '',

    version      INT         NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at   TIMESTAMPTZ,

    CONSTRAINT holidays_uq UNIQUE NULLS NOT DISTINCT (doctor_id, holiday_date)
);

CREATE INDEX IF NOT EXISTS idx_holidays_date ON holidays (holiday_date);

-- ---------------------------------------------------------------------------
-- waitlists
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS waitlists (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    patient_id      UUID        NOT NULL,
    doctor_id       UUID        NOT NULL,
    preferred_date  DATE        NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'waiting',
    notified_at     TIMESTAMPTZ,
    offered_slot_id UUID,
    offer_expires_at TIMESTAMPTZ,
    -- How many offers this entry has been given and let lapse. Used to drop an
    -- entry that is clearly not watching their phone.
    offer_count     INT         NOT NULL DEFAULT 0,
    -- When this entry last entered the waiting state. Equal to created_at until
    -- an offer lapses, at which point the entry goes to the back of the queue
    -- rather than being re-offered instantly forever. It is also the Redis
    -- sorted-set score, so the index and the table order identically.
    queued_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    version         INT         NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,

    CONSTRAINT waitlists_status_chk CHECK (
        status IN ('waiting', 'notified', 'booked', 'expired', 'cancelled')
    )
);

-- One live entry per patient per doctor per day. Without this a patient who
-- taps "join waitlist" three times gets promoted three times.
CREATE UNIQUE INDEX IF NOT EXISTS uq_waitlists_live
    ON waitlists (patient_id, doctor_id, preferred_date)
    WHERE status IN ('waiting', 'notified');

-- The promotion query: longest-waiting entry for a doctor on a date.
CREATE INDEX IF NOT EXISTS idx_waitlists_queue
    ON waitlists (doctor_id, preferred_date, queued_at)
    WHERE status = 'waiting';

CREATE INDEX IF NOT EXISTS idx_waitlists_patient ON waitlists (patient_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_waitlists_offer_expiry
    ON waitlists (offer_expires_at)
    WHERE status = 'notified';

-- ---------------------------------------------------------------------------
-- no_show_stats
-- ---------------------------------------------------------------------------
-- Per-patient counters. Denormalised deliberately: the prepayment decision sits
-- on the booking hot path and must not aggregate the appointments table.
CREATE TABLE IF NOT EXISTS no_show_stats (
    patient_id         UUID        PRIMARY KEY,
    total_appointments INT         NOT NULL DEFAULT 0,
    no_show_count      INT         NOT NULL DEFAULT 0,
    completed_count    INT         NOT NULL DEFAULT 0,
    cancelled_count    INT         NOT NULL DEFAULT 0,
    last_no_show_at    TIMESTAMPTZ,

    version            INT         NOT NULL DEFAULT 0,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT no_show_counts_chk CHECK (
        total_appointments >= 0 AND no_show_count >= 0 AND no_show_count <= total_appointments
    )
);

-- ---------------------------------------------------------------------------
-- consumed_events -- consumer-side idempotency
-- ---------------------------------------------------------------------------
-- JetStream guarantees at-least-once. Every consumer records the envelope id
-- it has already handled here, inside the same transaction as its effect.
CREATE TABLE IF NOT EXISTS consumed_events (
    consumer     TEXT        NOT NULL,
    event_id     UUID        NOT NULL,
    subject      TEXT        NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (consumer, event_id)
);

CREATE INDEX IF NOT EXISTS idx_consumed_events_processed ON consumed_events (processed_at);
