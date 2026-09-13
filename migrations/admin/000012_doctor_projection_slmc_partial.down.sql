DROP INDEX IF EXISTS idx_doctor_projection_slmc_pending;
DROP INDEX IF EXISTS idx_doctor_projection_slmc_approved;
CREATE UNIQUE INDEX idx_doctor_projection_slmc ON doctor_projection (slmc_number);
