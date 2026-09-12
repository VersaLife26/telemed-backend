-- Doctor-requested reschedule: hold a proposed time until the patient or an
-- administrator accepts (move the same paid appointment) or declines (full
-- refund). Reason text stays in this table and is never published on NATS.

CREATE TABLE IF NOT EXISTS reschedule_requests (
    id                      UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    appointment_id          UUID        NOT NULL,
    patient_id              UUID        NOT NULL,
    doctor_id               UUID        NOT NULL,
    original_slot_id        UUID        NOT NULL,
    original_start_at       TIMESTAMPTZ NOT NULL,
    original_end_at         TIMESTAMPTZ NOT NULL,
    proposed_slot_id        UUID        NOT NULL,
    proposed_start_at       TIMESTAMPTZ NOT NULL,
    proposed_end_at         TIMESTAMPTZ NOT NULL,
    -- True when this request inserted an ad-hoc slot rather than reserving a
    -- generated AVAILABLE one. Decline must CANCEL ad-hoc slots, not re-list them.
    proposed_slot_created   BOOLEAN     NOT NULL DEFAULT FALSE,
    reason                  TEXT        NOT NULL DEFAULT '',
    status                  TEXT        NOT NULL DEFAULT 'pending',
    decided_by              UUID,
    decided_by_role         TEXT,
    decided_at              TIMESTAMPTZ,
    version                 INT         NOT NULL DEFAULT 0,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT reschedule_status_chk CHECK (
        status IN ('pending', 'accepted', 'declined', 'expired')
    ),
    CONSTRAINT reschedule_proposed_range_chk CHECK (proposed_end_at > proposed_start_at),
    CONSTRAINT reschedule_original_range_chk CHECK (original_end_at > original_start_at),
    CONSTRAINT reschedule_reason_chk CHECK (char_length(reason) <= 500)
);

-- One live request per appointment: a second propose while one is pending is a 409.
CREATE UNIQUE INDEX IF NOT EXISTS uq_reschedule_requests_pending
    ON reschedule_requests (appointment_id)
    WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS idx_reschedule_requests_status
    ON reschedule_requests (status, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_reschedule_requests_patient
    ON reschedule_requests (patient_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_reschedule_requests_expiry
    ON reschedule_requests (original_start_at)
    WHERE status = 'pending';
