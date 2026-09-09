-- Record service schema: documents (medical files), prescriptions and their
-- items, the Sri Lankan drug formulary, the append-only access log, patient
-- controlled shares, and the local read-model that tells us which doctor
-- treated which patient (populated from consultation.* events, since this
-- service does not hold a foreign key into scheduling/consultation's
-- database -- see AGENT-BRIEF ADR-004: no cross-service foreign keys).

-- updated_at is maintained by trigger everywhere so a repository can never
-- forget to bump it on write.
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ============================================================================
-- documents
-- ============================================================================
CREATE TABLE IF NOT EXISTS documents (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_id     UUID        NOT NULL, -- the patient the record belongs to
    uploaded_by       UUID        NOT NULL, -- who performed the upload (patient or treating doctor)
    document_type     TEXT        NOT NULL CHECK (document_type IN ('report', 'scan', 'prescription', 'credential', 'recording')),
    bucket            TEXT        NOT NULL,
    object_key        TEXT        NOT NULL,
    filename          TEXT        NOT NULL,
    content_type      TEXT        NOT NULL,
    size_bytes        BIGINT      NOT NULL CHECK (size_bytes > 0),
    checksum_sha256   TEXT        NOT NULL,
    scan_status       TEXT        NOT NULL DEFAULT 'pending' CHECK (scan_status IN ('pending', 'clean', 'infected', 'skipped')),
    fhir_reference_id TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at        TIMESTAMPTZ,
    version           INT         NOT NULL DEFAULT 1,
    UNIQUE (bucket, object_key)
);

CREATE INDEX IF NOT EXISTS idx_documents_owner ON documents (owner_user_id, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_documents_type ON documents (owner_user_id, document_type) WHERE deleted_at IS NULL;

CREATE TRIGGER trg_documents_updated_at
    BEFORE UPDATE ON documents
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============================================================================
-- drugs -- Sri Lankan formulary reference table
-- ============================================================================
CREATE TABLE IF NOT EXISTS drugs (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          TEXT        NOT NULL,
    generic_name  TEXT        NOT NULL,
    strength      TEXT        NOT NULL,
    form          TEXT        NOT NULL, -- tablet, capsule, syrup, injection, cream, inhaler, drops
    manufacturer  TEXT,
    category      TEXT,       -- e.g. analgesic, antibiotic, antihypertensive
    is_controlled BOOLEAN     NOT NULL DEFAULT FALSE,
    is_generic    BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Trigram-free prefix + ILIKE search is adequate at formulary scale (a few
-- thousand rows); pg_trgm is not assumed available on every environment.
CREATE INDEX IF NOT EXISTS idx_drugs_name ON drugs (lower(name) text_pattern_ops);
CREATE INDEX IF NOT EXISTS idx_drugs_generic_name ON drugs (lower(generic_name) text_pattern_ops);

CREATE TRIGGER trg_drugs_updated_at
    BEFORE UPDATE ON drugs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============================================================================
-- prescriptions
-- ============================================================================
CREATE TABLE IF NOT EXISTS prescriptions (
    id                         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    appointment_id             UUID        NOT NULL UNIQUE,
    doctor_id                  UUID        NOT NULL,
    patient_id                 UUID        NOT NULL,
    -- Denormalised doctor display fields. This service does not own doctor
    -- profiles (doctor-service does, ADR-004: no cross-service joins), but
    -- the public verification endpoint must be able to answer "which doctor,
    -- what SLMC number" for a pharmacist days after issuance with no
    -- synchronous call to another service -- exactly the kind of read the
    -- outbox/event pattern exists to avoid needing. These are captured once,
    -- at issuance, from the issuing doctor's own request.
    doctor_name                TEXT        NOT NULL,
    doctor_slmc                TEXT        NOT NULL,
    doctor_qualifications      TEXT,
    issued_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    pdf_object_key             TEXT,
    verification_hmac          TEXT        NOT NULL,
    status                     TEXT        NOT NULL DEFAULT 'issued' CHECK (status IN ('issued', 'dispensed', 'cancelled')),
    fhir_medication_request_id TEXT,
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    version                    INT         NOT NULL DEFAULT 1
);

CREATE INDEX IF NOT EXISTS idx_prescriptions_doctor ON prescriptions (doctor_id, issued_at DESC);
CREATE INDEX IF NOT EXISTS idx_prescriptions_patient ON prescriptions (patient_id, issued_at DESC);

CREATE TRIGGER trg_prescriptions_updated_at
    BEFORE UPDATE ON prescriptions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============================================================================
-- prescription_items
-- ============================================================================
CREATE TABLE IF NOT EXISTS prescription_items (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    prescription_id UUID        NOT NULL REFERENCES prescriptions (id) ON DELETE CASCADE,
    drug_name       TEXT        NOT NULL,
    strength        TEXT        NOT NULL,
    form            TEXT        NOT NULL,
    dosage          TEXT        NOT NULL,
    frequency       TEXT        NOT NULL,
    duration_days   INT         NOT NULL CHECK (duration_days > 0),
    quantity        INT         NOT NULL CHECK (quantity > 0),
    instructions    TEXT,
    is_generic      BOOLEAN     NOT NULL DEFAULT FALSE,
    sort_order      INT         NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_prescription_items_prescription ON prescription_items (prescription_id, sort_order);

-- ============================================================================
-- document_access_log -- append-only. Under HIPAA/PDPA this log is what makes
-- a breach investigable, so it is defended at the database level, not just by
-- omitting UPDATE/DELETE handlers in application code.
-- ============================================================================
CREATE TABLE IF NOT EXISTS document_access_log (
    id               BIGSERIAL PRIMARY KEY,
    resource_type    TEXT        NOT NULL CHECK (resource_type IN ('document', 'prescription')),
    resource_id      UUID        NOT NULL,
    owner_user_id    UUID        NOT NULL,
    accessed_by      UUID,       -- NULL for the anonymous prescription-verification endpoint
    accessed_by_role TEXT        NOT NULL,
    action           TEXT        NOT NULL CHECK (action IN ('view', 'download', 'verify', 'list', 'upload')),
    granted          BOOLEAN     NOT NULL,
    reason           TEXT        NOT NULL, -- e.g. "owner", "treating_doctor", "share:<id>", "admin", "denied:no_relationship"
    ip_address        INET,
    user_agent       TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_access_log_resource ON document_access_log (resource_type, resource_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_access_log_owner ON document_access_log (owner_user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_access_log_created_at ON document_access_log USING BRIN (created_at);

-- Append-only enforcement: no application role, however misconfigured, can
-- rewrite or erase an access record once it lands.
CREATE OR REPLACE FUNCTION forbid_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'document_access_log is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_access_log_no_update
    BEFORE UPDATE ON document_access_log
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TRIGGER trg_access_log_no_delete
    BEFORE DELETE ON document_access_log
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ============================================================================
-- record_shares -- patient grants a doctor time-boxed access to their vault
-- ============================================================================
CREATE TABLE IF NOT EXISTS record_shares (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    patient_id  UUID        NOT NULL,
    doctor_id   UUID        NOT NULL,
    granted_by  UUID        NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    revoked_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    version     INT         NOT NULL DEFAULT 1,
    CHECK (expires_at > created_at)
);

CREATE INDEX IF NOT EXISTS idx_record_shares_patient ON record_shares (patient_id, doctor_id) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_record_shares_doctor_active ON record_shares (doctor_id, patient_id, expires_at) WHERE revoked_at IS NULL;

CREATE TRIGGER trg_record_shares_updated_at
    BEFORE UPDATE ON record_shares
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============================================================================
-- treating_relationships -- local read-model of "this doctor treated this
-- patient", built from consumed consultation.started / consultation.ended
-- events. This is what lets the authorization layer answer "may this doctor
-- read this patient's vault" without a synchronous call to another service's
-- database (ADR-004: no cross-service foreign keys, no cross-service joins).
-- ============================================================================
CREATE TABLE IF NOT EXISTS treating_relationships (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    appointment_id UUID        NOT NULL UNIQUE,
    doctor_id      UUID        NOT NULL,
    patient_id     UUID        NOT NULL,
    started_at     TIMESTAMPTZ,
    ended_at       TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_treating_doctor_patient ON treating_relationships (doctor_id, patient_id);
CREATE INDEX IF NOT EXISTS idx_treating_patient ON treating_relationships (patient_id);

CREATE TRIGGER trg_treating_relationships_updated_at
    BEFORE UPDATE ON treating_relationships
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============================================================================
-- consumed_events -- idempotency ledger for the NATS consumer. At-least-once
-- delivery is guaranteed by the broker; this table is what makes our
-- consumer's side effects exactly-once regardless of redelivery.
-- ============================================================================
CREATE TABLE IF NOT EXISTS consumed_events (
    event_id     UUID PRIMARY KEY,
    subject      TEXT        NOT NULL,
    consumed_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_consumed_events_subject ON consumed_events (subject, consumed_at DESC);
