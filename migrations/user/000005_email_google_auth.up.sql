-- Email/password and Google sign-in sit alongside phone OTP. Phone stays the
-- original identifier for existing accounts, but it is no longer required:
-- a patient who only has Gmail, or a doctor who logs in with the email on
-- their profile, must still get a session from this service (the platform's
-- sole patient/doctor JWT issuer).

ALTER TABLE users ALTER COLUMN phone DROP NOT NULL;

DROP INDEX IF EXISTS idx_users_phone_active;
CREATE UNIQUE INDEX idx_users_phone_active
    ON users (phone)
    WHERE deleted_at IS NULL AND phone IS NOT NULL AND phone <> '';

ALTER TABLE users ADD COLUMN IF NOT EXISTS password_hash TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS google_sub TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS email_verified_at TIMESTAMPTZ;

CREATE UNIQUE INDEX IF NOT EXISTS idx_users_google_sub_active
    ON users (google_sub)
    WHERE deleted_at IS NULL AND google_sub IS NOT NULL;

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_has_login_identity;
ALTER TABLE users ADD CONSTRAINT users_has_login_identity CHECK (
    (phone IS NOT NULL AND phone <> '')
    OR email IS NOT NULL
    OR google_sub IS NOT NULL
);
