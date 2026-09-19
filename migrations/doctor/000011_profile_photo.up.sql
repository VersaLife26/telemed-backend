-- Directory portrait. Bytes live in photo_data and are fetched only by
-- GET /doctors/{id}/photo; profile and search reads carry photo_url as a
-- cache-busted path so listings never pull multi-megabyte blobs.

ALTER TABLE doctors ADD COLUMN IF NOT EXISTS photo_content_type TEXT;
ALTER TABLE doctors ADD COLUMN IF NOT EXISTS photo_data BYTEA;
ALTER TABLE doctors ADD COLUMN IF NOT EXISTS photo_updated_at TIMESTAMPTZ;

ALTER TABLE doctors DROP CONSTRAINT IF EXISTS doctors_photo_pair;
ALTER TABLE doctors ADD CONSTRAINT doctors_photo_pair CHECK (
  (photo_data IS NULL AND photo_content_type IS NULL AND photo_updated_at IS NULL)
  OR (photo_data IS NOT NULL AND photo_content_type IS NOT NULL AND photo_updated_at IS NOT NULL)
);
