-- Remove only the version-1 seed rows this migration inserted.
DELETE FROM system_configs
WHERE version = 1
  AND key IN (
    'slot_defaults',
    'cancellation_policy',
    'fee_caps',
    'feature_flags',
    'corporate_clients',
    'commission_rules'
  );
