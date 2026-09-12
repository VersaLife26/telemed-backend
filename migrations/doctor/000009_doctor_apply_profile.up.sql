-- Extra fields collected on public doctor apply, plus credential images
-- stored until admin review (bytes live here; object storage is used later
-- when a doctor profile exists).

ALTER TABLE doctor_applications
    DROP CONSTRAINT IF EXISTS doctor_applications_languages_valid;

ALTER TABLE doctor_applications
    ADD CONSTRAINT doctor_applications_languages_valid
    CHECK (languages <@ ARRAY['en','si','ta','other']::TEXT[]);

ALTER TABLE doctor_applications
    ADD COLUMN IF NOT EXISTS first_name VARCHAR(100) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS last_name VARCHAR(100) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS language_other VARCHAR(100) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS pgim_board_certified BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS medical_school VARCHAR(200) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS qualifications_text TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS required_fee_cents BIGINT NOT NULL DEFAULT 0 CHECK (required_fee_cents >= 0),
    ADD COLUMN IF NOT EXISTS availability_notes TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS is_general_practitioner BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS practicing_locations TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS terms_accepted_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS bank_encrypted TEXT,
    ADD COLUMN IF NOT EXISTS bank_name VARCHAR(100) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS bank_branch VARCHAR(100) NOT NULL DEFAULT '';

ALTER TABLE doctors
    DROP CONSTRAINT IF EXISTS doctors_languages_valid;

ALTER TABLE doctors
    ADD CONSTRAINT doctors_languages_valid
    CHECK (languages <@ ARRAY['en','si','ta','other']::TEXT[]);

CREATE TABLE IF NOT EXISTS doctor_application_documents (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id  UUID NOT NULL REFERENCES doctor_applications(id) ON DELETE CASCADE,
    document_type   VARCHAR(50) NOT NULL
                      CHECK (document_type IN ('signature','seal','slmc_certificate')),
    filename        VARCHAR(255) NOT NULL DEFAULT '',
    content_type    VARCHAR(100) NOT NULL DEFAULT 'application/octet-stream',
    bytes           BYTEA NOT NULL,
    uploaded_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (application_id, document_type)
);

CREATE INDEX IF NOT EXISTS idx_doctor_application_documents_app
    ON doctor_application_documents (application_id);

DO $$
BEGIN
    IF EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = 'telemed_doctor_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON doctor_application_documents TO telemed_doctor_app;
    END IF;
END
$$;
