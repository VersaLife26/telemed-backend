-- Reverses 000004. Any row already stored as 'photo' would violate the
-- restored constraint, so it is demoted to 'other' first -- the key is kept,
-- which is what matters; only the label a reviewer sees is coarser.
UPDATE doctor_documents SET document_type = 'other' WHERE document_type = 'photo';

ALTER TABLE doctor_documents
    DROP CONSTRAINT IF EXISTS doctor_documents_document_type_check;

ALTER TABLE doctor_documents
    ADD CONSTRAINT doctor_documents_document_type_check
    CHECK (document_type IN
      ('slmc_certificate','nic','degree_certificate','specialty_board_certificate','other'));
