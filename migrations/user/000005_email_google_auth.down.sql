ALTER TABLE users DROP CONSTRAINT IF EXISTS users_has_login_identity;

DROP INDEX IF EXISTS idx_users_google_sub_active;

ALTER TABLE users DROP COLUMN IF EXISTS email_verified_at;
ALTER TABLE users DROP COLUMN IF EXISTS google_sub;
ALTER TABLE users DROP COLUMN IF EXISTS password_hash;

-- Restore the original NOT NULL phone column. Rows created only via email or
-- Google cannot be reversed automatically; this down migration is for local
-- rollback of an unused schema, not for production accounts that already
-- signed in without a phone.
UPDATE users SET phone = 'unknown:' || id::text WHERE phone IS NULL OR phone = '';

DROP INDEX IF EXISTS idx_users_phone_active;
ALTER TABLE users ALTER COLUMN phone SET NOT NULL;
CREATE UNIQUE INDEX idx_users_phone_active
    ON users (phone)
    WHERE deleted_at IS NULL;
