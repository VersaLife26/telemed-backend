-- Doctor-facing analytics: an event-fed projection, not a query.
--
-- WHY A PROJECTION AND NOT A JOIN
-- The four numbers doctor-app screen 9 renders -- sessions, rating, no-show
-- rate, peak hours -- and the three it renders on the earnings tab -- gross,
-- commission, net -- are spread across four services:
--
--   appointments  -> telemed_scheduling
--   consultations -> telemed_consultation
--   payments      -> telemed_payment
--   reviews       -> telemed_doctor   (here)
--
-- ADR-004 forbids the cross-service join that would answer this in one query,
-- and a fan-out of three synchronous calls on a screen a doctor opens every
-- morning would make that screen fail whenever any one of them is down. So the
-- facts are consumed off JetStream and folded into local rollups, exactly as
-- doctor_slot_state / doctor_availability_summary already do for search.
--
-- THE SHAPE, AND WHY IT IS TWO LAYERS
-- A counter-per-event design (`UPDATE ... SET sessions = sessions + 1`) is not
-- safe here. JetStream is at-least-once and unordered: a redelivery would
-- double-count and there is no way to tell it from a second real appointment.
-- Guarding a counter on last_event_at does not help either -- a counter has no
-- "current value" a later event supersedes.
--
-- So every event first lands as a FACT ROW keyed on its own natural business
-- key (appointment_id, consultation_id, payment_id, payout_id). Those upserts
-- are last-writer-wins by producer event time, which makes a redelivery a
-- no-op AND makes an out-of-order delivery a no-op. The daily and hour-of-week
-- rollups are then RECOMPUTED from the facts for exactly the buckets the fact
-- touched, inside the same transaction. Replay the entire stream in any order
-- and the rollups land on the same numbers.
--
-- DATING RULE (documented because the three fact types are dated differently
-- and a reader will otherwise assume they are not)
--   * appointment facts are dated by the APPOINTMENT'S START, in Asia/Colombo.
--     "How many sessions did I have on the 14th" means the day the patient was
--     seen, not the day an event was relayed.
--   * consultation facts are dated by when the call ENDED. consultation.ended
--     carries no appointment start; the two differ only for a call that
--     straddles local midnight.
--   * payment and payout facts are dated by SETTLEMENT. "What did I earn in
--     August" is a money question, and money lands when it lands.
--
-- Money is BIGINT cents throughout, never float, never NUMERIC-for-currency.

-- ---------------------------------------------------------------------------
-- doctor_appointment_fact -- one row per appointment that reached a terminal
-- state, fed by appointment.completed / appointment.no_show /
-- appointment.cancelled.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS doctor_appointment_fact (
    appointment_id UUID        PRIMARY KEY,
    doctor_id      UUID        NOT NULL,

    -- The appointment's scheduled start, UTC.
    start_at       TIMESTAMPTZ NOT NULL,
    -- start_at bucketed into the business timezone. Denormalised rather than
    -- computed at read time because a DATE derived with AT TIME ZONE inside a
    -- WHERE clause is not sargable and this is the column every rollup groups
    -- on.
    local_date     DATE        NOT NULL,
    -- 0 = Sunday .. 6 = Saturday, matching Go's time.Weekday and the
    -- day_of_week convention already used by working_hours.
    day_of_week    SMALLINT    NOT NULL CHECK (day_of_week BETWEEN 0 AND 6),
    hour_of_day    SMALLINT    NOT NULL CHECK (hour_of_day BETWEEN 0 AND 23),

    outcome        VARCHAR(20) NOT NULL
                       CHECK (outcome IN ('completed', 'no_show', 'cancelled')),

    -- Last-writer-wins by event time. See the header: this is what makes the
    -- projection converge under at-least-once, unordered delivery.
    last_event_id  UUID        NOT NULL,
    last_event_at  TIMESTAMPTZ NOT NULL,

    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_doctor_appointment_fact_daily
    ON doctor_appointment_fact (doctor_id, local_date);

-- The peak-hours recompute groups on this triple.
CREATE INDEX IF NOT EXISTS idx_doctor_appointment_fact_hour
    ON doctor_appointment_fact (doctor_id, day_of_week, hour_of_day);

-- ---------------------------------------------------------------------------
-- doctor_consultation_fact -- one row per ended call, fed by
-- consultation.ended. This is where average consultation duration comes from:
-- the appointment says how long the slot was, only the call says how long the
-- doctor actually spent.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS doctor_consultation_fact (
    consultation_id  UUID        PRIMARY KEY,
    doctor_id        UUID        NOT NULL,
    appointment_id   UUID        NOT NULL,

    ended_at         TIMESTAMPTZ NOT NULL,
    local_date       DATE        NOT NULL,
    duration_seconds INT         NOT NULL CHECK (duration_seconds >= 0),
    -- completed | abandoned | failed | no_show, per events.ConsultationEnded.
    -- Only 'completed' calls contribute to the average duration: averaging in
    -- a 4-second failed connection understates every doctor's consultation
    -- length and the number is quoted back to them as a quality metric.
    end_reason       VARCHAR(20) NOT NULL,

    last_event_id    UUID        NOT NULL,
    last_event_at    TIMESTAMPTZ NOT NULL,

    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_doctor_consultation_fact_daily
    ON doctor_consultation_fact (doctor_id, local_date);

-- ---------------------------------------------------------------------------
-- doctor_payment_fact -- one row per settled payment, fed by payment.succeeded.
--
-- commission_cents and payout_cents are taken from the event, never re-derived
-- from a local commission table. payment-service owns the commission rule; a
-- second copy of it here would drift, and the first time it did, a doctor's
-- earnings screen and their bank statement would disagree.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS doctor_payment_fact (
    payment_id      UUID        PRIMARY KEY,
    doctor_id       UUID        NOT NULL,
    appointment_id  UUID        NOT NULL,

    succeeded_at    TIMESTAMPTZ NOT NULL,
    local_date      DATE        NOT NULL,

    gross_cents     BIGINT      NOT NULL CHECK (gross_cents >= 0),
    commission_cents BIGINT     NOT NULL CHECK (commission_cents >= 0),
    net_cents       BIGINT      NOT NULL CHECK (net_cents >= 0),
    currency        CHAR(3)     NOT NULL,

    last_event_id   UUID        NOT NULL,
    last_event_at   TIMESTAMPTZ NOT NULL,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_doctor_payment_fact_daily
    ON doctor_payment_fact (doctor_id, local_date);

-- ---------------------------------------------------------------------------
-- doctor_payout_fact -- one row per settled payout, fed by payout.sent.
--
-- This is what makes "payout status" on the earnings screen a fact rather than
-- a guess. Without it the honest answer for every period is "unknown", and a
-- screen that renders "pending" for money already in the doctor's bank is a
-- support ticket.
--
-- period_start/period_end are DATE because events.PayoutSent declares them as
-- "YYYY-MM-DD" strings. payment-service is currently sending RFC3339 there
-- (recorded in _shared/INTEGRATION-FIXES.md); the consumer parses both.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS doctor_payout_fact (
    payout_id     UUID        PRIMARY KEY,
    doctor_id     UUID        NOT NULL,

    amount_cents  BIGINT      NOT NULL CHECK (amount_cents >= 0),
    currency      CHAR(3)     NOT NULL,
    period_start  DATE        NOT NULL,
    period_end    DATE        NOT NULL,
    transfer_id   TEXT        NOT NULL DEFAULT '',
    sent_at       TIMESTAMPTZ NOT NULL,

    last_event_id UUID        NOT NULL,
    last_event_at TIMESTAMPTZ NOT NULL,

    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT doctor_payout_fact_period_chk CHECK (period_end >= period_start)
);

CREATE INDEX IF NOT EXISTS idx_doctor_payout_fact_period
    ON doctor_payout_fact (doctor_id, period_start, period_end);

-- ---------------------------------------------------------------------------
-- doctor_analytics_daily -- the rollup the analytics and earnings endpoints
-- read. Recomputed from the fact tables above, never incremented.
--
-- One row per (doctor_id, date). See the header for why the session columns
-- and the money columns are dated by different clocks.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS doctor_analytics_daily (
    doctor_id            UUID   NOT NULL,
    date                 DATE   NOT NULL,

    completed_count      INT    NOT NULL DEFAULT 0 CHECK (completed_count >= 0),
    no_show_count        INT    NOT NULL DEFAULT 0 CHECK (no_show_count >= 0),
    cancelled_count      INT    NOT NULL DEFAULT 0 CHECK (cancelled_count >= 0),

    -- Calls that ended with end_reason = 'completed', and their total length.
    -- Stored as a SUM and a COUNT rather than a pre-divided average: averaging
    -- an average over a date range is wrong, and storing the two components is
    -- the only way the range endpoint can divide once at the end.
    consultation_count   INT    NOT NULL DEFAULT 0 CHECK (consultation_count >= 0),
    consultation_seconds BIGINT NOT NULL DEFAULT 0 CHECK (consultation_seconds >= 0),

    gross_cents          BIGINT NOT NULL DEFAULT 0 CHECK (gross_cents >= 0),
    commission_cents     BIGINT NOT NULL DEFAULT 0 CHECK (commission_cents >= 0),
    net_cents            BIGINT NOT NULL DEFAULT 0 CHECK (net_cents >= 0),
    -- Empty until the first payment lands on this date. A day with sessions
    -- and no settled payment has no currency to report, and defaulting it to
    -- 'LKR' would be inventing one.
    currency             CHAR(3) NOT NULL DEFAULT '',

    payment_count        INT    NOT NULL DEFAULT 0 CHECK (payment_count >= 0),

    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (doctor_id, date)
);

-- The range scan every endpoint does: WHERE doctor_id = $1 AND date BETWEEN.
-- The primary key already serves it; this index is deliberately NOT added.

-- ---------------------------------------------------------------------------
-- doctor_peak_hours -- bookings by hour of week, which is what the app charts.
--
-- 168 rows per doctor at most (7 days x 24 hours), materialised rather than
-- grouped at read time so the chart is a single index range scan on a table
-- whose size is bounded by the doctor count, not by the appointment count.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS doctor_peak_hours (
    doctor_id       UUID     NOT NULL,
    day_of_week     SMALLINT NOT NULL CHECK (day_of_week BETWEEN 0 AND 6),
    hour_of_day     SMALLINT NOT NULL CHECK (hour_of_day BETWEEN 0 AND 23),

    -- Every appointment that reached a terminal state in this bucket:
    -- completed + no_show + cancelled. This is DEMAND -- when patients try to
    -- book this doctor -- which is the question the chart answers.
    booking_count   INT      NOT NULL DEFAULT 0 CHECK (booking_count >= 0),
    completed_count INT      NOT NULL DEFAULT 0 CHECK (completed_count >= 0),
    no_show_count   INT      NOT NULL DEFAULT 0 CHECK (no_show_count >= 0),
    cancelled_count INT      NOT NULL DEFAULT 0 CHECK (cancelled_count >= 0),

    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (doctor_id, day_of_week, hour_of_day)
);
