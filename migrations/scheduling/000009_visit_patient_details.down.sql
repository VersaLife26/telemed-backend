ALTER TABLE appointments DROP CONSTRAINT IF EXISTS appointments_visit_patient_sex_valid;
ALTER TABLE appointments
    DROP COLUMN IF EXISTS visit_patient_allergies,
    DROP COLUMN IF EXISTS visit_patient_weight_kg,
    DROP COLUMN IF EXISTS visit_patient_sex;
