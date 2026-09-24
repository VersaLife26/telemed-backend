-- DocumentType already had 'signature' and 'seal', and record-service refuses
-- to issue a prescription without both, but this constraint rejected them --
-- so no doctor could ever store the images a prescription requires.
ALTER TABLE doctor_documents
    DROP CONSTRAINT IF EXISTS doctor_documents_document_type_check;

ALTER TABLE doctor_documents
    ADD CONSTRAINT doctor_documents_document_type_check
    CHECK (document_type IN
      ('slmc_certificate','nic','degree_certificate','specialty_board_certificate','photo','other',
       'signature','seal'));
