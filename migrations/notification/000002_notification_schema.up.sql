-- Notification service schema.
--
-- Design note on cross-service data (ADR-004 compliance): this service must
-- decide, every minute, "which confirmed appointments start in 55-65
-- minutes?" without a foreign key or SQL join into telemed_scheduling's
-- database -- each service owns its own database and cross-service reads go
-- over events, never a join. appointment_reminder_state below is a local,
-- disposable read-model of exactly the columns the reminder cron needs,
-- populated by consuming appointment.confirmed / appointment.cancelled /
-- appointment.completed / appointment.no_show off NATS. If this table is
-- dropped and rebuilt from the event log, correctness returns after the
-- retention window (7 days on the JetStream stream) with only cosmetic loss
-- (a missed reminder for an appointment that started in the rebuild gap).

CREATE TABLE IF NOT EXISTS notifications (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id             UUID        NOT NULL,
    channel             TEXT        NOT NULL CHECK (channel IN ('sms', 'push', 'email', 'in_app')),
    template_key        TEXT        NOT NULL,
    locale              TEXT        NOT NULL CHECK (locale IN ('en', 'si', 'ta')),
    urgency             TEXT        NOT NULL DEFAULT 'normal' CHECK (urgency IN ('critical', 'urgent', 'normal')),

    -- Rendered content. PHI-adjacent (see retention note at the bottom of this
    -- file) -- a notification body routinely reads "Your consultation with
    -- Dr Perera is in 1 hour", which discloses that the recipient has a
    -- medical appointment. Never included in application logs.
    subject             TEXT,
    body                TEXT        NOT NULL,

    -- Where to actually send it: an E.164 phone for sms, an email address
    -- for email. NULL for push (device_tokens, which this service owns, is
    -- resolved at dispatch time) and in_app (no external recipient at all).
    -- Populated at enqueue time from the triggering event's payload rather
    -- than looked up synchronously from user-service -- see docs/DESIGN.md
    -- on why this service never makes a blocking cross-service call from
    -- its delivery path.
    recipient           TEXT,

    status              TEXT        NOT NULL DEFAULT 'queued'
                                     CHECK (status IN ('queued', 'sending', 'sent', 'delivered', 'failed', 'suppressed')),
    provider             TEXT,
    provider_message_id TEXT,

    attempts            INT         NOT NULL DEFAULT 0,
    last_error          TEXT,
    error_class         TEXT        CHECK (error_class IN ('transient', 'permanent')),

    -- scheduled_for defers a send (quiet-hours hold). next_attempt_at backs
    -- off a retry (transient provider failure). They are independent because
    -- a message can be both deferred once for quiet hours and later retried.
    scheduled_for       TIMESTAMPTZ,
    next_attempt_at     TIMESTAMPTZ,

    sent_at              TIMESTAMPTZ,
    delivered_at         TIMESTAMPTZ,
    read_at               TIMESTAMPTZ,

    -- The idempotency enforcement mechanism. At-least-once event delivery is
    -- guaranteed by JetStream; this UNIQUE constraint, not an in-memory set,
    -- is what makes redelivering the same envelope.ID a no-op: the second
    -- INSERT ... ON CONFLICT (dedupe_key) DO NOTHING affects zero rows.
    dedupe_key            TEXT        NOT NULL,
    source_event_id       UUID,

    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at             TIMESTAMPTZ,
    version                INT         NOT NULL DEFAULT 0,

    CONSTRAINT uq_notifications_dedupe_key UNIQUE (dedupe_key)
);

-- The dispatcher's claim query scans exactly this shape: queued work whose
-- hold/backoff has elapsed, oldest first.
CREATE INDEX IF NOT EXISTS idx_notifications_dispatch
    ON notifications (created_at)
    WHERE status = 'queued';

-- GET /api/v1/notifications (mine, paginated).
CREATE INDEX IF NOT EXISTS idx_notifications_user_created
    ON notifications (user_id, created_at DESC)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_notifications_status ON notifications (status);

-- Per-user, per-channel opt-in/opt-out plus locale and quiet hours. One row
-- per user, created lazily on first preference read/write; a missing row
-- means "platform defaults", not "notifications disabled".
CREATE TABLE IF NOT EXISTS notification_preferences (
    user_id             UUID PRIMARY KEY,
    sms_enabled          BOOLEAN     NOT NULL DEFAULT TRUE,
    push_enabled         BOOLEAN     NOT NULL DEFAULT TRUE,
    email_enabled        BOOLEAN     NOT NULL DEFAULT TRUE,
    in_app_enabled       BOOLEAN     NOT NULL DEFAULT TRUE,
    locale               TEXT        NOT NULL DEFAULT 'en' CHECK (locale IN ('en', 'si', 'ta')),

    -- NULL/NULL means no quiet hours configured. Stored as plain integer
    -- minutes-since-midnight (0-1439) rather than TIME/INTERVAL: it keeps
    -- the wire mapping driver-agnostic and sidesteps any ambiguity in how a
    -- given Postgres client library maps TIME OF DAY, at the cost of one
    -- division in application code. Evaluated in the user's own timezone --
    -- never converted to UTC, because "22:00" must keep meaning 10pm local
    -- regardless of where the server runs.
    quiet_hours_start_min SMALLINT CHECK (quiet_hours_start_min BETWEEN 0 AND 1439),
    quiet_hours_end_min   SMALLINT CHECK (quiet_hours_end_min BETWEEN 0 AND 1439),
    timezone              TEXT        NOT NULL DEFAULT 'Asia/Colombo',

    version                INT         NOT NULL DEFAULT 0,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW()
    -- No deleted_at: this is a singleton settings row keyed by user_id, not
    -- an independent user-visible record. "Deleting" it means resetting to
    -- defaults, which is a DELETE, not a soft delete.
);

-- FCM/APNs/web-push tokens. Multiple per user (phone + tablet + web).
CREATE TABLE IF NOT EXISTS device_tokens (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id           UUID        NOT NULL,
    token             TEXT        NOT NULL,
    platform          TEXT        NOT NULL CHECK (platform IN ('ios', 'android', 'web')),
    last_seen_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Soft-delete signal for this table. Set when the client calls
    -- DELETE /devices/{id}, or when FCM reports the token unregistered
    -- (NotRegistered / UNREGISTERED / 404). A second deleted_at column would
    -- be redundant with this one.
    invalidated_at    TIMESTAMPTZ,

    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT uq_device_tokens_user_token UNIQUE (user_id, token)
);

CREATE INDEX IF NOT EXISTS idx_device_tokens_active
    ON device_tokens (user_id)
    WHERE invalidated_at IS NULL;

-- Versioned, seeded templates. text/template for sms/push/in_app (plain
-- text, no HTML escaping needed or wanted), html/template for email (auto
-- context-aware escaping -- using text/template for an HTML body is exactly
-- the injection hole the brief calls out).
CREATE TABLE IF NOT EXISTS templates (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    key               TEXT        NOT NULL,
    channel           TEXT        NOT NULL CHECK (channel IN ('sms', 'push', 'email', 'in_app')),
    locale            TEXT        NOT NULL CHECK (locale IN ('en', 'si', 'ta')),
    urgency           TEXT        NOT NULL DEFAULT 'normal' CHECK (urgency IN ('critical', 'urgent', 'normal')),

    subject_template  TEXT,
    body_template     TEXT        NOT NULL,

    version           INT         NOT NULL DEFAULT 1,
    is_active         BOOLEAN     NOT NULL DEFAULT TRUE,

    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT uq_templates_key_channel_locale_version UNIQUE (key, channel, locale, version)
);

-- At most one active version per (key, channel, locale): the render lookup
-- is a single indexed read, and "promote a new version" is a transaction
-- that flips is_active on the old row off and the new row on.
CREATE UNIQUE INDEX IF NOT EXISTS uq_templates_active
    ON templates (key, channel, locale)
    WHERE is_active;

-- Append-only per-attempt delivery record, mirroring outbox_events' shape:
-- created_at only, no updated_at/deleted_at/version, because a log line is
-- never mutated after the fact.
CREATE TABLE IF NOT EXISTS delivery_log (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    notification_id     UUID        NOT NULL REFERENCES notifications (id) ON DELETE CASCADE,
    attempt             INT         NOT NULL,
    provider             TEXT        NOT NULL,
    status                TEXT        NOT NULL CHECK (status IN ('sent', 'delivered', 'failed', 'bounced')),
    error_class           TEXT        CHECK (error_class IN ('transient', 'permanent')),
    -- Raw provider response, minus anything PHI: message content is never
    -- echoed back into this column, only IDs/codes/status text.
    provider_response    JSONB,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_delivery_log_notification
    ON delivery_log (notification_id, created_at DESC);

-- Permanent failures and retry-exhausted sends land here for operator triage.
-- Kept separate from notifications.status='failed' so "show me what needs a
-- human" is a dedicated, small table instead of a filtered scan of the whole
-- notifications table.
CREATE TABLE IF NOT EXISTS notification_dead_letters (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    notification_id     UUID        NOT NULL REFERENCES notifications (id) ON DELETE CASCADE,
    user_id             UUID        NOT NULL,
    channel             TEXT        NOT NULL,
    template_key        TEXT        NOT NULL,
    attempts             INT         NOT NULL,
    last_error            TEXT,
    reason                 TEXT        NOT NULL CHECK (reason IN ('permanent_error', 'retries_exhausted')),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at            TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_dead_letters_unresolved
    ON notification_dead_letters (created_at)
    WHERE resolved_at IS NULL;

-- Local projection of scheduling's appointments, populated by consuming
-- appointment.confirmed / appointment.cancelled / appointment.completed /
-- appointment.no_show. See the design note at the top of this file. Holds no
-- more data than the reminder cron needs to decide whom to remind and how to
-- reach them -- patient_phone/patient_email are denormalized from the
-- appointment.confirmed payload at the moment it arrives specifically so the
-- cron (which runs on a timer, not in response to a fresh event) never needs
-- a synchronous lookup into user-service to find where to send a reminder.
CREATE TABLE IF NOT EXISTS appointment_reminder_state (
    appointment_id         UUID PRIMARY KEY,
    patient_id              UUID        NOT NULL,
    doctor_id                UUID        NOT NULL,
    doctor_name              TEXT        NOT NULL DEFAULT '',
    specialty                 TEXT        NOT NULL DEFAULT '',
    patient_phone             TEXT        NOT NULL DEFAULT '',
    patient_email             TEXT        NOT NULL DEFAULT '',
    join_link                  TEXT        NOT NULL DEFAULT '',
    starts_at                  TIMESTAMPTZ NOT NULL,
    status                      TEXT        NOT NULL DEFAULT 'confirmed'
                                             CHECK (status IN ('confirmed', 'cancelled', 'completed', 'no_show')),

    reminder_24h_sent_at    TIMESTAMPTZ,
    reminder_1h_sent_at      TIMESTAMPTZ,

    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The cron's WHERE clause, indexed: confirmed appointments ordered by start
-- time, with no filter on the (unindexable, always-changing) NOW() window.
CREATE INDEX IF NOT EXISTS idx_reminder_state_due
    ON appointment_reminder_state (starts_at)
    WHERE status = 'confirmed';

-- Retention: rendered notification bodies are PHI-adjacent free text (see the
-- `notifications.subject`/`body` comment above). A maintenance job -- cron,
-- run outside application code, e.g. `SELECT prune_notification_bodies();`
-- wired into the platform's nightly maintenance window -- blanks subject and
-- body on rows older than 90 days:
--
--   UPDATE notifications
--   SET subject = NULL, body = '[pruned]'
--   WHERE created_at < NOW() - INTERVAL '90 days' AND body <> '[pruned]';
--
-- The row (status, channel, timestamps, dedupe_key) is kept indefinitely --
-- it is operational metadata, not PHI -- only the free-text content is
-- pruned. See docs/RUNBOOK.md for the scheduled job.
