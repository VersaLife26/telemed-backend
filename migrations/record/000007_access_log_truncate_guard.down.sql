DROP TRIGGER IF EXISTS trg_note_revisions_no_truncate ON clinical_note_revisions;
DROP TRIGGER IF EXISTS trg_access_log_no_truncate ON document_access_log;

-- Restore the blanket DML the init script's default privileges would otherwise
-- have given. Rolling back must not leave the application unable to write its
-- own audit log.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'telemed_record_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON document_access_log     TO telemed_record_app;
        GRANT SELECT, INSERT, UPDATE, DELETE ON clinical_note_revisions TO telemed_record_app;
    END IF;
END $$;
