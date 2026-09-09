-- Closes the hole security review F14 found in the append-only audit log, and
-- the completeness hole in the hash chain that made it invisible.
--
-- WHAT WAS WRONG
--
-- 000002 guards audit_logs with BEFORE UPDATE and BEFORE DELETE row triggers,
-- REVOKEs UPDATE/DELETE/TRUNCATE from the app role, and hash-chains every row.
-- Three things were still open:
--
--   1. NO TRUNCATE TRIGGER. Row-level BEFORE UPDATE/DELETE triggers do not
--      fire on TRUNCATE -- Postgres implements TRUNCATE as a relation-level
--      operation, not as a set of row deletions. So the two triggers that make
--      audit_logs "append-only regardless of role" were silent for the one
--      statement that removes every row at once. The REVOKE covers the app
--      role, but the owner and any superuser walked straight through.
--
--   2. THE CHAIN DID NOT DETECT TRUNCATION. audit_chain_state.last_hash chains
--      rows to each other; nothing anchored "how many rows there should be".
--      An empty audit_logs verified as VALID (`checked: 0`), because a walk
--      over zero rows finds zero broken links. Truncate-then-continue was
--      caught only accidentally -- the first new row's prev_hash no longer
--      matched genesis -- and even that was defeatable, because the app role
--      held UPDATE on audit_chain_state and could simply reset last_hash to
--      the genesis value and re-chain from scratch.
--
--   3. THE APP ROLE COULD WRITE audit_chain_state DIRECTLY. It needed UPDATE
--      there only because the BEFORE INSERT chain trigger ran with the
--      caller's privileges. That is more privilege than the requirement.
--
-- WHAT THIS MIGRATION DOES
--
--   a) BEFORE TRUNCATE ... FOR EACH STATEMENT triggers on audit_logs and on
--      audit_chain_state, raising unconditionally. Like the UPDATE/DELETE
--      triggers, they fire for every role including the owner and a superuser.
--
--   b) audit_chain_state gains entry_count and last_id: a monotone counter and
--      the highest row id ever chained. The insert trigger advances them. They
--      are the anchor that makes "rows are missing" a detectable statement
--      rather than an unanswerable one.
--
--   c) A BEFORE UPDATE guard on audit_chain_state that permits ONLY a forward
--      step -- entry_count exactly +1 and last_id strictly increasing. Resetting
--      the chain to genesis, or rewinding it to hide a removal, is now refused
--      by the database rather than merely discouraged. DELETE is refused
--      outright.
--
--   d) The chain trigger becomes SECURITY DEFINER, so the app role no longer
--      needs -- and no longer has -- any write privilege on audit_chain_state.
--
-- WHAT IS STILL TRUE AFTERWARDS: a superuser who deliberately drops these
-- triggers can still erase the table. That is unavoidable at the database
-- level; the point of the hash chain plus the entry_count anchor is that doing
-- so is *detectable* on the next verify, which before this migration it was
-- not.

-- ---------------------------------------------------------------------------
-- (b) Completeness anchor.
--
-- Backfilled from the table as it stands, BEFORE the monotone guard exists --
-- the backfill is by definition not a "+1" step, so it has to happen first.
-- ---------------------------------------------------------------------------
ALTER TABLE audit_chain_state
    ADD COLUMN IF NOT EXISTS entry_count BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_id     BIGINT NOT NULL DEFAULT 0;

UPDATE audit_chain_state
SET entry_count = (SELECT COUNT(*) FROM audit_logs),
    last_id     = COALESCE((SELECT MAX(id) FROM audit_logs), 0)
WHERE id = TRUE;

COMMENT ON COLUMN audit_chain_state.entry_count IS
    'Number of rows ever chained into audit_logs. Compared against COUNT(*) by '
    'VerifyChain: a mismatch means rows were removed. Advances by exactly one '
    'per insert and can never go backwards (trg_audit_chain_state_forward_only).';
COMMENT ON COLUMN audit_chain_state.last_id IS
    'Highest audit_logs.id ever chained. Compared against MAX(id) by VerifyChain, '
    'so a truncate that resets the sequence is detected too.';

-- ---------------------------------------------------------------------------
-- (d) Chain trigger, now SECURITY DEFINER and maintaining the anchor.
--
-- SECURITY DEFINER means it executes as this function's owner -- the migration
-- role -- so the connecting app role needs no privilege on audit_chain_state at
-- all. search_path is pinned, which is mandatory for a SECURITY DEFINER
-- function: without it a caller could put a schema of their own ahead of the
-- domain schema and have `audit_chain_state` resolve to a table they control.
-- Pinned to svc_admin since consolidation, which is where audit_logs and
-- audit_chain_state now live.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION audit_logs_chain_trigger() RETURNS TRIGGER
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, svc_admin
AS $$
DECLARE
    v_prev_hash TEXT;
    v_canonical TEXT;
BEGIN
    -- FOR UPDATE on the singleton row blocks any other concurrent insert's
    -- trigger from reading last_hash until this transaction commits or rolls
    -- back, which is what keeps the chain a single strict sequence under
    -- concurrent admin actions.
    SELECT last_hash INTO v_prev_hash FROM audit_chain_state WHERE id = TRUE FOR UPDATE;

    NEW.created_at := COALESCE(NEW.created_at, NOW());
    NEW.prev_hash := v_prev_hash;

    v_canonical := audit_row_canonical(
        NEW.id, NEW.actor_id, NEW.actor_role, NEW.action, NEW.resource_type,
        NEW.resource_id, NEW.old_value, NEW.new_value, NEW.ip, NEW.user_agent,
        NEW.request_id, NEW.created_at
    );

    NEW.row_hash := audit_row_hash(v_prev_hash, v_canonical);

    UPDATE audit_chain_state
    SET last_hash   = NEW.row_hash,
        entry_count = entry_count + 1,
        last_id     = NEW.id
    WHERE id = TRUE;

    RETURN NEW;
END;
$$;

-- ---------------------------------------------------------------------------
-- (c) audit_chain_state may only ever move forward.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION audit_chain_state_forward_only() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'audit_chain_state is the audit log''s tamper anchor: DELETE is not permitted'
            USING HINT = 'The chain head may only be advanced by inserting into audit_logs.';
    END IF;

    IF NEW.entry_count <> OLD.entry_count + 1 THEN
        RAISE EXCEPTION
            'audit_chain_state may only advance by one entry at a time (% -> %)',
            OLD.entry_count, NEW.entry_count
            USING HINT = 'Resetting or rewinding the audit chain anchor is not permitted.';
    END IF;
    IF NEW.last_id <= OLD.last_id THEN
        RAISE EXCEPTION
            'audit_chain_state.last_id must strictly increase (% -> %)',
            OLD.last_id, NEW.last_id
            USING HINT = 'Resetting or rewinding the audit chain anchor is not permitted.';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_audit_chain_state_forward_only
    BEFORE UPDATE OR DELETE ON audit_chain_state
    FOR EACH ROW EXECUTE FUNCTION audit_chain_state_forward_only();

-- ---------------------------------------------------------------------------
-- (a) TRUNCATE guards.
--
-- A separate function from audit_logs_block_mutation(): a statement-level
-- trigger has no OLD record, so the row-level function's COALESCE(OLD.id, -1)
-- cannot run here.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION audit_block_truncate() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION
        '% is append-only: TRUNCATE is not permitted', TG_TABLE_NAME
        USING HINT = 'TRUNCATE does not fire row-level DELETE triggers, which is why this '
                     'statement-level guard exists. Rows here are evidence, not cache.';
END;
$$;

CREATE TRIGGER trg_audit_logs_no_truncate
    BEFORE TRUNCATE ON audit_logs
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_truncate();

CREATE TRIGGER trg_audit_chain_state_no_truncate
    BEFORE TRUNCATE ON audit_chain_state
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_truncate();

-- ---------------------------------------------------------------------------
-- Privileges.
--
-- The app role reads the anchor (VerifyChain does) and writes it only through
-- the SECURITY DEFINER trigger above. Everything else goes.
-- ---------------------------------------------------------------------------
REVOKE ALL ON audit_chain_state FROM PUBLIC;
REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON audit_chain_state FROM telemed_admin_app;
GRANT SELECT ON audit_chain_state TO telemed_admin_app;

COMMENT ON TABLE audit_chain_state IS
    'Single-row tamper anchor for audit_logs: the hash of the newest chained row, '
    'how many rows have ever been chained, and the highest id. Written only by '
    'audit_logs_chain_trigger (SECURITY DEFINER); can only move forward; cannot be '
    'deleted or truncated.';

-- ---------------------------------------------------------------------------
-- outbox_events: the one table in this database with no GRANT of its own.
--
-- 000001 is the shared platform migration, identical in all nine repos, and it
-- creates outbox_events without granting anything -- which was invisible while
-- the service connected as the database owner. Now that telemed_admin_app is a
-- genuinely unprivileged role, the grant has to exist or every outbox write
-- fails. DELETE is included because 000001 documents a nightly retention job
-- that prunes published rows after seven days.
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON outbox_events TO telemed_admin_app;
