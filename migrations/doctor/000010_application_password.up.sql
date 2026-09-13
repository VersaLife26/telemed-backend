-- Password chosen on the public apply form. Copied onto users.password_hash
-- when an admin approves, then cleared so the hash is not kept in two places.
ALTER TABLE doctor_applications
    ADD COLUMN IF NOT EXISTS password_hash TEXT;
