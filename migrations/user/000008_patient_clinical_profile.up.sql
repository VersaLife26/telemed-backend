-- Clinical details a patient can keep on their profile so booking and
-- prescribing can prefill them. Both nullable: existing accounts have neither.
ALTER TABLE users ADD COLUMN IF NOT EXISTS sex TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS allergies TEXT;

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_sex_valid;
ALTER TABLE users ADD CONSTRAINT users_sex_valid CHECK (sex IN ('female','male','other'));
