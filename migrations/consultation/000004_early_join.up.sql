-- When a doctor finishes early, offer the immediate next patient a chance to
-- join now. Consent lives on the NEXT consultation row. Accept/decline does
-- not rewrite scheduled_at; later slots are never cascaded.

ALTER TABLE consultations
    ADD COLUMN IF NOT EXISTS early_join_offered_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS early_join_response TEXT,
    ADD COLUMN IF NOT EXISTS early_join_responded_at TIMESTAMPTZ;

ALTER TABLE consultations
    DROP CONSTRAINT IF EXISTS ck_consultations_early_join_response;
ALTER TABLE consultations
    ADD CONSTRAINT ck_consultations_early_join_response
        CHECK (
            early_join_response IS NULL
            OR early_join_response IN ('accepted', 'declined')
        );

ALTER TABLE consultations
    DROP CONSTRAINT IF EXISTS ck_consultations_early_join_offered;
ALTER TABLE consultations
    ADD CONSTRAINT ck_consultations_early_join_offered
        CHECK (
            early_join_response IS NULL
            OR early_join_offered_at IS NOT NULL
        );
