-- In-meeting chat messages between doctor and patient during a consultation.
CREATE TABLE IF NOT EXISTS consultation_messages (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    consultation_id   UUID NOT NULL REFERENCES consultations (id) ON DELETE CASCADE,
    sender_id         UUID NOT NULL,
    sender_role       TEXT NOT NULL CHECK (sender_role IN ('doctor', 'patient')),
    sender_name       TEXT NOT NULL,
    content           TEXT NOT NULL CHECK (length(trim(content)) > 0),
    metadata          JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_consultation_messages_timeline 
    ON consultation_messages (consultation_id, created_at ASC);
