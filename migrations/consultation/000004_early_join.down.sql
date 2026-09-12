ALTER TABLE consultations DROP CONSTRAINT IF EXISTS ck_consultations_early_join_offered;
ALTER TABLE consultations DROP CONSTRAINT IF EXISTS ck_consultations_early_join_response;
ALTER TABLE consultations DROP COLUMN IF EXISTS early_join_responded_at;
ALTER TABLE consultations DROP COLUMN IF EXISTS early_join_response;
ALTER TABLE consultations DROP COLUMN IF EXISTS early_join_offered_at;
