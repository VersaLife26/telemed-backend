-- Credential documents arrive AFTER registration.
--
-- A doctor calls POST /doctors/register, then uploads each credential with a
-- separate POST /doctors/me/documents. doctor.registered is therefore always
-- published before any document exists, and a projection fed only by that
-- event shows every application in the verification queue with an empty
-- document viewer -- so a reviewer cannot see the SLMC certificate, NIC,
-- degree certificate or photograph they are being asked to sign off on.
--
-- doctor-service now also publishes doctor.documents_updated carrying the full
-- current key set. This column is the ordering guard for that stream: an event
-- is applied only when it is strictly newer than the one already projected, so
-- a duplicate delivery is a no-op (at-least-once, per AGENT-BRIEF section 3)
-- and a redelivery that arrives out of order cannot roll the reviewer's
-- document set backwards.
--
-- NULL means "no documents event has ever been applied to this row", which is
-- distinct from "an event applied an empty set".
ALTER TABLE doctor_projection
    ADD COLUMN IF NOT EXISTS documents_updated_at TIMESTAMPTZ;

COMMENT ON COLUMN doctor_projection.documents_updated_at IS
    'occurred_at of the newest doctor.documents_updated applied to this row; ordering guard, not a display field';
