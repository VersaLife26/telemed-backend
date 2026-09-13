ALTER TABLE users DROP CONSTRAINT IF EXISTS users_photo_pair;
ALTER TABLE users DROP COLUMN IF EXISTS photo_updated_at;
ALTER TABLE users DROP COLUMN IF EXISTS photo_data;
ALTER TABLE users DROP COLUMN IF EXISTS photo_content_type;
