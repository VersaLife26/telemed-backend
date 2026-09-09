-- Carrier-billing PIN challenges.
--
-- Dialog Ideamart's direct debit is authorised by a PIN sent to the
-- subscriber's handset: request PIN -> patient types it -> debit. That middle
-- step is a real state that lasts minutes, and until this table existed the
-- platform had nowhere to keep it. `payments.status = 'requires_pin'` recorded
-- *that* a challenge was outstanding but nothing about it: not when it
-- expires, not how many times the patient has guessed, not which handset it
-- went to. So a PIN that arrived six minutes late was indistinguishable from
-- one typed immediately, a mistyped digit failed the whole payment outright,
-- and an operator looking at a stuck payment had nothing to look at.
--
-- Three properties this table has to have, and why:
--
--  1. **An expiry.** Ideamart's PIN is valid for about five minutes. Debiting
--     against a reference the carrier has already forgotten fails at the
--     carrier with an opaque code; refusing it here gives the patient
--     "that code has expired, tap resend" instead.
--
--  2. **An attempt counter.** Without one, either a mistyped digit kills the
--     payment (which is what happened before) or the PIN can be brute-forced.
--     Four digits is 10,000 possibilities; three attempts is the standard
--     telco allowance and is what this defaults to.
--
--  3. **No PIN.** The code the patient types is never written here, never
--     hashed here, never logged. It travels from the request body to the
--     carrier and is discarded. There is no column it could go in.
--
-- The subscriber's number is stored masked (+9477***4567) for the same
-- reason: an operator needs to see which handset a challenge went to; nobody
-- needs the full number, and payments is not the system of record for it.

CREATE TABLE IF NOT EXISTS pin_challenges (
    id             UUID        PRIMARY KEY,
    payment_id     UUID        NOT NULL REFERENCES payments (id),

    provider       TEXT        NOT NULL
        CHECK (provider IN ('stripe', 'payhere', 'dialog', 'mock')),
    -- The carrier's own correlation id for this challenge (Ideamart's
    -- referenceNo). It is not a secret and it is not the PIN; it is what the
    -- debit call is made against.
    reference      TEXT        NOT NULL CHECK (reference <> ''),
    -- Already masked when it arrives here. logger.MaskPhone does the masking
    -- inside the provider package, so the unmasked number never leaves it.
    masked_msisdn  TEXT        NOT NULL DEFAULT '',

    status         TEXT        NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'confirmed', 'failed', 'expired', 'superseded')),

    attempts       INT         NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts   INT         NOT NULL DEFAULT 3 CHECK (max_attempts > 0),

    expires_at     TIMESTAMPTZ NOT NULL,
    confirmed_at   TIMESTAMPTZ,
    settled_at     TIMESTAMPTZ,
    failure_reason TEXT        NOT NULL DEFAULT '',

    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    version        INT         NOT NULL DEFAULT 1,

    CONSTRAINT pin_challenges_attempts_bounded CHECK (attempts <= max_attempts),
    CONSTRAINT pin_challenges_terminal CHECK (
        (status = 'pending'   AND confirmed_at IS NULL)
     OR (status = 'confirmed' AND confirmed_at IS NOT NULL)
     OR status IN ('failed', 'expired', 'superseded')
    )
);

-- At most one outstanding challenge per payment. Tapping "resend PIN" marks
-- the old one superseded and issues a new one; it does not leave two live
-- references, either of which the patient's SMS might correspond to.
CREATE UNIQUE INDEX IF NOT EXISTS idx_pin_challenges_payment_pending
    ON pin_challenges (payment_id)
    WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS idx_pin_challenges_payment
    ON pin_challenges (payment_id, created_at DESC);

-- The expiry sweep. Partial, so it stays proportional to the number of
-- patients currently holding a phone rather than to lifetime volume.
CREATE INDEX IF NOT EXISTS idx_pin_challenges_expiring
    ON pin_challenges (expires_at)
    WHERE status = 'pending';

DROP TRIGGER IF EXISTS trg_pin_challenges_updated_at ON pin_challenges;
CREATE TRIGGER trg_pin_challenges_updated_at BEFORE UPDATE ON pin_challenges
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
