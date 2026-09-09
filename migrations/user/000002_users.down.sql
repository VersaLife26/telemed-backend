DROP INDEX IF EXISTS idx_consents_user;
DROP TABLE IF EXISTS consents;

DROP INDEX IF EXISTS idx_otp_attempts_created_brin;
DROP INDEX IF EXISTS idx_otp_attempts_phone_time;
DROP TABLE IF EXISTS otp_attempts;

DROP INDEX IF EXISTS idx_refresh_tokens_expiry;
DROP INDEX IF EXISTS idx_refresh_tokens_family;
DROP INDEX IF EXISTS idx_refresh_tokens_user;
DROP INDEX IF EXISTS idx_refresh_tokens_hash;
DROP TABLE IF EXISTS refresh_tokens;

DROP INDEX IF EXISTS idx_family_members_owner;
DROP TABLE IF EXISTS family_members;

DROP INDEX IF EXISTS idx_users_erasure_due;
DROP INDEX IF EXISTS idx_users_keycloak_id;
DROP INDEX IF EXISTS idx_users_email_active;
DROP INDEX IF EXISTS idx_users_phone_active;
DROP TABLE IF EXISTS users;
