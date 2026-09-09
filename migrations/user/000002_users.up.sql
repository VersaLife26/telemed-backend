-- Identity, phone-OTP authentication, sessions, and family profiles.
--
-- This service is the sole owner of these tables. Other services that need a
-- user's name or phone read it over gRPC (user.v1.GetUser/GetUsersBatch) or
-- via the user.registered event -- never via a cross-database join.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- users -----------------------------------------------------------------
CREATE TABLE IF NOT EXISTS users (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    phone          TEXT        NOT NULL,
    email          TEXT,
    name           TEXT        NOT NULL,
    -- NIC (National Identity Card) is bcrypt-hashed, never stored plaintext.
    -- It is used only to *confirm* a NIC a user re-enters, never to display it.
    nic_hash       TEXT,
    language       TEXT        NOT NULL DEFAULT 'en'
                       CHECK (language IN ('en', 'si', 'ta')),
    role           TEXT        NOT NULL DEFAULT 'patient'
                       CHECK (role IN ('patient', 'doctor', 'admin', 'super_admin', 'ops', 'finance', 'support')),
    status         TEXT        NOT NULL DEFAULT 'active'
                       CHECK (status IN ('active', 'suspended', 'deleted')),
    no_show_count  INT         NOT NULL DEFAULT 0,
    keycloak_id    TEXT,

    -- PDPA erasure: set when a user requests deletion. A background reaper
    -- anonymises the row once erasure_due_at has passed, giving support a
    -- grace window to reverse an accidental or fraudulent deletion request.
    erasure_due_at TIMESTAMPTZ,
    anonymized_at  TIMESTAMPTZ,

    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at     TIMESTAMPTZ,
    version        INT         NOT NULL DEFAULT 0
);

-- Partial unique indexes: soft-deleted / anonymised rows must not block a new
-- registration from reusing the same phone or email.
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_phone_active
    ON users (phone) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_email_active
    ON users (email) WHERE deleted_at IS NULL AND email IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_keycloak_id
    ON users (keycloak_id) WHERE keycloak_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_users_erasure_due
    ON users (erasure_due_at) WHERE erasure_due_at IS NOT NULL AND anonymized_at IS NULL;

-- family_members ----------------------------------------------------------
-- A patient books appointments for a child or an elderly parent. This is
-- heavily used in Sri Lanka, where the account holder is rarely the patient.
CREATE TABLE IF NOT EXISTS family_members (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_id  UUID        NOT NULL REFERENCES users (id),
    name           TEXT        NOT NULL,
    dob            DATE        NOT NULL,
    relation       TEXT        NOT NULL
                       CHECK (relation IN ('child', 'parent', 'spouse', 'sibling', 'other')),
    nic_hash       TEXT,

    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at     TIMESTAMPTZ,
    version        INT         NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_family_members_owner
    ON family_members (owner_user_id) WHERE deleted_at IS NULL;

-- refresh_tokens ------------------------------------------------------------
-- Only the SHA-256 hash of a refresh token is ever stored. family_id groups
-- every token descended from one login so a detected replay can revoke the
-- whole lineage in one statement, which is the standard defence against
-- stolen-refresh-token reuse.
CREATE TABLE IF NOT EXISTS refresh_tokens (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        UUID        NOT NULL REFERENCES users (id),
    family_id      UUID        NOT NULL,
    token_hash     TEXT        NOT NULL,
    device_id_hash TEXT,
    issued_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at     TIMESTAMPTZ NOT NULL,
    revoked_at     TIMESTAMPTZ,
    replaced_by    UUID        REFERENCES refresh_tokens (id),

    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_refresh_tokens_hash ON refresh_tokens (token_hash);
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_user ON refresh_tokens (user_id);
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_family ON refresh_tokens (family_id);
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_expiry
    ON refresh_tokens (expires_at) WHERE revoked_at IS NULL;

-- otp_attempts --------------------------------------------------------------
-- Append-only audit trail for abuse investigation. The OTP code itself is
-- never written here or anywhere else in plaintext -- only that an attempt
-- happened, whether it succeeded, and where from.
CREATE TABLE IF NOT EXISTS otp_attempts (
    id         BIGSERIAL PRIMARY KEY,
    phone      TEXT        NOT NULL,
    purpose    TEXT        NOT NULL CHECK (purpose IN ('register', 'login')),
    action     TEXT        NOT NULL CHECK (action IN ('send', 'verify')),
    ip         TEXT,
    success    BOOLEAN     NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_otp_attempts_phone_time ON otp_attempts (phone, created_at DESC);
-- BRIN suits a high-volume, append-only, time-ordered audit table far better
-- than a B-tree: it is a few KB regardless of table size.
CREATE INDEX IF NOT EXISTS idx_otp_attempts_created_brin ON otp_attempts USING BRIN (created_at);

-- consents --------------------------------------------------------------
-- PDPA/GDPR consent ledger. Append-only by design: a withdrawn consent is a
-- new row with granted=false, never an UPDATE, so the platform can always
-- answer "what had this user agreed to, and when" for a regulator.
CREATE TABLE IF NOT EXISTS consents (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID        NOT NULL REFERENCES users (id),
    kind       TEXT        NOT NULL,
    version    TEXT        NOT NULL,
    granted    BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_consents_user ON consents (user_id, kind, created_at DESC);
