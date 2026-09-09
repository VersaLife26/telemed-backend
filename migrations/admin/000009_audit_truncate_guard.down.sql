-- Reverses 000009 exactly: the TRUNCATE guards, the forward-only anchor guard,
-- the anchor columns, and the SECURITY DEFINER form of the chain trigger.
--
-- Rolling this back restores 000002's behaviour, which means the audit log
-- becomes truncatable again and an empty table verifies as valid. That is what
-- "reverse the up migration" has to mean here; it is not a state to leave a
-- production database in.

DROP TRIGGER IF EXISTS trg_audit_chain_state_no_truncate ON audit_chain_state;
DROP TRIGGER IF EXISTS trg_audit_logs_no_truncate ON audit_logs;
DROP FUNCTION IF EXISTS audit_block_truncate();

-- The forward-only guard has to go before the columns it reads.
DROP TRIGGER IF EXISTS trg_audit_chain_state_forward_only ON audit_chain_state;
DROP FUNCTION IF EXISTS audit_chain_state_forward_only();

-- Restore 000002's chain trigger verbatim: caller privileges, no anchor
-- maintenance. Recreated rather than dropped, because dropping it would leave
-- audit_logs with NOT NULL prev_hash/row_hash columns and nothing to populate
-- them, so every subsequent insert would fail.
CREATE OR REPLACE FUNCTION audit_logs_chain_trigger() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
DECLARE
    v_prev_hash TEXT;
    v_canonical TEXT;
BEGIN
    SELECT last_hash INTO v_prev_hash FROM audit_chain_state WHERE id = TRUE FOR UPDATE;

    NEW.created_at := COALESCE(NEW.created_at, NOW());
    NEW.prev_hash := v_prev_hash;

    v_canonical := audit_row_canonical(
        NEW.id, NEW.actor_id, NEW.actor_role, NEW.action, NEW.resource_type,
        NEW.resource_id, NEW.old_value, NEW.new_value, NEW.ip, NEW.user_agent,
        NEW.request_id, NEW.created_at
    );

    NEW.row_hash := audit_row_hash(v_prev_hash, v_canonical);

    UPDATE audit_chain_state SET last_hash = NEW.row_hash WHERE id = TRUE;

    RETURN NEW;
END;
$$;

ALTER TABLE audit_chain_state
    DROP COLUMN IF EXISTS last_id,
    DROP COLUMN IF EXISTS entry_count;

-- 000002's grant list: the app role wrote audit_chain_state directly then.
GRANT SELECT, UPDATE ON audit_chain_state TO telemed_admin_app;

REVOKE ALL ON outbox_events FROM telemed_admin_app;
