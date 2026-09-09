-- Reverse 000006. Any credential_document rows already written are rewritten
-- to 'document' first: document_access_log is append-only by trigger for the
-- rows themselves, and re-adding the narrower CHECK would otherwise fail
-- against existing data. This is a schema rollback, not a data edit -- it
-- widens no access and loses only the distinction the up migration added.
ALTER TABLE document_access_log DROP CONSTRAINT IF EXISTS document_access_log_resource_type_check;

ALTER TABLE document_access_log DISABLE TRIGGER trg_access_log_no_update;
UPDATE document_access_log SET resource_type = 'document' WHERE resource_type = 'credential_document';
ALTER TABLE document_access_log ENABLE TRIGGER trg_access_log_no_update;

ALTER TABLE document_access_log ADD CONSTRAINT document_access_log_resource_type_check
    CHECK (resource_type IN ('document', 'prescription', 'clinical_note'));
