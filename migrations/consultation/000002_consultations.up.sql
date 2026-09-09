-- Consultation domain: room lifecycle, access, waiting room, consent-gated
-- recording, and the append-only timeline used for support and disputes.
--
-- Two tables beyond the five named in the design brief were added, both
-- narrowly scoped and documented here:
--   * consultation_webhook_receipts -- LiveKit redelivers webhooks that are not
--     acknowledged with 2xx quickly enough. Without a dedupe record, a
--     redelivered egress_ended could double-append recording events.
--   * connection-quality samples are NOT a separate table. consultation_events
--     already needs an append-only timeline with example event types
--     'quality_degraded' etc, so raw quality samples are stored as
--     consultation_events rows (event_type = 'quality_sample'). That keeps a
--     single, chronologically ordered story per consultation instead of two
--     tables you have to interleave by hand at 2am.

CREATE TABLE IF NOT EXISTS consultations (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    appointment_id    UUID NOT NULL,
    patient_id        UUID NOT NULL,
    doctor_id         UUID NOT NULL,
    room_name         TEXT NOT NULL,
    status            TEXT NOT NULL DEFAULT 'scheduled'
                          CHECK (status IN ('scheduled', 'waiting', 'active', 'ended', 'abandoned', 'failed')),
    scheduled_at      TIMESTAMPTZ NOT NULL,
    started_at        TIMESTAMPTZ,
    ended_at          TIMESTAMPTZ,
    duration_seconds  INT,
    recording_url     TEXT,
    recording_status  TEXT NOT NULL DEFAULT 'none'
                          CHECK (recording_status IN ('none', 'pending', 'recording', 'processing', 'completed', 'failed')),
    -- egress_id correlates an in-flight LiveKit recording with the
    -- egress_ended webhook that eventually reports its result.
    egress_id         TEXT,
    end_reason        TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at        TIMESTAMPTZ,
    version           INT NOT NULL DEFAULT 0,
    CONSTRAINT uq_consultations_appointment UNIQUE (appointment_id),
    CONSTRAINT uq_consultations_room_name UNIQUE (room_name),
    CONSTRAINT ck_consultations_duration_nonneg CHECK (duration_seconds IS NULL OR duration_seconds >= 0)
);

CREATE INDEX IF NOT EXISTS idx_consultations_doctor ON consultations (doctor_id, scheduled_at DESC);
CREATE INDEX IF NOT EXISTS idx_consultations_patient ON consultations (patient_id, scheduled_at DESC);
CREATE INDEX IF NOT EXISTS idx_consultations_status ON consultations (status) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_consultations_egress ON consultations (egress_id) WHERE egress_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS consultation_participants (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    consultation_id   UUID NOT NULL REFERENCES consultations (id) ON DELETE CASCADE,
    -- identity is the LiveKit participant identity: consultations.patient_id
    -- as text for the patient, consultations.doctor_id (the doctor PROFILE
    -- id, not the doctor's own account/user id -- those are different id
    -- spaces) for the doctor. Using the same id consultations already carries
    -- is what lets a webhook's participant identity be matched back to a role
    -- with no extra lookup; see partyIdentity in service.go.
    identity          TEXT NOT NULL,
    role              TEXT NOT NULL CHECK (role IN ('patient', 'doctor')),
    joined_at         TIMESTAMPTZ,
    left_at           TIMESTAMPTZ,
    reconnect_count   INT NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_consultation_participant UNIQUE (consultation_id, identity)
);

CREATE INDEX IF NOT EXISTS idx_participants_consultation ON consultation_participants (consultation_id);

CREATE TABLE IF NOT EXISTS consultation_consents (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    consultation_id   UUID NOT NULL REFERENCES consultations (id) ON DELETE CASCADE,
    user_id           UUID NOT NULL,
    consent_type      TEXT NOT NULL CHECK (consent_type IN ('recording', 'telemedicine')),
    granted           BOOLEAN NOT NULL,
    granted_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- TEXT rather than INET: consent logging needs the string an operator can
    -- read in an audit export, not Postgres's network-arithmetic type, and it
    -- sidesteps IPv4-mapped-IPv6 and X-Forwarded-For edge cases entirely.
    ip_address        TEXT,
    user_agent        TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Consent is append-only (a new row per submission, never UPDATEd) so a
-- dispute can show exactly when a party changed their mind. "Current" consent
-- is the most recent row per (consultation, user, type).
CREATE INDEX IF NOT EXISTS idx_consents_lookup
    ON consultation_consents (consultation_id, consent_type, user_id, granted_at DESC);

CREATE TABLE IF NOT EXISTS consultation_events (
    id                BIGSERIAL PRIMARY KEY,
    consultation_id   UUID NOT NULL REFERENCES consultations (id) ON DELETE CASCADE,
    -- joined, left, admitted, quality_sample, quality_degraded, video_disabled,
    -- recording_started, recording_completed, recording_failed, ended,
    -- abandoned, consent_recorded, cancellation_ignored_active ...
    event_type        TEXT NOT NULL,
    actor_identity    TEXT,
    metadata          JSONB NOT NULL DEFAULT '{}'::jsonb,
    occurred_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_events_consultation ON consultation_events (consultation_id, occurred_at);
CREATE INDEX IF NOT EXISTS idx_events_quality_lookup
    ON consultation_events (consultation_id, actor_identity, event_type, occurred_at DESC)
    WHERE event_type = 'quality_sample';

CREATE TABLE IF NOT EXISTS waiting_room_entries (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    consultation_id   UUID NOT NULL REFERENCES consultations (id) ON DELETE CASCADE,
    doctor_id         UUID NOT NULL,
    patient_id        UUID NOT NULL,
    entered_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    status            TEXT NOT NULL DEFAULT 'waiting' CHECK (status IN ('waiting', 'admitted', 'left', 'expired')),
    admitted_at       TIMESTAMPTZ,
    left_at           TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_waiting_room_consultation UNIQUE (consultation_id)
);

-- The doctor's queue: every patient currently waiting for this doctor across
-- all of today's consultations, ordered by arrival. This is the Postgres
-- source of truth the Redis sorted set mirrors; ADR-007 applies here exactly
-- as it does to slot booking -- Redis is the fast path, this index is correct
-- even if Redis has just been flushed.
CREATE INDEX IF NOT EXISTS idx_waiting_room_doctor_queue
    ON waiting_room_entries (doctor_id, entered_at)
    WHERE status = 'waiting';

CREATE TABLE IF NOT EXISTS consultation_webhook_receipts (
    event_id     TEXT PRIMARY KEY,
    event_type   TEXT NOT NULL,
    received_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
