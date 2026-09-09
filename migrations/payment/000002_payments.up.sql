-- Payment service schema.
--
-- Three rules govern everything below:
--
--  1. Money is BIGINT cents. There is no NUMERIC, no DOUBLE PRECISION, and no
--     column anywhere in this file that could hold 0.1 + 0.2.
--  2. Commission rules are versioned rows, never mutated in place. A payment
--     records the exact rule row that priced it, so an invoice printed in 2031
--     for a consultation in 2026 reproduces the 2026 numbers.
--  3. ledger_entries is append-only, enforced by trigger rather than by
--     convention. A convention is not a control.

-- ---------------------------------------------------------------------------
-- commission_rules: versioned pricing. Insert a new version, never UPDATE.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS commission_rules (
    id                       UUID        PRIMARY KEY,
    -- rule_key identifies the *rule*, version identifies the revision of it.
    -- e.g. rule_key = 'specialty:GP', versions 1, 2, 3 over the years.
    rule_key                 TEXT        NOT NULL,
    version                  INT         NOT NULL CHECK (version > 0),

    -- scope/match_value drive selection. Most specific wins:
    --   corporate (3) > specialty (2) > default (1)
    scope                    TEXT        NOT NULL
        CHECK (scope IN ('default', 'specialty', 'corporate')),
    match_value              TEXT        NOT NULL DEFAULT '',

    -- Rates are integer basis points. 2000 bps = 20.00%. Basis points give
    -- two decimal places of rate precision with zero floating point.
    rate_bps                 INT         NOT NULL CHECK (rate_bps BETWEEN 0 AND 10000),
    provider_fee_bps         INT         NOT NULL DEFAULT 0 CHECK (provider_fee_bps BETWEEN 0 AND 10000),
    provider_fee_fixed_cents BIGINT      NOT NULL DEFAULT 0 CHECK (provider_fee_fixed_cents >= 0),

    -- Rounding is part of the priced contract, not an implementation detail.
    -- Changing it changes totals, so it is versioned with the rate.
    rounding                 TEXT        NOT NULL DEFAULT 'half_up'
        CHECK (rounding IN ('half_up', 'half_even')),

    effective_from           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    effective_to             TIMESTAMPTZ,
    note                     TEXT        NOT NULL DEFAULT '',
    created_by               UUID,

    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT commission_rules_key_version_uniq UNIQUE (rule_key, version),
    CONSTRAINT commission_rules_window CHECK (effective_to IS NULL OR effective_to > effective_from)
);

-- Exactly one open version per rule_key. Superseding a rule means stamping
-- effective_to on the old row and inserting the next version, in one
-- transaction; this index makes any other sequence fail loudly.
CREATE UNIQUE INDEX IF NOT EXISTS idx_commission_rules_active
    ON commission_rules (rule_key)
    WHERE effective_to IS NULL;

CREATE INDEX IF NOT EXISTS idx_commission_rules_lookup
    ON commission_rules (scope, match_value, effective_from DESC);

-- ---------------------------------------------------------------------------
-- payouts: one row per doctor per settlement period.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS payouts (
    id              UUID        PRIMARY KEY,
    doctor_id       UUID        NOT NULL,
    period_start    DATE        NOT NULL,
    period_end      DATE        NOT NULL,

    amount_cents    BIGINT      NOT NULL DEFAULT 0 CHECK (amount_cents >= 0),
    currency        CHAR(3)     NOT NULL DEFAULT 'LKR',
    payment_count   INT         NOT NULL DEFAULT 0 CHECK (payment_count >= 0),

    status          TEXT        NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'processing', 'paid', 'failed', 'cancelled')),
    provider        TEXT        NOT NULL
        CHECK (provider IN ('stripe', 'payhere', 'dialog', 'mock')),
    transfer_id     TEXT        NOT NULL DEFAULT '',
    failure_reason  TEXT        NOT NULL DEFAULT '',

    -- Stable across retries: the same payout row always presents the same
    -- idempotency key to the provider, so a re-run after a mid-batch crash
    -- cannot create a second transfer.
    idempotency_key TEXT        NOT NULL,

    initiated_at    TIMESTAMPTZ,
    paid_at         TIMESTAMPTZ,
    attempts        INT         NOT NULL DEFAULT 0,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,
    version         INT         NOT NULL DEFAULT 1,

    CONSTRAINT payouts_idempotency_uniq UNIQUE (idempotency_key),
    -- The re-runnability backstop. Two concurrent payout runs for the same
    -- doctor and period cannot both create a payout; the loser gets 23505.
    CONSTRAINT payouts_doctor_period_uniq UNIQUE (doctor_id, period_start, period_end),
    CONSTRAINT payouts_period CHECK (period_end >= period_start)
);

CREATE INDEX IF NOT EXISTS idx_payouts_doctor
    ON payouts (doctor_id, period_start DESC)
    WHERE deleted_at IS NULL;

-- The resume scan. Partial, so it stays tiny on a healthy system.
CREATE INDEX IF NOT EXISTS idx_payouts_unsettled
    ON payouts (created_at)
    WHERE status IN ('pending', 'processing');

-- ---------------------------------------------------------------------------
-- payments
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS payments (
    id                  UUID        PRIMARY KEY,

    -- One payment per appointment. This UNIQUE is what makes the
    -- appointment.created consumer safe to redeliver.
    appointment_id      UUID        NOT NULL,
    patient_id          UUID        NOT NULL,
    doctor_id           UUID        NOT NULL,

    -- Denormalised pricing inputs, captured at creation. If the doctor later
    -- changes specialty, this payment still prices as it did on the day.
    specialty           TEXT        NOT NULL DEFAULT '',
    corporate_client    TEXT        NOT NULL DEFAULT '',

    amount_cents        BIGINT      NOT NULL CHECK (amount_cents > 0),
    currency            CHAR(3)     NOT NULL DEFAULT 'LKR',

    provider            TEXT        NOT NULL
        CHECK (provider IN ('stripe', 'payhere', 'dialog', 'mock')),
    provider_intent_id  TEXT        NOT NULL DEFAULT '',
    provider_reference  TEXT        NOT NULL DEFAULT '',

    status              TEXT        NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'requires_action', 'requires_pin', 'succeeded',
                          'failed', 'partially_refunded', 'refunded')),

    commission_cents    BIGINT      NOT NULL DEFAULT 0 CHECK (commission_cents >= 0),
    provider_fee_cents  BIGINT      NOT NULL DEFAULT 0 CHECK (provider_fee_cents >= 0),
    doctor_payout_cents BIGINT      NOT NULL DEFAULT 0 CHECK (doctor_payout_cents >= 0),
    refunded_cents      BIGINT      NOT NULL DEFAULT 0 CHECK (refunded_cents >= 0),
    -- The capture-time split above is immutable: it is what the invoice
    -- printed and what the patient was told. Refunds are tracked as separate
    -- clawback columns so a partial refund never rewrites history, and so the
    -- split invariant below keeps holding for the life of the row.
    refunded_commission_cents BIGINT NOT NULL DEFAULT 0 CHECK (refunded_commission_cents >= 0),
    refunded_payout_cents     BIGINT NOT NULL DEFAULT 0 CHECK (refunded_payout_cents >= 0),

    -- Which rule row priced this payment. Historical reconstruction depends
    -- entirely on this reference, which is why it is a real FK.
    commission_rule_id  UUID        REFERENCES commission_rules (id),
    commission_rule_key TEXT        NOT NULL DEFAULT '',
    commission_rule_ver INT         NOT NULL DEFAULT 0,

    idempotency_key     TEXT        NOT NULL,
    payout_id           UUID        REFERENCES payouts (id),
    failure_reason      TEXT        NOT NULL DEFAULT '',

    succeeded_at        TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at          TIMESTAMPTZ,
    version             INT         NOT NULL DEFAULT 1,

    CONSTRAINT payments_appointment_uniq  UNIQUE (appointment_id),
    CONSTRAINT payments_idempotency_uniq  UNIQUE (idempotency_key),
    CONSTRAINT payments_refund_bounded    CHECK (refunded_cents <= amount_cents),
    -- A refund is itself split, and the split must reconstitute the refund.
    CONSTRAINT payments_refund_split      CHECK (refunded_commission_cents + refunded_payout_cents = refunded_cents),
    CONSTRAINT payments_refund_commission CHECK (refunded_commission_cents <= commission_cents),
    CONSTRAINT payments_refund_payout     CHECK (refunded_payout_cents <= doctor_payout_cents),

    -- The money invariant, enforced by the database and not merely by Go.
    -- Before capture the split is all zeroes; from the moment a payment
    -- succeeds the three parts must reconstitute the amount exactly.
    CONSTRAINT payments_split_balances CHECK (
        status IN ('pending', 'requires_action', 'requires_pin', 'failed')
        OR commission_cents + provider_fee_cents + doctor_payout_cents = amount_cents
    )
);

CREATE INDEX IF NOT EXISTS idx_payments_patient
    ON payments (patient_id, created_at DESC)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_payments_doctor
    ON payments (doctor_id, created_at DESC)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_payments_status
    ON payments (status, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_payments_provider_intent
    ON payments (provider, provider_intent_id)
    WHERE provider_intent_id <> '';

-- The payout batch scan: succeeded, not yet paid out. Partial index keeps it
-- proportional to the settlement backlog rather than to lifetime volume.
CREATE INDEX IF NOT EXISTS idx_payments_payout_due
    ON payments (doctor_id, succeeded_at)
    WHERE status IN ('succeeded', 'partially_refunded') AND payout_id IS NULL;

-- ---------------------------------------------------------------------------
-- webhook_events: the idempotency ledger for inbound provider callbacks.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS webhook_events (
    id               UUID        PRIMARY KEY,
    provider         TEXT        NOT NULL
        CHECK (provider IN ('stripe', 'payhere', 'dialog', 'mock')),
    event_id         TEXT        NOT NULL,
    event_type       TEXT        NOT NULL DEFAULT '',
    payment_id       UUID        REFERENCES payments (id),

    -- The raw body exactly as received, retained for dispute evidence. It is
    -- never logged; it lives here and only here.
    payload          JSONB       NOT NULL,
    signature_header TEXT        NOT NULL DEFAULT '',

    received_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at     TIMESTAMPTZ,
    processing_error TEXT        NOT NULL DEFAULT '',
    attempts         INT         NOT NULL DEFAULT 0,

    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- THE idempotency mechanism. A replayed webhook loses the race to insert
    -- and is answered 200 without touching payment state.
    --
    -- Scoped by provider, not global: a PayHere event id is our own order id
    -- and a Dialog externalTrxId is also our own reference, so a single-column
    -- unique index would let a Dialog callback silently suppress a PayHere one
    -- for the same appointment. Provider-scoped uniqueness is the same
    -- guarantee without that cross-provider collision.
    CONSTRAINT webhook_events_provider_event_uniq UNIQUE (provider, event_id)
);

CREATE INDEX IF NOT EXISTS idx_webhook_events_unprocessed
    ON webhook_events (received_at)
    WHERE processed_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_webhook_events_payment
    ON webhook_events (payment_id, received_at DESC);

-- ---------------------------------------------------------------------------
-- refunds
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS refunds (
    id                 UUID        PRIMARY KEY,
    payment_id         UUID        NOT NULL REFERENCES payments (id),

    amount_cents       BIGINT      NOT NULL CHECK (amount_cents > 0),
    currency           CHAR(3)     NOT NULL DEFAULT 'LKR',

    -- Why the money went back: doctor_cancelled | patient_cancelled_early |
    -- patient_cancelled_late | no_show | admin_override | duplicate.
    reason             TEXT        NOT NULL,
    -- Which policy clause produced the amount, kept for audit.
    policy             TEXT        NOT NULL DEFAULT '',
    percent            INT         NOT NULL DEFAULT 0 CHECK (percent BETWEEN 0 AND 100),

    provider_refund_id TEXT        NOT NULL DEFAULT '',
    status             TEXT        NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'succeeded', 'failed')),
    failure_reason     TEXT        NOT NULL DEFAULT '',

    -- One refund per (payment, reason-instance). Blocks a double-click and a
    -- redelivered appointment.cancelled event alike.
    idempotency_key    TEXT        NOT NULL,

    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    version            INT         NOT NULL DEFAULT 1,

    CONSTRAINT refunds_idempotency_uniq UNIQUE (idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_refunds_payment ON refunds (payment_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_refunds_status  ON refunds (status) WHERE status = 'pending';

-- ---------------------------------------------------------------------------
-- ledger_entries: append-only double-entry.
--
-- Every financial event writes a balanced group of rows sharing one entry_id:
-- SUM(debit) = SUM(credit) within the group, always. This table is the thing
-- a finance auditor reads; payments.commission_cents is a convenience column
-- derived from it.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ledger_entries (
    id           BIGSERIAL   PRIMARY KEY,
    -- Groups the legs of one balanced transaction.
    entry_id     UUID        NOT NULL,
    entry_type   TEXT        NOT NULL,

    account      TEXT        NOT NULL,
    direction    TEXT        NOT NULL CHECK (direction IN ('debit', 'credit')),
    amount_cents BIGINT      NOT NULL CHECK (amount_cents > 0),
    currency     CHAR(3)     NOT NULL DEFAULT 'LKR',

    payment_id   UUID        REFERENCES payments (id),
    payout_id    UUID        REFERENCES payouts (id),
    refund_id    UUID        REFERENCES refunds (id),
    doctor_id    UUID,

    description  TEXT        NOT NULL DEFAULT '',
    occurred_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ledger_entry_group ON ledger_entries (entry_id);
CREATE INDEX IF NOT EXISTS idx_ledger_payment     ON ledger_entries (payment_id);
CREATE INDEX IF NOT EXISTS idx_ledger_payout      ON ledger_entries (payout_id);
CREATE INDEX IF NOT EXISTS idx_ledger_account     ON ledger_entries (account, occurred_at);
-- BRIN because this table only ever grows in occurred_at order and will be the
-- largest table in the database within a year.
CREATE INDEX IF NOT EXISTS idx_ledger_occurred_brin
    ON ledger_entries USING BRIN (occurred_at);

-- Immutability as a database control, not a code review comment. Application
-- roles keep INSERT and SELECT; UPDATE, DELETE and TRUNCATE raise.
CREATE OR REPLACE FUNCTION ledger_entries_append_only() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only: % is not permitted', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_ledger_no_update ON ledger_entries;
CREATE TRIGGER trg_ledger_no_update
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_append_only();

DROP TRIGGER IF EXISTS trg_ledger_no_truncate ON ledger_entries;
CREATE TRIGGER trg_ledger_no_truncate
    BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_entries_append_only();

-- ---------------------------------------------------------------------------
-- updated_at maintenance. One function, four triggers.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_payments_updated_at ON payments;
CREATE TRIGGER trg_payments_updated_at BEFORE UPDATE ON payments
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_payouts_updated_at ON payouts;
CREATE TRIGGER trg_payouts_updated_at BEFORE UPDATE ON payouts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_refunds_updated_at ON refunds;
CREATE TRIGGER trg_refunds_updated_at BEFORE UPDATE ON refunds
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_webhook_events_updated_at ON webhook_events;
CREATE TRIGGER trg_webhook_events_updated_at BEFORE UPDATE ON webhook_events
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_commission_rules_updated_at ON commission_rules;
CREATE TRIGGER trg_commission_rules_updated_at BEFORE UPDATE ON commission_rules
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
