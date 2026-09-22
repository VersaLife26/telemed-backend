-- Who the consultation is for, captured at booking (account holder or someone else).
ALTER TABLE appointments
    ADD COLUMN IF NOT EXISTS visit_patient_name TEXT,
    ADD COLUMN IF NOT EXISTS visit_patient_dob DATE;
