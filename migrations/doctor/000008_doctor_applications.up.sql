-- Public doctor applications: collected BEFORE any OTP or user account.
-- On admin approval the applicant completes OTP; user-service then calls
-- Attach, which creates the doctors row (id = application id) already
-- approved and marks the application activated.

CREATE TABLE IF NOT EXISTS doctor_applications (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    phone              VARCHAR(20)  NOT NULL,
    email              VARCHAR(255) NOT NULL,
    display_name       VARCHAR(200) NOT NULL,
    slmc_number        VARCHAR(20)  NOT NULL,
    specialty          VARCHAR(50)  NOT NULL REFERENCES specialties(code),
    languages          TEXT[]       NOT NULL DEFAULT '{}'
                           CONSTRAINT doctor_applications_languages_valid
                           CHECK (languages <@ ARRAY['en','si','ta']::TEXT[]),
    experience_years   SMALLINT     NOT NULL DEFAULT 0 CHECK (experience_years >= 0),
    fee_cents          BIGINT       NOT NULL CHECK (fee_cents >= 0),
    bio                TEXT         NOT NULL DEFAULT '',
    status             VARCHAR(20)  NOT NULL DEFAULT 'pending'
                           CONSTRAINT doctor_applications_status_valid
                           CHECK (status IN ('pending', 'approved', 'rejected', 'activated')),
    rejection_reason   TEXT,
    decided_at         TIMESTAMPTZ,
    decided_by         UUID,
    activated_user_id  UUID,
    activated_at       TIMESTAMPTZ,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- One open application per phone / SLMC. Rejected or activated rows may reuse.
CREATE UNIQUE INDEX IF NOT EXISTS uq_doctor_applications_phone_open
    ON doctor_applications (phone)
    WHERE status IN ('pending', 'approved');

CREATE UNIQUE INDEX IF NOT EXISTS uq_doctor_applications_slmc_open
    ON doctor_applications (slmc_number)
    WHERE status IN ('pending', 'approved');

CREATE INDEX IF NOT EXISTS idx_doctor_applications_status
    ON doctor_applications (status, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_doctor_applications_phone
    ON doctor_applications (phone);

-- The grantee has to exist before it can be granted to, and in a container
-- brought up by testcontainers nothing has created it: the role is normally
-- provisioned by telemed-infra's postgres init script, which never runs there.
-- This migration has always failed with `role "telemed_doctor_app" does not
-- exist` under the integration suite -- it is not a consolidation regression,
-- it just went unnoticed because that suite is not part of the default
-- `go test ./...` run.
--
-- Guarded and idempotent, in the same shape migrations/admin/000002 already
-- uses for telemed_admin_app. A deployment where the role exists is unaffected.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = 'telemed_doctor_app') THEN
        CREATE ROLE telemed_doctor_app LOGIN PASSWORD 'changeme_in_deployment_secret_manager';
    END IF;
END
$$;

DO $$
BEGIN
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO telemed_doctor_app', current_database());
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO telemed_doctor_app', current_schema());
END
$$;

GRANT SELECT, INSERT, UPDATE ON doctor_applications TO telemed_doctor_app;
