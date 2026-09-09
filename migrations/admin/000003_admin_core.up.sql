-- Core admin schema: the local identity/authorization table for admin
-- console users, disputes, versioned system configuration, and doctor
-- credentialing checklists.
--
-- None of these tables carry a foreign key to another service's data.
-- appointment_id/patient_id/doctor_id below are UUIDs that *mean* a row in
-- telemed_scheduling/telemed_user/telemed_doctor, per ADR-004: each service
-- owns its own database, and cross-service relationships are enforced by the
-- service layer and events, never by a database constraint that would require
-- a cross-database join Postgres cannot perform anyway.

-- ---------------------------------------------------------------------------
-- admin_users: links a verified Keycloak identity to a platform admin role
-- and an optional per-admin IP scope. RequireAuth + RequireRole already
-- authorize purely from the JWT; this table adds what the JWT cannot carry:
-- the ability for a super_admin to deactivate a single admin account without
-- touching Keycloak, and an additional CIDR scope narrower than the global
-- office/VPN allowlist for a specific admin (e.g. a support contractor).
-- ---------------------------------------------------------------------------
CREATE TABLE admin_users (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    keycloak_subject  TEXT        NOT NULL,
    email             TEXT        NOT NULL,
    display_name      TEXT        NOT NULL DEFAULT '',
    role              TEXT        NOT NULL
                          CHECK (role IN ('admin', 'super_admin', 'ops', 'finance', 'support')),
    -- Additional CIDRs scoping this specific admin, on top of the global
    -- allowlist enforced by middleware.IPAllowlist. Empty means "no extra
    -- restriction beyond the global list".
    ip_allowlist      TEXT[]      NOT NULL DEFAULT '{}',
    active            BOOLEAN     NOT NULL DEFAULT TRUE,
    last_login_at     TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at        TIMESTAMPTZ,
    version           INT         NOT NULL DEFAULT 1
);

CREATE UNIQUE INDEX idx_admin_users_keycloak_subject ON admin_users (keycloak_subject) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX idx_admin_users_email ON admin_users (lower(email)) WHERE deleted_at IS NULL;
CREATE INDEX idx_admin_users_role ON admin_users (role) WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- disputes
-- ---------------------------------------------------------------------------
CREATE TABLE disputes (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    appointment_id      UUID        NOT NULL,
    patient_id          UUID        NOT NULL,
    doctor_id           UUID        NOT NULL,
    category            TEXT        NOT NULL
                            CHECK (category IN ('billing', 'quality_of_care', 'no_show', 'technical', 'other')),
    description         TEXT        NOT NULL,
    status              TEXT        NOT NULL DEFAULT 'open'
                            CHECK (status IN ('open', 'investigating', 'resolved', 'closed')),
    assigned_to         UUID REFERENCES admin_users (id),
    resolution          TEXT,
    refund_requested    BOOLEAN     NOT NULL DEFAULT FALSE,
    refund_amount_cents BIGINT,
    currency            TEXT        NOT NULL DEFAULT 'LKR',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at          TIMESTAMPTZ,
    version             INT         NOT NULL DEFAULT 1,
    CONSTRAINT chk_disputes_refund_amount CHECK (refund_amount_cents IS NULL OR refund_amount_cents >= 0)
);

CREATE INDEX idx_disputes_status ON disputes (status, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_disputes_assigned_to ON disputes (assigned_to) WHERE deleted_at IS NULL;
CREATE INDEX idx_disputes_appointment ON disputes (appointment_id);
CREATE INDEX idx_disputes_patient ON disputes (patient_id);
CREATE INDEX idx_disputes_doctor ON disputes (doctor_id);

CREATE TABLE dispute_comments (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    dispute_id       UUID        NOT NULL REFERENCES disputes (id),
    author_admin_id  UUID        NOT NULL REFERENCES admin_users (id),
    body             TEXT        NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at       TIMESTAMPTZ,
    version          INT         NOT NULL DEFAULT 1
);

CREATE INDEX idx_dispute_comments_dispute ON dispute_comments (dispute_id, created_at) WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- system_configs: versioned configuration. A "PUT" is always an INSERT of a
-- new version; the previous row is never touched. The current value for a key
-- is the row with the greatest effective_from <= now() (falling back to the
-- greatest version if effective_from ties), which is a pure read -- there is
-- no "close out the previous version" write to forget. The UPDATE/DELETE
-- trigger below makes "never mutated in place" a database fact, not a
-- promise the repository keeps by convention.
-- ---------------------------------------------------------------------------
CREATE TABLE system_configs (
    id              BIGSERIAL PRIMARY KEY,
    key             TEXT        NOT NULL,
    value           JSONB       NOT NULL,
    version         INT         NOT NULL,
    updated_by      UUID REFERENCES admin_users (id),
    effective_from  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (key, version)
);

CREATE INDEX idx_system_configs_key_effective ON system_configs (key, effective_from DESC, version DESC);

CREATE OR REPLACE FUNCTION block_mutation_generic() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not permitted (row id=%)',
        TG_TABLE_NAME, TG_OP, COALESCE(OLD.id::TEXT, '?')
        USING HINT = 'insert a new version instead of mutating an existing row.';
END;
$$;

CREATE TRIGGER trg_system_configs_no_update
    BEFORE UPDATE ON system_configs
    FOR EACH ROW EXECUTE FUNCTION block_mutation_generic();

CREATE TRIGGER trg_system_configs_no_delete
    BEFORE DELETE ON system_configs
    FOR EACH ROW EXECUTE FUNCTION block_mutation_generic();

-- ---------------------------------------------------------------------------
-- verification_checklists: the admin console's own record of what has been
-- checked for a doctor's credentialing, independent of doctor-service's
-- verification_status (which is updated asynchronously, via the
-- doctor.approved/doctor.rejected event this service publishes on decision).
--
-- A partial unique index rather than a table-wide UNIQUE(doctor_id) allows
-- yearly re-verification (SDD section 20) to open a new checklist per doctor
-- over time while still preventing two *pending* checklists for the same
-- doctor from existing simultaneously.
-- ---------------------------------------------------------------------------
CREATE TABLE verification_checklists (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    doctor_id                 UUID        NOT NULL,

    slmc_format_valid         BOOLEAN,
    slmc_format_checked_by    UUID REFERENCES admin_users (id),
    slmc_format_checked_at    TIMESTAMPTZ,

    slmc_registry_checked     BOOLEAN,
    slmc_registry_checked_by  UUID REFERENCES admin_users (id),
    slmc_registry_checked_at  TIMESTAMPTZ,

    experience_verified       BOOLEAN,
    experience_checked_by     UUID REFERENCES admin_users (id),
    experience_checked_at     TIMESTAMPTZ,

    nic_matches                BOOLEAN,
    nic_checked_by             UUID REFERENCES admin_users (id),
    nic_checked_at             TIMESTAMPTZ,

    photo_clear                BOOLEAN,
    photo_checked_by           UUID REFERENCES admin_users (id),
    photo_checked_at           TIMESTAMPTZ,

    overall_status              TEXT        NOT NULL DEFAULT 'pending'
                                    CHECK (overall_status IN ('pending', 'approved', 'rejected')),
    decision_reason              TEXT,
    decided_by                   UUID REFERENCES admin_users (id),
    decided_at                   TIMESTAMPTZ,

    created_at                   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at                   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at                   TIMESTAMPTZ,
    version                      INT         NOT NULL DEFAULT 1
);

CREATE UNIQUE INDEX idx_verification_checklists_doctor_pending
    ON verification_checklists (doctor_id) WHERE overall_status = 'pending';
CREATE INDEX idx_verification_checklists_doctor ON verification_checklists (doctor_id, created_at DESC);
CREATE INDEX idx_verification_checklists_status ON verification_checklists (overall_status, created_at DESC);

-- ---------------------------------------------------------------------------
-- Grants for the restricted app role. system_configs is intentionally missing
-- UPDATE/DELETE -- the trigger above is the enforcement, this GRANT list is
-- the first line of defence.
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE ON admin_users TO telemed_admin_app;
GRANT SELECT, INSERT, UPDATE ON disputes TO telemed_admin_app;
GRANT SELECT, INSERT, UPDATE ON dispute_comments TO telemed_admin_app;
GRANT SELECT, INSERT ON system_configs TO telemed_admin_app;
GRANT SELECT, INSERT, UPDATE ON verification_checklists TO telemed_admin_app;
GRANT USAGE, SELECT ON SEQUENCE system_configs_id_seq TO telemed_admin_app;
