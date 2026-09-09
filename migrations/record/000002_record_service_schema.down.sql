DROP TABLE IF EXISTS consumed_events;

DROP TRIGGER IF EXISTS trg_treating_relationships_updated_at ON treating_relationships;
DROP TABLE IF EXISTS treating_relationships;

DROP TRIGGER IF EXISTS trg_record_shares_updated_at ON record_shares;
DROP TABLE IF EXISTS record_shares;

DROP TRIGGER IF EXISTS trg_access_log_no_delete ON document_access_log;
DROP TRIGGER IF EXISTS trg_access_log_no_update ON document_access_log;
DROP TABLE IF EXISTS document_access_log;
DROP FUNCTION IF EXISTS forbid_mutation();

DROP TABLE IF EXISTS prescription_items;

DROP TRIGGER IF EXISTS trg_prescriptions_updated_at ON prescriptions;
DROP TABLE IF EXISTS prescriptions;

DROP TRIGGER IF EXISTS trg_drugs_updated_at ON drugs;
DROP TABLE IF EXISTS drugs;

DROP TRIGGER IF EXISTS trg_documents_updated_at ON documents;
DROP TABLE IF EXISTS documents;

DROP FUNCTION IF EXISTS set_updated_at();
