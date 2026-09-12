DROP INDEX IF EXISTS idx_consultations_running_late;
ALTER TABLE consultations DROP CONSTRAINT IF EXISTS ck_consultations_scheduled_range;
ALTER TABLE consultations DROP COLUMN IF EXISTS running_late_notified_at;
ALTER TABLE consultations DROP COLUMN IF EXISTS scheduled_end_at;
