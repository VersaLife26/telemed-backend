-- Patient self-service profile photograph. Bytes live in photo_data and are
-- fetched only by GET /users/me/photo; profile reads carry photo_updated_at
-- solely as a presence / cache-bust signal so directory lookups never pull
-- multi-megabyte blobs.

ALTER TABLE users ADD COLUMN IF NOT EXISTS photo_content_type TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS photo_data BYTEA;
ALTER TABLE users ADD COLUMN IF NOT EXISTS photo_updated_at TIMESTAMPTZ;

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_photo_pair;
ALTER TABLE users ADD CONSTRAINT users_photo_pair CHECK (
  (photo_data IS NULL AND photo_content_type IS NULL AND photo_updated_at IS NULL)
  OR (photo_data IS NOT NULL AND photo_content_type IS NOT NULL AND photo_updated_at IS NOT NULL)
);
