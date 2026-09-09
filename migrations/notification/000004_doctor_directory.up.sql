-- A local, event-fed projection of the handful of doctor facts this service
-- needs to render a message: a display name, and the list price/specialty.
--
-- Why a projection rather than a field on each event: the canonical payloads
-- in internal/platform/events/payloads.go carry identifiers and facts about
-- what happened, not a denormalised copy of another service's data. Copying
-- doctor_name onto appointment.confirmed, payment.succeeded, waitlist.offered,
-- prescription.issued and consultation.started would mean five producers all
-- had to remember to send it -- and the one that forgot would produce exactly
-- the failure _shared/INTEGRATION-FIXES.md item 9 documents: a notification
-- addressed to nobody, with encoding/json quietly supplying "".
--
-- Why a projection rather than a gRPC call: unlike a patient's phone number,
-- a doctor's public profile already arrives here as events this service is
-- subscribed to anyway (doctor.approved, doctor.updated). Fetching what the
-- event already told us would be a round trip for nothing.
CREATE TABLE IF NOT EXISTS doctor_directory (
    doctor_id     UUID PRIMARY KEY,

    -- The doctor's own account in telemed_user. Needed because notifications
    -- address a USER (payout.sent names a doctor, but the notification and
    -- the preferences row belong to the user behind them).
    user_id       UUID        NOT NULL,

    full_name     TEXT        NOT NULL DEFAULT '',
    email         TEXT        NOT NULL DEFAULT '',
    specialty     TEXT        NOT NULL DEFAULT '',

    -- The doctor's CURRENT list price. Deliberately not used to render the
    -- amount on a booking or a receipt -- those must show what the patient was
    -- actually quoted or charged, which travels on the appointment and payment
    -- events. It is here for messages that describe the doctor rather than a
    -- transaction.
    fee_cents     BIGINT      NOT NULL DEFAULT 0,
    currency      TEXT        NOT NULL DEFAULT 'LKR',

    status        TEXT        NOT NULL DEFAULT 'approved',

    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
    -- No deleted_at and no version: this is a projection, not a user-visible
    -- record. Its truth lives in telemed_doctor; rebuilding it is a stream
    -- replay, not a restore.
);

CREATE INDEX IF NOT EXISTS idx_doctor_directory_user ON doctor_directory (user_id);
