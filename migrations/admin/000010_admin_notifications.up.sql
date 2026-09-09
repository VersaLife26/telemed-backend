-- In-app notifications for the admin console (e.g. new doctor applications).
CREATE TABLE IF NOT EXISTS admin_notifications (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         TEXT        NOT NULL,
    title        TEXT        NOT NULL,
    body         TEXT        NOT NULL,
    href         TEXT        NOT NULL DEFAULT '/doctors',
    resource_id  UUID,
    read_at      TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_admin_notifications_unread
    ON admin_notifications (created_at DESC)
    WHERE read_at IS NULL;

-- Application fee / contact are on the public-apply event (no user yet).
ALTER TABLE doctor_projection
    ADD COLUMN IF NOT EXISTS fee_cents BIGINT,
    ADD COLUMN IF NOT EXISTS user_id UUID;

GRANT SELECT, INSERT, UPDATE ON admin_notifications TO telemed_admin_app;
