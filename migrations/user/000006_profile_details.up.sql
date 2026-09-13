-- Patient self-service profile fields. Name and phone already exist; address
-- and date of birth do not, and the patient web profile cannot store them
-- without this. date_of_birth is nullable because existing accounts have none.

ALTER TABLE users ADD COLUMN IF NOT EXISTS address TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS date_of_birth DATE;
