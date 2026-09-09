-- Seed version-1 defaults for every system_configs key the admin console reads.
-- GET /configs/{key} returns 404 when no row exists; the UI treats that as a
-- broken route. These rows are the platform baseline an ops admin may edit.

INSERT INTO system_configs (key, value, version, updated_by, effective_from)
SELECT 'slot_defaults', '{"slot_duration_minutes":15,"buffer_minutes":5,"horizon_days":30,"max_per_day":24}'::jsonb, 1, NULL, NOW()
WHERE NOT EXISTS (SELECT 1 FROM system_configs WHERE key = 'slot_defaults');

INSERT INTO system_configs (key, value, version, updated_by, effective_from)
SELECT 'cancellation_policy', '{"free_cancellation_hours":24,"late_cancellation_fee_percent":50,"no_show_fee_percent":100}'::jsonb, 1, NULL, NOW()
WHERE NOT EXISTS (SELECT 1 FROM system_configs WHERE key = 'cancellation_policy');

INSERT INTO system_configs (key, value, version, updated_by, effective_from)
SELECT 'fee_caps', '{"currency":"LKR","min_fee_cents":50000,"max_fee_cents":5000000,"per_specialty_max_cents":{}}'::jsonb, 1, NULL, NOW()
WHERE NOT EXISTS (SELECT 1 FROM system_configs WHERE key = 'fee_caps');

INSERT INTO system_configs (key, value, version, updated_by, effective_from)
SELECT 'feature_flags', '[{"key":"waitlist_v2","enabled":false,"description":"Use the v2 waitlist flow when a doctor day is fully booked."}]'::jsonb, 1, NULL, NOW()
WHERE NOT EXISTS (SELECT 1 FROM system_configs WHERE key = 'feature_flags');

INSERT INTO system_configs (key, value, version, updated_by, effective_from)
SELECT 'corporate_clients', '[]'::jsonb, 1, NULL, NOW()
WHERE NOT EXISTS (SELECT 1 FROM system_configs WHERE key = 'corporate_clients');

INSERT INTO system_configs (key, value, version, updated_by, effective_from)
SELECT 'commission_rules', '{"default_commission_percent":20,"rules":[]}'::jsonb, 1, NULL, NOW()
WHERE NOT EXISTS (SELECT 1 FROM system_configs WHERE key = 'commission_rules');
