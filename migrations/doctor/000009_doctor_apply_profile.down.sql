DROP TABLE IF EXISTS doctor_application_documents;

ALTER TABLE doctor_applications
    DROP COLUMN IF EXISTS first_name,
    DROP COLUMN IF EXISTS last_name,
    DROP COLUMN IF EXISTS language_other,
    DROP COLUMN IF EXISTS pgim_board_certified,
    DROP COLUMN IF EXISTS medical_school,
    DROP COLUMN IF EXISTS qualifications_text,
    DROP COLUMN IF EXISTS required_fee_cents,
    DROP COLUMN IF EXISTS availability_notes,
    DROP COLUMN IF EXISTS is_general_practitioner,
    DROP COLUMN IF EXISTS practicing_locations,
    DROP COLUMN IF EXISTS terms_accepted_at,
    DROP COLUMN IF EXISTS bank_encrypted,
    DROP COLUMN IF EXISTS bank_name,
    DROP COLUMN IF EXISTS bank_branch;

ALTER TABLE doctor_applications
    DROP CONSTRAINT IF EXISTS doctor_applications_languages_valid;

ALTER TABLE doctor_applications
    ADD CONSTRAINT doctor_applications_languages_valid
    CHECK (languages <@ ARRAY['en','si','ta']::TEXT[]);

ALTER TABLE doctors
    DROP CONSTRAINT IF EXISTS doctors_languages_valid;

ALTER TABLE doctors
    ADD CONSTRAINT doctors_languages_valid
    CHECK (languages <@ ARRAY['en','si','ta']::TEXT[]);
