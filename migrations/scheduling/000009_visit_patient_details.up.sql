-- Clinical details for the person the visit is for, captured at booking
-- alongside visit_patient_name/dob (000008). Snapshots, not references: a
-- later profile edit must not rewrite what the doctor was told at the time.
ALTER TABLE appointments
    ADD COLUMN IF NOT EXISTS visit_patient_sex TEXT,
    ADD COLUMN IF NOT EXISTS visit_patient_weight_kg NUMERIC(5,1),
    ADD COLUMN IF NOT EXISTS visit_patient_allergies TEXT;

ALTER TABLE appointments DROP CONSTRAINT IF EXISTS appointments_visit_patient_sex_valid;
ALTER TABLE appointments ADD CONSTRAINT appointments_visit_patient_sex_valid
    CHECK (visit_patient_sex IN ('female','male','other'));
