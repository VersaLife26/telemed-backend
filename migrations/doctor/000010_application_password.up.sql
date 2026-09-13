-- Store the bcrypt hash of the password chosen at public apply so OTP
-- activation can copy it onto users.password_hash. Plaintext is never stored.
-- Cleared when the application is activated.

ALTER TABLE doctor_applications
    ADD COLUMN IF NOT EXISTS password_hash TEXT;

COMMENT ON COLUMN doctor_applications.password_hash IS
    'bcrypt hash of the password the doctor chose at apply; copied to users at OTP attach, then cleared';
