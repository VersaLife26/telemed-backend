-- Reverses 000008. Any rows already recording a deletion would violate the
-- narrower constraint, so they are removed first -- which is lossy on an
-- append-only table and is exactly why this down migration should only ever
-- run on a database that has not served traffic.
DELETE FROM document_access_log WHERE action = 'delete';

ALTER TABLE document_access_log DROP CONSTRAINT IF EXISTS document_access_log_action_check;
ALTER TABLE document_access_log ADD CONSTRAINT document_access_log_action_check
    CHECK (action IN ('view', 'download', 'verify', 'list', 'upload', 'finalise', 'amend'));
