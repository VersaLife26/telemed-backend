-- Patient-side lookup for GET /admin/users/{id}/activity. The original
-- appointments_projection indexes cover doctor_id (utilisation dashboards)
-- but not patient_id, so a patient activity query would seq-scan.

CREATE INDEX idx_appointments_projection_patient
    ON appointments_projection (patient_id)
    WHERE patient_id IS NOT NULL;
