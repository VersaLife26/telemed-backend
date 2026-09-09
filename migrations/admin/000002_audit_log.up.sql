-- The audit log is the point of this service. It must be genuinely
-- append-only, not append-only by convention:
--
--   1. A dedicated, non-owner role (telemed_admin_app) that the service
--      connects as in every environment except local dev convenience. It is
--      granted SELECT + INSERT on audit_logs and nothing else. Table owners
--      always retain implicit privileges on their own objects in Postgres, so
--      the app must NOT be the table owner -- that is what makes the REVOKE
--      below meaningful rather than theatre.
--   2. A BEFORE UPDATE/DELETE trigger that raises unconditionally, so even a
--      session connected as the owner or a superuser gets a loud error
--      instead of a silent mutation. Triggers fire for every role, including
--      superusers, unless a session explicitly disables them.
--   3. A tamper-evident SHA-256 hash chain: every row carries prev_hash and
--      row_hash, where row_hash = SHA256(prev_hash || canonical_json(row)).
--      The canonicalisation and hashing live in ONE place (SQL functions
--      below), reused by both the insert trigger and the verify endpoint, so
--      there are not two implementations that can silently drift apart.
--   4. A BRIN index on created_at: the table is strictly insert-ordered by
--      time, which is exactly the shape BRIN is built for and is dramatically
--      smaller and cheaper to maintain than a btree over the same column.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ---------------------------------------------------------------------------
-- Restricted application role.
--
-- Local docker-compose dev commonly connects every service as the database
-- owner for convenience; that is fine for iteration but it means the
-- append-only guarantee for that connection rests entirely on the trigger
-- (step 2), not on the GRANT/REVOKE below, because an owner can always GRANT
-- itself back the privileges this migration revokes. Production deployments
-- MUST set DATABASE_URL to connect as telemed_admin_app, which is not the
-- table owner and cannot re-grant itself anything. See docs/RUNBOOK.md.
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = 'telemed_admin_app') THEN
        CREATE ROLE telemed_admin_app LOGIN PASSWORD 'changeme_in_deployment_secret_manager';
    END IF;
END
$$;

-- current_database() rather than a hardcoded "telemed_admin" literal so this
-- migration also works unmodified against a differently-named database, e.g.
-- an ephemeral one created by testcontainers in CI.
DO $$
BEGIN
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO telemed_admin_app', current_database());
END
$$;
-- USAGE on the schema this migration is running INTO, not only public.
--
-- Consolidation moved these tables out of public and into svc_admin. A role
-- without USAGE on a schema cannot see its tables at all: Postgres answers
-- "relation audit_logs does not exist", which reads like a missing migration
-- rather than a missing grant, and sent this exact investigation the wrong way
-- once already.
--
-- current_schema() rather than a literal, for the same reason current_database()
-- is used above: the migration has to work unmodified against an ephemeral
-- testcontainers database, and against public for anyone still on the old
-- single-schema layout.
DO $$
BEGIN
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO telemed_admin_app', current_schema());
END
$$;

-- public as well, and separately: pgcrypto lives there (one copy per database),
-- and audit_row_hash calls public.digest().
GRANT USAGE ON SCHEMA public TO telemed_admin_app;

-- ---------------------------------------------------------------------------
-- Chain state. A singleton row (id is a CHECK-constrained boolean so exactly
-- one row can ever exist) holding the hash of the most recently inserted
-- audit row. The insert trigger locks this row FOR UPDATE before computing
-- the new hash, which serialises concurrent audit writes on this one cheap
-- critical section -- without it, two concurrent admin actions could both
-- read the same "last" hash and fork the chain, producing a false positive
-- tamper report on verify even though nothing was actually tampered with.
-- ---------------------------------------------------------------------------
CREATE TABLE audit_chain_state (
    id        BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    last_hash TEXT NOT NULL
);

-- Genesis hash: 64 zero characters, the conventional "no previous block"
-- value. Anything else stored here after this migration runs is proof the
-- chain has processed at least one row.
INSERT INTO audit_chain_state (id, last_hash) VALUES (TRUE, repeat('0', 64));

GRANT SELECT, UPDATE ON audit_chain_state TO telemed_admin_app;

CREATE TABLE audit_logs (
    id            BIGSERIAL PRIMARY KEY,
    actor_id      UUID,
    actor_role    TEXT        NOT NULL,
    action        TEXT        NOT NULL,
    resource_type TEXT        NOT NULL,
    resource_id   TEXT,
    old_value     JSONB,
    new_value     JSONB,
    ip            INET,
    user_agent    TEXT,
    request_id    TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- prev_hash/row_hash are never supplied by the application -- the trigger
    -- below is the only writer. NOT NULL with no default means a plain
    -- INSERT that bypasses the trigger (impossible in normal operation, but
    -- worth stating) fails loudly rather than leaving a hole in the chain.
    prev_hash     TEXT        NOT NULL,
    row_hash      TEXT        NOT NULL
);

COMMENT ON TABLE audit_logs IS
    'Append-only. INSERT only, via the application. UPDATE/DELETE are blocked '
    'by trg_audit_logs_no_update/no_delete regardless of role. Verify chain '
    'integrity with POST /api/v1/admin/audit/verify.';

-- Time-series scans (list by date range, export) dominate; the table is
-- strictly insert-ordered by created_at, which is exactly BRIN's sweet spot
-- and orders of magnitude smaller than a btree over the same column.
CREATE INDEX idx_audit_logs_created_at_brin ON audit_logs USING BRIN (created_at);

-- Point filters the UI actually offers: by actor, by action, by resource.
CREATE INDEX idx_audit_logs_actor ON audit_logs (actor_id, created_at DESC);
CREATE INDEX idx_audit_logs_action ON audit_logs (action, created_at DESC);
CREATE INDEX idx_audit_logs_resource ON audit_logs (resource_type, resource_id, created_at DESC);
CREATE INDEX idx_audit_logs_request_id ON audit_logs (request_id);

-- ---------------------------------------------------------------------------
-- Canonicalisation + hashing, defined once, used by both the insert trigger
-- and the verify query.
--
-- jsonb_build_object(...) is used deliberately instead of hand-built text
-- concatenation: Postgres's jsonb storage normalises key layout
-- deterministically on every call for the same input, which is what makes
-- casting it ::text a legitimate "canonical JSON" for hashing purposes here
-- -- the same logical row always serialises to the same bytes, and there is
-- exactly one code path that decides how, not two that have to be kept in
-- sync by hand.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION audit_row_canonical(
    p_id            BIGINT,
    p_actor_id      UUID,
    p_actor_role    TEXT,
    p_action        TEXT,
    p_resource_type TEXT,
    p_resource_id   TEXT,
    p_old_value     JSONB,
    p_new_value     JSONB,
    p_ip            INET,
    p_user_agent    TEXT,
    p_request_id    TEXT,
    p_created_at    TIMESTAMPTZ
) RETURNS TEXT
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT jsonb_build_object(
        'id', p_id,
        'actor_id', p_actor_id,
        'actor_role', p_actor_role,
        'action', p_action,
        'resource_type', p_resource_type,
        'resource_id', p_resource_id,
        'old_value', p_old_value,
        'new_value', p_new_value,
        'ip', host(p_ip),
        'user_agent', p_user_agent,
        'request_id', p_request_id,
        'created_at', to_char(p_created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')
    )::text;
$$;

-- digest() is SCHEMA-QUALIFIED, and it has to be.
--
-- pgcrypto lives in public (one copy for the whole database; an extension is a
-- database-global object). This function is called from
-- audit_logs_chain_trigger, which is SECURITY DEFINER and therefore pins its
-- own search_path to `pg_catalog, svc_admin` -- deliberately, so a caller
-- cannot prepend a schema of their own. That pin is in force for everything
-- the trigger calls, so an unqualified digest() resolves against a path that
-- does not contain public and fails with "function digest(text, unknown) does
-- not exist" on every audit insert.
--
-- Qualifying here is the tighter of the two fixes: the alternative is adding
-- public to the definer's pinned path, which widens the trusted search path of
-- the one function in the schema that most needs it narrow.
CREATE OR REPLACE FUNCTION audit_row_hash(p_prev_hash TEXT, p_canonical TEXT) RETURNS TEXT
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT encode(public.digest(p_prev_hash || p_canonical, 'sha256'), 'hex');
$$;

CREATE OR REPLACE FUNCTION audit_logs_chain_trigger() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
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

    UPDATE audit_chain_state SET last_hash = NEW.row_hash WHERE id = TRUE;

    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_audit_logs_chain
    BEFORE INSERT ON audit_logs
    FOR EACH ROW EXECUTE FUNCTION audit_logs_chain_trigger();

-- ---------------------------------------------------------------------------
-- Append-only enforcement. This is the loud-not-silent backstop: it fires for
-- every role including the table owner and any superuser session that has
-- not deliberately set session_replication_role = replica.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION audit_logs_block_mutation() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION
        'audit_logs is append-only: % is not permitted (row id=%)',
        TG_OP, COALESCE(OLD.id, -1)
        USING HINT = 'audit_logs may only be inserted into, never updated or deleted.';
END;
$$;

CREATE TRIGGER trg_audit_logs_no_update
    BEFORE UPDATE ON audit_logs
    FOR EACH ROW EXECUTE FUNCTION audit_logs_block_mutation();

CREATE TRIGGER trg_audit_logs_no_delete
    BEFORE DELETE ON audit_logs
    FOR EACH ROW EXECUTE FUNCTION audit_logs_block_mutation();

-- Grant only what the append-only contract allows: read everything, insert
-- new rows. No UPDATE, no DELETE, no TRUNCATE -- stated explicitly even
-- though the absence of a GRANT already implies it, because an explicit
-- REVOKE also strips any privilege PUBLIC might otherwise carry.
REVOKE ALL ON audit_logs FROM PUBLIC;
GRANT SELECT, INSERT ON audit_logs TO telemed_admin_app;
GRANT USAGE, SELECT ON SEQUENCE audit_logs_id_seq TO telemed_admin_app;
REVOKE UPDATE, DELETE, TRUNCATE ON audit_logs FROM telemed_admin_app;

-- The canonicalisation/hash functions must be callable by the verify
-- endpoint, which only ever reads.
GRANT EXECUTE ON FUNCTION audit_row_canonical(BIGINT, UUID, TEXT, TEXT, TEXT, TEXT, JSONB, JSONB, INET, TEXT, TEXT, TIMESTAMPTZ) TO telemed_admin_app;
GRANT EXECUTE ON FUNCTION audit_row_hash(TEXT, TEXT) TO telemed_admin_app;
