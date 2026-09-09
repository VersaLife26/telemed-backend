-- Promotional codes and their redemptions.
--
-- A promo code is money. Everything in this file exists because of that:
--
--  1. A discount is BIGINT cents, like every other amount in this schema.
--     `percent_bps` is integer basis points (2000 = 20.00%), never a float,
--     so the same code applied to the same fee produces the same cent figure
--     on every machine, forever.
--
--  2. `payments.amount_cents` remains THE amount charged, so every existing
--     rule -- the commission split, the ledger, the payout, the refund
--     policy -- keeps computing on the right number without being told about
--     promotions at all. The pre-discount price is preserved separately in
--     `gross_amount_cents`, and the three columns are tied together by a
--     CHECK so no code path can leave them inconsistent.
--
--  3. Redemption is a *reservation* first and a consumption later. A patient
--     who types a code and then closes the app must not burn it, and a
--     patient who pays must not be able to reuse it. Those are two different
--     states, so they are two different values of `status`, not one boolean.
--
--  4. Over-redemption is refused by the database, not only by Go. The
--     `promo_codes_budget` CHECK is the backstop underneath the row lock the
--     service takes; if a future refactor loses the lock, the constraint
--     turns a silent over-spend into a failed transaction.

-- ---------------------------------------------------------------------------
-- payments: record the pre-discount price alongside the charged one.
-- ---------------------------------------------------------------------------
ALTER TABLE payments ADD COLUMN IF NOT EXISTS gross_amount_cents BIGINT NOT NULL DEFAULT 0;
ALTER TABLE payments ADD COLUMN IF NOT EXISTS discount_cents     BIGINT NOT NULL DEFAULT 0;
ALTER TABLE payments ADD COLUMN IF NOT EXISTS promo_code         TEXT   NOT NULL DEFAULT '';

-- Existing rows were never discounted, so gross == charged.
UPDATE payments SET gross_amount_cents = amount_cents WHERE gross_amount_cents = 0;

ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_discount_nonneg;
ALTER TABLE payments ADD  CONSTRAINT payments_discount_nonneg CHECK (discount_cents >= 0);

-- The invariant. amount_cents is what the rail charges and what the split is
-- computed from; gross_amount_cents is what the doctor's list price was. A
-- write that forgets one of the three fails here rather than producing an
-- invoice whose lines do not add up.
ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_discount_balances;
ALTER TABLE payments ADD  CONSTRAINT payments_discount_balances
    CHECK (gross_amount_cents = amount_cents + discount_cents);

-- A discounted payment must name the code that discounted it, and an
-- undiscounted one must not claim a code. Without this, a released
-- reservation that forgot to clear promo_code would print a promotion on the
-- invoice that was never applied.
ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_discount_has_code;
ALTER TABLE payments ADD  CONSTRAINT payments_discount_has_code
    CHECK ((discount_cents = 0 AND promo_code = '') OR (discount_cents > 0 AND promo_code <> ''));

-- ---------------------------------------------------------------------------
-- promo_codes
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS promo_codes (
    id                 UUID        PRIMARY KEY,

    -- Stored and compared in upper case. The service upper-cases on the way
    -- in, so "welcome20" and "WELCOME20" are one code and not two.
    code               TEXT        NOT NULL,
    description        TEXT        NOT NULL DEFAULT '',

    discount_type      TEXT        NOT NULL CHECK (discount_type IN ('percent', 'fixed')),
    -- Integer basis points: 2000 = 20.00%. Two decimal places of precision
    -- with zero floating point, exactly like commission_rules.rate_bps.
    percent_bps        INT         NOT NULL DEFAULT 0 CHECK (percent_bps BETWEEN 0 AND 10000),
    amount_off_cents   BIGINT      NOT NULL DEFAULT 0 CHECK (amount_off_cents >= 0),
    -- Caps a percentage code, e.g. "20% off, up to LKR 500". 0 means uncapped.
    max_discount_cents BIGINT      NOT NULL DEFAULT 0 CHECK (max_discount_cents >= 0),
    -- Floor on the consultation fee the code may be used against.
    min_amount_cents   BIGINT      NOT NULL DEFAULT 0 CHECK (min_amount_cents >= 0),

    currency           CHAR(3)     NOT NULL DEFAULT 'LKR',

    valid_from         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    valid_until        TIMESTAMPTZ,

    -- NULL means unlimited. A finite budget is the common case and the one
    -- that has to be exactly right under concurrency.
    max_redemptions    INT         CHECK (max_redemptions IS NULL OR max_redemptions > 0),
    max_per_user       INT         NOT NULL DEFAULT 1 CHECK (max_per_user > 0),

    -- Live redemptions: reserved plus consumed, never released. Maintained in
    -- the same transaction, under the same row lock, as every insert into and
    -- release from promo_redemptions, so it cannot drift from that table.
    redemption_count   INT         NOT NULL DEFAULT 0 CHECK (redemption_count >= 0),

    active             BOOLEAN     NOT NULL DEFAULT TRUE,

    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at         TIMESTAMPTZ,
    version            INT         NOT NULL DEFAULT 1,

    CONSTRAINT promo_codes_code_uniq UNIQUE (code),
    CONSTRAINT promo_codes_window CHECK (valid_until IS NULL OR valid_until > valid_from),

    -- Exactly one of the two discount shapes is populated. A row carrying
    -- both a percentage and a fixed amount has no defined meaning, and the
    -- code that read it would have to invent one.
    CONSTRAINT promo_codes_shape CHECK (
        (discount_type = 'percent' AND percent_bps > 0     AND amount_off_cents = 0)
     OR (discount_type = 'fixed'   AND amount_off_cents > 0 AND percent_bps = 0)
    ),

    -- The over-redemption backstop. The service serialises redemption with
    -- SELECT ... FOR UPDATE on this row; this constraint is what still holds
    -- if that lock is ever lost in a refactor.
    CONSTRAINT promo_codes_budget CHECK (max_redemptions IS NULL OR redemption_count <= max_redemptions)
);

CREATE INDEX IF NOT EXISTS idx_promo_codes_active
    ON promo_codes (code)
    WHERE active AND deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- promo_redemptions
--
-- reserved -> consumed   (the payment captured; the code is spent)
-- reserved -> released   (abandoned, expired, replaced, or the payment failed)
--
-- There is no consumed -> released edge. Refunding a paid consultation does
-- not give the promotion back: the patient used it.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS promo_redemptions (
    id             UUID        PRIMARY KEY,
    promo_code_id  UUID        NOT NULL REFERENCES promo_codes (id),
    -- Denormalised so an audit of "who used WELCOME20" survives the code row
    -- being renamed or soft-deleted.
    code           TEXT        NOT NULL,

    user_id        UUID        NOT NULL,
    payment_id     UUID        NOT NULL REFERENCES payments (id),
    appointment_id UUID        NOT NULL,

    discount_cents BIGINT      NOT NULL CHECK (discount_cents > 0),
    currency       CHAR(3)     NOT NULL DEFAULT 'LKR',

    status         TEXT        NOT NULL DEFAULT 'reserved'
        CHECK (status IN ('reserved', 'consumed', 'released')),

    reserved_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- A reservation nobody pays for is released, by the next caller of the
    -- same code or by the sweeper, whichever gets there first. Without this a
    -- limited code is permanently burned by abandoned checkouts.
    expires_at     TIMESTAMPTZ NOT NULL,
    consumed_at    TIMESTAMPTZ,
    released_at    TIMESTAMPTZ,
    release_reason TEXT        NOT NULL DEFAULT '',

    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    version        INT         NOT NULL DEFAULT 1,

    CONSTRAINT promo_redemptions_terminal CHECK (
        (status = 'reserved' AND consumed_at IS NULL AND released_at IS NULL)
     OR (status = 'consumed' AND consumed_at IS NOT NULL)
     OR (status = 'released' AND released_at IS NOT NULL)
    )
);

-- One live redemption per payment. This is what makes "apply a second code"
-- a replacement rather than a stack, and it is enforced by the database
-- because the alternative is a patient discovering they can apply five codes
-- to one consultation.
CREATE UNIQUE INDEX IF NOT EXISTS idx_promo_redemptions_payment_live
    ON promo_redemptions (payment_id)
    WHERE status <> 'released';

-- The per-user limit scan.
CREATE INDEX IF NOT EXISTS idx_promo_redemptions_user_live
    ON promo_redemptions (promo_code_id, user_id)
    WHERE status <> 'released';

-- The sweeper's scan: reserved and past expiry. Partial, so it stays small.
CREATE INDEX IF NOT EXISTS idx_promo_redemptions_expiring
    ON promo_redemptions (expires_at)
    WHERE status = 'reserved';

DROP TRIGGER IF EXISTS trg_promo_codes_updated_at ON promo_codes;
CREATE TRIGGER trg_promo_codes_updated_at BEFORE UPDATE ON promo_codes
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_promo_redemptions_updated_at ON promo_redemptions;
CREATE TRIGGER trg_promo_redemptions_updated_at BEFORE UPDATE ON promo_redemptions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
