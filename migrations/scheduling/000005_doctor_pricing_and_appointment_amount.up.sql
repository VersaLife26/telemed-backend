-- The revenue path.
--
-- Before this migration the scheduling database had no fee, price or amount
-- column anywhere. It consumed doctor.approved, which carried only
-- {doctor_id, user_id}, and published appointment.created without an amount.
-- payment-service required amount_cents and refused anything at zero, so every
-- booking on the platform was created, held a slot, and could never be paid
-- for. Nothing errored: encoding/json filled the missing field with 0.
--
-- Two tables change:
--   1. doctor_pricing -- a projection of doctor-service's list price, fed by
--      doctor.approved and doctor.updated.
--   2. appointments   -- gains the QUOTE, stamped once at booking.
--
-- The distinction between the two is the whole design. doctor_pricing is
-- current and mutable; appointments.amount_cents is historical and immutable.
-- A patient who books at LKR 2,000 pays LKR 2,000 even if the doctor raises
-- their fee a minute later, and the row proves what they were quoted.

-- ---------------------------------------------------------------------------
-- doctor_pricing
-- ---------------------------------------------------------------------------
-- No FK to any doctors table: that table lives in telemed_doctor, owned by
-- another service (ADR-004). doctor_id is a UUID that *means* a row there.
CREATE TABLE IF NOT EXISTS doctor_pricing (
    doctor_id      UUID        PRIMARY KEY,

    specialty      TEXT        NOT NULL DEFAULT '',
    -- BIGINT cents, never a float, never a currency-named column.
    fee_cents      BIGINT      NOT NULL CHECK (fee_cents >= 0),
    currency       CHAR(3)     NOT NULL DEFAULT 'LKR'
                       CONSTRAINT doctor_pricing_currency_chk CHECK (currency ~ '^[A-Z]{3}$'),
    languages      TEXT[]      NOT NULL DEFAULT '{}',
    -- Mirrors doctor-service's verification_status. Stored so an operator can
    -- see why a doctor stopped being quotable without joining across services.
    status         TEXT        NOT NULL DEFAULT 'approved',

    -- OUT-OF-ORDER SAFETY, and it is load-bearing.
    --
    -- JetStream is at-least-once and gives no ordering guarantee across
    -- redeliveries. Without this column, a doctor.updated raising the fee to
    -- 3000 followed by a REDELIVERY of the older doctor.approved at 2000 would
    -- silently roll the price back, and every subsequent booking would quote
    -- the wrong number. Every upsert is guarded on this being non-decreasing,
    -- so a stale event is a no-op rather than a regression.
    last_event_at  TIMESTAMPTZ NOT NULL,

    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The booking hot path reads by primary key, so no extra index is needed for
-- it. This one serves the admin "who is quotable right now" question.
CREATE INDEX IF NOT EXISTS idx_doctor_pricing_status
    ON doctor_pricing (status, specialty);

-- ---------------------------------------------------------------------------
-- appointments: the quote
-- ---------------------------------------------------------------------------
-- Nullable, with no default. That is deliberate and it is the opposite of what
-- a DEFAULT 0 would do:
--
--   * DEFAULT 0 would let a booking that failed to find a price succeed anyway
--     and become exactly the unpayable appointment this migration exists to
--     prevent. A zero amount is not a free consultation; it is a bug.
--   * NOT NULL cannot be applied to a table that already has rows booked
--     before pricing existed, and back-filling those with a guessed price
--     would be inventing money.
--
-- The service layer refuses to insert an appointment without a quote
-- (ErrDoctorNotPriced), and the CHECK below refuses a zero or negative one, so
-- the only rows that can carry NULL are pre-migration ones.
ALTER TABLE appointments
    ADD COLUMN IF NOT EXISTS amount_cents BIGINT,
    ADD COLUMN IF NOT EXISTS currency     CHAR(3),
    ADD COLUMN IF NOT EXISTS specialty    TEXT;

ALTER TABLE appointments
    ADD CONSTRAINT appointments_amount_chk
    CHECK (amount_cents IS NULL OR amount_cents > 0);

ALTER TABLE appointments
    ADD CONSTRAINT appointments_currency_chk
    CHECK (currency IS NULL OR currency ~ '^[A-Z]{3}$');

-- amount_cents and currency travel together or not at all. A row with an
-- amount and no currency is not a price.
ALTER TABLE appointments
    ADD CONSTRAINT appointments_amount_currency_chk
    CHECK ((amount_cents IS NULL) = (currency IS NULL));

-- Finance reads "what did we quote, by day". Partial: pre-pricing rows are
-- history, not revenue.
CREATE INDEX IF NOT EXISTS idx_appointments_amount
    ON appointments (slot_start_at, amount_cents)
    WHERE amount_cents IS NOT NULL;
