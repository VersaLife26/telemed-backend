-- The PHI access log is the record of who read whose medical documents. It is
-- the artefact a breach investigation depends on, and under HIPAA §164.312(b)
-- and Sri Lanka's PDPA it is not optional.
--
-- Two holes, both found by a pre-production security review:
--
-- 1. TRUNCATE was never guarded. 000002 blocks UPDATE and DELETE with row-level
--    triggers, but TRUNCATE is a statement-level operation that fires neither.
--    One statement erased the entire trail, and the row triggers watched it
--    happen.
--
-- 2. The application role could re-grant itself anything, because it OWNED the
--    table. An owner may always restore its own privileges, so any REVOKE
--    against it was decoration. That half is fixed in telemed-infra's role
--    model (telemed_record_app now owns nothing); the grants below express the
--    intent explicitly so the two agree rather than merely happening to.

-- forbid_mutation() reads only TG_OP and never touches OLD, so it is already
-- valid as a statement-level trigger. No new function is needed.
DROP TRIGGER IF EXISTS trg_access_log_no_truncate ON document_access_log;
CREATE TRIGGER trg_access_log_no_truncate
    BEFORE TRUNCATE ON document_access_log
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_mutation();

-- The same reasoning applies to every append-only table in this service.
DROP TRIGGER IF EXISTS trg_note_revisions_no_truncate ON clinical_note_revisions;
CREATE TRIGGER trg_note_revisions_no_truncate
    BEFORE TRUNCATE ON clinical_note_revisions
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_mutation();

-- Grants, so the privilege model states the intent rather than relying on the
-- triggers alone. Defence in depth: a trigger can be dropped by whoever owns
-- the table; a privilege the role never held cannot be exercised at all.
--
-- Guarded because telemed_record_app does not exist in every environment (a
-- developer running migrations as a superuser against a scratch database is a
-- legitimate case, and this migration must not fail there).
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'telemed_record_app') THEN
        REVOKE ALL ON document_access_log      FROM PUBLIC;
        REVOKE ALL ON clinical_note_revisions  FROM PUBLIC;

        REVOKE UPDATE, DELETE, TRUNCATE ON document_access_log     FROM telemed_record_app;
        REVOKE UPDATE, DELETE, TRUNCATE ON clinical_note_revisions FROM telemed_record_app;

        GRANT SELECT, INSERT ON document_access_log     TO telemed_record_app;
        GRANT SELECT, INSERT ON clinical_note_revisions TO telemed_record_app;

        GRANT USAGE, SELECT ON SEQUENCE document_access_log_id_seq TO telemed_record_app;
    END IF;
END $$;
