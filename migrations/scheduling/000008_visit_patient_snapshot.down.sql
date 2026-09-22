ALTER TABLE appointments
    DROP COLUMN IF EXISTS visit_patient_dob,
    DROP COLUMN IF EXISTS visit_patient_name;
