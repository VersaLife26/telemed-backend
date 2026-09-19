-- Folders inside a patient's vault.
--
-- A folder belongs to exactly one vault (owner_user_id), and so does every
-- document filed in it. The service refuses any move whose target folder has
-- a different owner, so the tree can never carry a file from one patient's
-- vault into another's. Access is authorised as the 'document' resource: a
-- folder name is vault content, and whoever may list a vault may list its
-- folders.
CREATE TABLE IF NOT EXISTS folders (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_id UUID        NOT NULL,
    parent_id     UUID        REFERENCES folders(id),
    name          TEXT        NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 120),
    created_by    UUID        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at    TIMESTAMPTZ,
    version       INT         NOT NULL DEFAULT 1
);

-- Sibling names are unique per vault, case-insensitively. COALESCE folds the
-- root (NULL parent) into one comparable value so two root folders cannot
-- share a name either.
CREATE UNIQUE INDEX IF NOT EXISTS uq_folders_sibling_name
    ON folders (owner_user_id, COALESCE(parent_id, '00000000-0000-0000-0000-000000000000'::uuid), lower(name))
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_folders_owner_parent ON folders (owner_user_id, parent_id) WHERE deleted_at IS NULL;

CREATE TRIGGER trg_folders_updated_at
    BEFORE UPDATE ON folders
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE documents ADD COLUMN IF NOT EXISTS folder_id UUID REFERENCES folders(id);

CREATE INDEX IF NOT EXISTS idx_documents_owner_folder ON documents (owner_user_id, folder_id, created_at DESC) WHERE deleted_at IS NULL;
