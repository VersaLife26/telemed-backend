-- User search projection for the admin console, fed by user.registered,
-- user.suspended and user.reinstated events (the latter two are new subjects
-- this service adds to internal/platform/events/events.go; see AllSubjects).
-- user-service remains the system of record; this table exists so
-- GET /api/v1/admin/users?query= does not require a synchronous fan-out call
-- on every keystroke of an admin's search box.

CREATE TABLE user_projection (
    user_id      UUID PRIMARY KEY,
    event_id     UUID        NOT NULL,
    full_name    TEXT,
    email        TEXT,
    phone        TEXT,
    role         TEXT        NOT NULL CHECK (role IN ('patient', 'doctor')),
    status       TEXT        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    registered_at TIMESTAMPTZ,
    ingested_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_user_projection_role ON user_projection (role);
CREATE INDEX idx_user_projection_status ON user_projection (status);
CREATE INDEX idx_user_projection_email ON user_projection (lower(email));
CREATE INDEX idx_user_projection_phone ON user_projection (phone);

GRANT SELECT, INSERT, UPDATE ON user_projection TO telemed_admin_app;
