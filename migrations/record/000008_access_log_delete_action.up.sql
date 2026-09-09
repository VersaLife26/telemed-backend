-- Record a deletion in the PHI access log.
--
-- records.Delete was the only PHI-touching method in the service that called
-- neither access.Check nor InsertAccessLog: an inline role check, then
-- SoftDelete. So any admin or support role could destroy a patient's medical
-- documents with no entry in the append-only log whose entire purpose is
-- answering "who touched this record". The action CHECK constraint did not
-- even permit 'delete', so it could not have been logged without this
-- migration (SECURITY-REVIEW F27).
--
-- Deletion is a write, and access.Action.IsWrite classifies it as one, so the
-- read-only grants that let an administrator open a doctor's credential
-- paperwork do not also let them erase a patient's chart.
ALTER TABLE document_access_log DROP CONSTRAINT IF EXISTS document_access_log_action_check;
ALTER TABLE document_access_log ADD CONSTRAINT document_access_log_action_check
    CHECK (action IN ('view', 'download', 'verify', 'list', 'upload', 'finalise', 'amend', 'delete'));
