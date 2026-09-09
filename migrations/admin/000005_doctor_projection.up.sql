-- Doctor credentialing queue projection, fed by the doctor.registered event
-- published by doctor-service (subject already declared in
-- internal/platform/events/events.go). This service never queries
-- telemed_doctor directly -- per ADR-004 there is no cross-service join
-- available -- and it never stores a doctor's raw NIC number: only the
-- MinIO object keys for the uploaded documents, which the admin console
-- turns into short-lived presigned URLs for the verification queue's
-- side-by-side document viewer.
--
-- verification_status here is a locally-projected mirror for display and
-- queue filtering. The authoritative status lives in doctor-service; this
-- service changes it only by publishing doctor.approved/doctor.rejected
-- (via the outbox, see internal/credentialing) and updating its own copy in
-- the same local transaction. If doctor-service and this projection ever
-- disagree, doctor-service wins -- this table is a read-optimised cache of
-- an event stream, not a second source of truth.

CREATE TABLE doctor_projection (
    doctor_id             UUID PRIMARY KEY,
    event_id              UUID        NOT NULL,
    full_name             TEXT        NOT NULL,
    email                 TEXT,
    phone                 TEXT,
    slmc_number           TEXT        NOT NULL,
    years_experience      INT,
    specialty_code        TEXT,
    verification_status   TEXT        NOT NULL DEFAULT 'pending'
                              CHECK (verification_status IN ('pending', 'approved', 'rejected')),
    slmc_certificate_key  TEXT,
    nic_document_key      TEXT,
    degree_certificate_key TEXT,
    photo_key             TEXT,
    registered_at         TIMESTAMPTZ NOT NULL,
    ingested_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_doctor_projection_status ON doctor_projection (verification_status, registered_at);
CREATE UNIQUE INDEX idx_doctor_projection_slmc ON doctor_projection (slmc_number);

GRANT SELECT, INSERT, UPDATE ON doctor_projection TO telemed_admin_app;
