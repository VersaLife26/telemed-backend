-- Reversing an append-only audit log is inherently destructive -- there is no
-- non-destructive way to "undo" table creation without losing every row. This
-- down migration is provided (and tested) purely so `migrate down` does not
-- leave the schema half-applied during development; it must never run against
-- a database holding real audit history.

DROP TRIGGER IF EXISTS trg_audit_logs_no_delete ON audit_logs;
DROP TRIGGER IF EXISTS trg_audit_logs_no_update ON audit_logs;
DROP TRIGGER IF EXISTS trg_audit_logs_chain ON audit_logs;

DROP FUNCTION IF EXISTS audit_logs_block_mutation();
DROP FUNCTION IF EXISTS audit_logs_chain_trigger();
DROP FUNCTION IF EXISTS audit_row_hash(TEXT, TEXT);
DROP FUNCTION IF EXISTS audit_row_canonical(BIGINT, UUID, TEXT, TEXT, TEXT, TEXT, JSONB, JSONB, INET, TEXT, TEXT, TIMESTAMPTZ);

DROP INDEX IF EXISTS idx_audit_logs_request_id;
DROP INDEX IF EXISTS idx_audit_logs_resource;
DROP INDEX IF EXISTS idx_audit_logs_action;
DROP INDEX IF EXISTS idx_audit_logs_actor;
DROP INDEX IF EXISTS idx_audit_logs_created_at_brin;

DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS audit_chain_state;

-- The role is intentionally left in place: dropping a role that may still own
-- privileges/objects in other migrations (000003+) here would make the down
-- sequence order-dependent. A full teardown drops the role at the database
-- level once every migration that grants to it has been reversed.
