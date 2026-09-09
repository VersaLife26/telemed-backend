-- SECURITY-REVIEW F4: administrators no longer read patient documents or
-- prescriptions, but the doctor-verification queue still has to open the SLMC
-- certificate a doctor uploaded to prove their registration. That read is not
-- patient data, it is not clinical, and it lives in a different bucket
-- ("doctor-credentials", never "medical-reports"), so it is a different
-- resource type rather than an exception carved into 'document'.
--
-- Splitting it at the type level is what lets the access-control rule be two
-- plain rows -- "an admin may read credential_document", "an admin may not
-- read document, prescription or clinical_note" -- instead of one rule doing
-- double duty. It is also what makes the access log answer the compliance
-- question directly: SELECT ... WHERE accessed_by_role IN (admin roles) AND
-- resource_type <> 'credential_document' AND granted should be empty, and now
-- can be read as a fact rather than an inference.
ALTER TABLE document_access_log DROP CONSTRAINT IF EXISTS document_access_log_resource_type_check;
ALTER TABLE document_access_log ADD CONSTRAINT document_access_log_resource_type_check
    CHECK (resource_type IN ('document', 'prescription', 'clinical_note', 'credential_document'));
