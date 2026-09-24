-- Reverses 000013. Signature and seal rows would violate the restored
-- constraint and are deleted; their objects stay in the doctor-credentials
-- bucket, unreferenced. Doctors must re-upload both after re-applying 000013.
DELETE FROM doctor_documents WHERE document_type IN ('signature','seal');

ALTER TABLE doctor_documents
    DROP CONSTRAINT IF EXISTS doctor_documents_document_type_check;

ALTER TABLE doctor_documents
    ADD CONSTRAINT doctor_documents_document_type_check
    CHECK (document_type IN
      ('slmc_certificate','nic','degree_certificate','specialty_board_certificate','photo','other'));
