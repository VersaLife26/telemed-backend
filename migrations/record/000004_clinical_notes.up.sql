-- Clinical notes: the doctor-app SOAP screen (SDD §7.2 screen 7) finally gets
-- a home. They live in record-service rather than consultation-service or a
-- service of their own because this is where the medical record already is:
-- prescriptions, the treating-relationship authorisation model, the
-- append-only document_access_log and the FHIR mapping. Splitting the record
-- across two services would mean two audit trails and two authorisation
-- implementations, and the second one is always the one with the hole in it.

-- ============================================================================
-- icd10_codes -- WHO ICD-10 reference table
--
-- Sri Lanka codes against WHO ICD-10 (the international version), NOT
-- ICD-10-CM: the Ministry of Health's morbidity returns and the private
-- hospital sector both use the WHO rubrics. That is why the codes here are
-- three- and four-character ("E11.9", "A90") rather than the five- to
-- seven-character US clinical modification ("E11.9" vs "E11.65", "M54.5" vs
-- "M54.50"), and why the display text uses British spelling ("diarrhoea",
-- "haemorrhagic", "oedema") -- a doctor typing "haemorrhage" must find the
-- row, and a doctor typing "hemorrhage" must not be told nothing exists.
-- The doctor app's bundled fallback list uses exactly the same convention.
-- ============================================================================
CREATE TABLE IF NOT EXISTS icd10_codes (
    code       TEXT PRIMARY KEY,
    display    TEXT        NOT NULL,
    category   TEXT        NOT NULL,           -- grouping for the picker UI
    synonyms   TEXT        NOT NULL DEFAULT '', -- how clinicians actually say it
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- The search index. Postgres' built-in full-text search is used rather
    -- than pg_trgm because it needs no extension: migration 000002 already
    -- records that pg_trgm cannot be assumed present on every environment,
    -- and an ICD-10 picker that only works where a superuser ran
    -- CREATE EXTENSION is an ICD-10 picker that works on the developer's
    -- laptop and not in production.
    --
    -- Weights matter: an exact rubric word ('A') outranks a synonym ('C'),
    -- so searching "diabetes" puts "Type 2 diabetes mellitus" above every
    -- row that merely mentions diabetes in its synonym list.
    search_vector TSVECTOR GENERATED ALWAYS AS (
        setweight(to_tsvector('english', display), 'A') ||
        setweight(to_tsvector('english', replace(code, '.', ' ')), 'B') ||
        setweight(to_tsvector('english', synonyms), 'C')
    ) STORED
);

CREATE INDEX IF NOT EXISTS idx_icd10_search ON icd10_codes USING GIN (search_vector);

-- Code-prefix search ("E11" -> every type 2 diabetes subtype). text_pattern_ops
-- is required for LIKE 'E11%' to use an index under a non-C collation, which
-- is what every default Postgres install has.
CREATE INDEX IF NOT EXISTS idx_icd10_code_prefix ON icd10_codes (code text_pattern_ops);
CREATE INDEX IF NOT EXISTS idx_icd10_category ON icd10_codes (category, code);

CREATE TRIGGER trg_icd10_codes_updated_at
    BEFORE UPDATE ON icd10_codes
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============================================================================
-- clinical_notes -- one SOAP note per appointment
--
-- appointment_id is UNIQUE: one consultation produces one note. That is also
-- what makes the autosave endpoint an idempotent upsert on a URL the client
-- already knows (PUT /clinical-notes/{appointment_id}) instead of a create
-- that needs a round trip to learn an id before the second keystroke.
-- ============================================================================
CREATE TABLE IF NOT EXISTS clinical_notes (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    appointment_id UUID        NOT NULL UNIQUE,
    doctor_id      UUID        NOT NULL,
    patient_id     UUID        NOT NULL,

    -- The four SOAP fields. NOT NULL DEFAULT '' rather than nullable: a
    -- half-written note has empty sections, not unknown ones, and '' vs NULL
    -- is a distinction no clinician will ever mean.
    subjective TEXT NOT NULL DEFAULT '' CHECK (length(subjective) <= 20000),
    objective  TEXT NOT NULL DEFAULT '' CHECK (length(objective)  <= 20000),
    assessment TEXT NOT NULL DEFAULT '' CHECK (length(assessment) <= 20000),
    plan       TEXT NOT NULL DEFAULT '' CHECK (length(plan)       <= 20000),

    status       TEXT        NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'finalised')),
    finalised_at TIMESTAMPTZ,

    fhir_composition_id TEXT,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ,
    version    INT         NOT NULL DEFAULT 1,

    -- status and finalised_at are two halves of one fact, so the database
    -- refuses to hold them apart. Without this a bug that sets status without
    -- the timestamp produces a "finalised" legal record with no moment of
    -- finalisation -- unnoticeable until someone needs it in a dispute.
    CONSTRAINT clinical_notes_finalised_at_matches_status
        CHECK ((status = 'finalised') = (finalised_at IS NOT NULL))
);

-- The doctor's own list, newest first. Partial on deleted_at because the list
-- endpoint never shows deleted notes.
CREATE INDEX IF NOT EXISTS idx_clinical_notes_doctor
    ON clinical_notes (doctor_id, updated_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_clinical_notes_patient
    ON clinical_notes (patient_id, finalised_at DESC) WHERE deleted_at IS NULL AND status = 'finalised';

CREATE TRIGGER trg_clinical_notes_updated_at
    BEFORE UPDATE ON clinical_notes
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============================================================================
-- clinical_note_diagnoses -- the ICD-10 codes attached to a note
--
-- There is deliberately NO foreign key to icd10_codes. The code and its
-- display term are captured at the moment the doctor picked them, exactly as
-- prescriptions captures doctor_name/doctor_slmc at issuance: a coded
-- diagnosis on a finalised record must still read correctly in 2040 after the
-- reference table has been re-seeded from a newer ICD-10 revision, and a
-- rubric that WHO later rewords must not silently reword itself inside a
-- signed medical record.
-- ============================================================================
CREATE TABLE IF NOT EXISTS clinical_note_diagnoses (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    note_id    UUID        NOT NULL REFERENCES clinical_notes (id) ON DELETE CASCADE,
    code       TEXT        NOT NULL,
    display    TEXT        NOT NULL,
    is_primary BOOLEAN     NOT NULL DEFAULT FALSE,
    sort_order INT         NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (note_id, code)
);

CREATE INDEX IF NOT EXISTS idx_clinical_note_diagnoses_note
    ON clinical_note_diagnoses (note_id, sort_order);

-- A note has at most one primary diagnosis. Enforced here rather than in Go
-- because "which one is the primary diagnosis" is the field every downstream
-- report groups by, and two of them is a report that silently double-counts.
CREATE UNIQUE INDEX IF NOT EXISTS uq_clinical_note_one_primary_diagnosis
    ON clinical_note_diagnoses (note_id) WHERE is_primary;

-- ============================================================================
-- clinical_note_revisions -- APPEND-ONLY
--
-- A finalised clinical note is a legal medical record. Amending one is a
-- normal, expected clinical act ("the culture came back, it was not viral"),
-- but the text as it stood when the doctor signed it must remain readable
-- forever, alongside who changed it, when, and why.
--
-- Each row holds the note's content AS IT STOOD AT THE END OF that change:
-- revision 1 is the text at first finalisation, revision 2 the text after the
-- first amendment, and so on. The live clinical_notes row always mirrors the
-- highest revision. That convention makes the property this table exists for
-- -- "the prior text survives" -- directly checkable: after any number of
-- amendments, revision 1 still reads exactly as it was signed.
--
-- Append-only is enforced by trigger, exactly as document_access_log is
-- (migration 000002), because an application-level rule protects a legal
-- record only until someone opens psql.
-- ============================================================================
CREATE TABLE IF NOT EXISTS clinical_note_revisions (
    id               BIGSERIAL PRIMARY KEY,
    note_id          UUID        NOT NULL REFERENCES clinical_notes (id) ON DELETE RESTRICT,
    revision         INT         NOT NULL CHECK (revision >= 1),
    subjective       TEXT        NOT NULL,
    objective        TEXT        NOT NULL,
    assessment       TEXT        NOT NULL,
    plan             TEXT        NOT NULL,
    -- Diagnoses are frozen into the revision as JSONB rather than joined from
    -- clinical_note_diagnoses: the live table holds only the CURRENT codes,
    -- and a revision that could not say which codes were on the note when it
    -- was signed would be an incomplete record of the signed document.
    diagnoses        JSONB       NOT NULL DEFAULT '[]'::jsonb,
    change_type      TEXT        NOT NULL CHECK (change_type IN ('finalise', 'amend')),
    amendment_reason TEXT        NOT NULL DEFAULT '' CHECK (length(amendment_reason) <= 1000),
    changed_by       UUID        NOT NULL,
    changed_by_role  TEXT        NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (note_id, revision),
    -- An amendment without a stated reason is an unexplained change to a legal
    -- record. The first revision (the finalisation itself) needs no reason.
    CONSTRAINT clinical_note_revisions_amend_has_reason
        CHECK (change_type <> 'amend' OR length(amendment_reason) > 0)
);

CREATE INDEX IF NOT EXISTS idx_clinical_note_revisions_note
    ON clinical_note_revisions (note_id, revision DESC);

-- forbid_mutation() was written in 000002 with document_access_log's name
-- baked into the message. Two append-only tables now share it, so it reports
-- the table it actually fired on. Behaviour is unchanged for
-- document_access_log beyond a more accurate error string; 000004's down
-- migration restores the original text verbatim.
CREATE OR REPLACE FUNCTION forbid_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not permitted', TG_TABLE_NAME, TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_clinical_note_revisions_no_update
    BEFORE UPDATE ON clinical_note_revisions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TRIGGER trg_clinical_note_revisions_no_delete
    BEFORE DELETE ON clinical_note_revisions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ============================================================================
-- document_access_log gains 'clinical_note' as a resource type, and two
-- actions that only exist for clinical notes.
--
-- Autosave is deliberately NOT logged here. A doctor typing a paragraph
-- generates hundreds of PUTs, and an audit trail that drowns the one read
-- worth investigating in ten thousand keystroke rows is not an audit trail.
-- What is logged is every READ (view, list) and every act that changes the
-- legal record (finalise, amend); the revision table is the durable record of
-- the writes themselves.
-- ============================================================================
ALTER TABLE document_access_log DROP CONSTRAINT IF EXISTS document_access_log_resource_type_check;
ALTER TABLE document_access_log ADD CONSTRAINT document_access_log_resource_type_check
    CHECK (resource_type IN ('document', 'prescription', 'clinical_note'));

ALTER TABLE document_access_log DROP CONSTRAINT IF EXISTS document_access_log_action_check;
ALTER TABLE document_access_log ADD CONSTRAINT document_access_log_action_check
    CHECK (action IN ('view', 'download', 'verify', 'list', 'upload', 'finalise', 'amend'));
