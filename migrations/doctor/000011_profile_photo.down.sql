ALTER TABLE doctors DROP CONSTRAINT IF EXISTS doctors_photo_pair;
ALTER TABLE doctors DROP COLUMN IF EXISTS photo_data;
ALTER TABLE doctors DROP COLUMN IF EXISTS photo_content_type;
ALTER TABLE doctors DROP COLUMN IF EXISTS photo_updated_at;
