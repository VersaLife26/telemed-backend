-- The admin verification queue reviews four credential documents: the SLMC
-- certificate, the NIC, the degree certificate, and an identifying photograph.
-- Its checklist has an explicit `photo_clear` item and its projection has a
-- `photo_key` column -- but doctor_documents had no `photo` document type, so
-- there was no way for a doctor to submit the photograph the reviewer is asked
-- to sign off on. The four types the reviewer needs and the types this table
-- accepts now line up exactly.
--
-- doctors.photo_url is unrelated and stays as it is: that is the public profile
-- picture patients browse, served from wherever the profile CDN points. This is
-- a credential in the private `doctor-credentials` bucket, presigned briefly at
-- review time and never public.
ALTER TABLE doctor_documents
    DROP CONSTRAINT IF EXISTS doctor_documents_document_type_check;

ALTER TABLE doctor_documents
    ADD CONSTRAINT doctor_documents_document_type_check
    CHECK (document_type IN
      ('slmc_certificate','nic','degree_certificate','specialty_board_certificate','photo','other'));
