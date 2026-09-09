-- Reverses 000004 exactly, in dependency order.

-- Restore the access-log constraints to the exact form 000002 created them
-- in, so rolling back to 000003 leaves the table as 000003 left it.
ALTER TABLE document_access_log DROP CONSTRAINT IF EXISTS document_access_log_action_check;
ALTER TABLE document_access_log ADD CONSTRAINT document_access_log_action_check
    CHECK (action IN ('view', 'download', 'verify', 'list', 'upload'));

ALTER TABLE document_access_log DROP CONSTRAINT IF EXISTS document_access_log_resource_type_check;
ALTER TABLE document_access_log ADD CONSTRAINT document_access_log_resource_type_check
    CHECK (resource_type IN ('document', 'prescription'));

DROP TRIGGER IF EXISTS trg_clinical_note_revisions_no_delete ON clinical_note_revisions;
DROP TRIGGER IF EXISTS trg_clinical_note_revisions_no_update ON clinical_note_revisions;
DROP TABLE IF EXISTS clinical_note_revisions;

-- Put forbid_mutation() back the way 000002 wrote it. document_access_log's
-- triggers still reference it and must keep working after this rollback.
CREATE OR REPLACE FUNCTION forbid_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'document_access_log is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

DROP TABLE IF EXISTS clinical_note_diagnoses;

DROP TRIGGER IF EXISTS trg_clinical_notes_updated_at ON clinical_notes;
DROP TABLE IF EXISTS clinical_notes;

DROP TRIGGER IF EXISTS trg_icd10_codes_updated_at ON icd10_codes;
DROP TABLE IF EXISTS icd10_codes;
