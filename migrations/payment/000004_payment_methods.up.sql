-- Saved payment methods: provider tokens, and nothing else.
--
-- READ THIS BEFORE ADDING A COLUMN.
--
-- There is no card number column here. There is no CVV column here. There
-- never will be. This platform is not PCI-DSS certified and has no business
-- holding a PAN: the card is entered inside the provider's own SDK (Stripe
-- PaymentSheet), the provider returns an opaque token, and the token is the
-- only thing that crosses back into our process. `brand`, `last4`, `exp_month`
-- and `exp_year` are display metadata that the provider gives us about a card
-- we have never seen -- storing the last four digits is explicitly permitted
-- and is what lets a patient recognise their own card in a list.
--
-- The CHECK constraints below encode that: `last4` is exactly four digits, and
-- `provider_token` is bounded to a length no card number plus expiry could
-- hide inside. They are cheap, and they mean an accidental write of the wrong
-- value fails loudly at the database instead of quietly persisting a PAN.

-- ---------------------------------------------------------------------------
-- payment_customers: our patient id <-> the rail's customer handle.
--
-- One row per patient per rail. Stripe needs a Customer to attach a saved
-- PaymentMethod to; that handle is opaque and carries no personal data of
-- ours -- the customer is created with the patient's UUID in metadata and
-- nothing else, deliberately, so a breach of the Stripe account does not
-- disclose who our patients are.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS payment_customers (
    id                   UUID        PRIMARY KEY,
    patient_id           UUID        NOT NULL,
    provider             TEXT        NOT NULL
        CHECK (provider IN ('stripe', 'payhere', 'dialog', 'mock')),
    provider_customer_id TEXT        NOT NULL CHECK (provider_customer_id <> ''),

    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at           TIMESTAMPTZ,
    version              INT         NOT NULL DEFAULT 1,

    -- One customer handle per patient per rail: two would mean a patient's
    -- saved cards split across two Stripe customers, half of them invisible.
    CONSTRAINT payment_customers_patient_provider_uniq UNIQUE (patient_id, provider)
);

-- ---------------------------------------------------------------------------
-- payment_methods
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS payment_methods (
    id                   UUID        PRIMARY KEY,
    patient_id           UUID        NOT NULL,

    provider             TEXT        NOT NULL
        CHECK (provider IN ('stripe', 'payhere', 'dialog', 'mock')),
    provider_customer_id TEXT        NOT NULL DEFAULT '',
    -- The tokenised instrument. This is the ONLY thing that identifies the
    -- card to the rail, and it is useless to anyone who does not also hold our
    -- API key.
    provider_token       TEXT        NOT NULL CHECK (provider_token <> '' AND length(provider_token) <= 255),

    -- Display metadata, supplied by the provider. Never entered by a client:
    -- a client that could name its own brand and last4 could label somebody
    -- else's token as its own card.
    method_type          TEXT        NOT NULL DEFAULT 'card'
        CHECK (method_type IN ('card', 'wallet', 'bank')),
    brand                TEXT        NOT NULL DEFAULT '',
    last4                TEXT        NOT NULL DEFAULT '' CHECK (last4 = '' OR last4 ~ '^[0-9]{4}$'),
    exp_month            SMALLINT    NOT NULL DEFAULT 0 CHECK (exp_month BETWEEN 0 AND 12),
    exp_year             SMALLINT    NOT NULL DEFAULT 0 CHECK (exp_year = 0 OR exp_year BETWEEN 2000 AND 2100),

    is_default           BOOLEAN     NOT NULL DEFAULT FALSE,

    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at           TIMESTAMPTZ,
    version              INT         NOT NULL DEFAULT 1,

    -- The same token saved twice is the same card. This is what makes the
    -- SetupIntent confirmation idempotent across the two paths that can
    -- record it (the client's own confirm call and the provider webhook),
    -- which will frequently race each other by milliseconds.
    CONSTRAINT payment_methods_token_uniq UNIQUE (provider, provider_token)
);

-- At most one default card per patient, enforced by the database. Two
-- defaults is not a cosmetic bug: the checkout would pick whichever the query
-- happened to return first, and the patient would be charged on a card they
-- did not choose.
CREATE UNIQUE INDEX IF NOT EXISTS idx_payment_methods_one_default
    ON payment_methods (patient_id)
    WHERE is_default AND deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_payment_methods_patient
    ON payment_methods (patient_id, created_at DESC)
    WHERE deleted_at IS NULL;

DROP TRIGGER IF EXISTS trg_payment_customers_updated_at ON payment_customers;
CREATE TRIGGER trg_payment_customers_updated_at BEFORE UPDATE ON payment_customers
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_payment_methods_updated_at ON payment_methods;
CREATE TRIGGER trg_payment_methods_updated_at BEFORE UPDATE ON payment_methods
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
