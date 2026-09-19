DROP INDEX IF EXISTS idx_documents_owner_folder;
ALTER TABLE documents DROP COLUMN IF EXISTS folder_id;
DROP TABLE IF EXISTS folders;
