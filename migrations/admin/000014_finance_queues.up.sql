-- Finance queues the admin console already calls: refund requests and payout
-- batches. Admin-service cannot read telemed_payment (ADR-004), so these are
-- local projections plus staff-opened refund rows.

CREATE TABLE IF NOT EXISTS finance_refund_requests (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    payment_id      UUID        NOT NULL,
    appointment_id  UUID,
    dispute_id      UUID,
    amount_cents    BIGINT      NOT NULL CHECK (amount_cents > 0),
    currency        TEXT        NOT NULL DEFAULT 'LKR',
    reason          TEXT        NOT NULL DEFAULT '',
    status          TEXT        NOT NULL CHECK (status IN ('pending', 'approved', 'rejected')),
    requested_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    decided_at      TIMESTAMPTZ,
    decided_by      UUID
);

CREATE UNIQUE INDEX idx_finance_refund_pending_payment
    ON finance_refund_requests (payment_id)
    WHERE status = 'pending';

CREATE INDEX idx_finance_refund_requests_status
    ON finance_refund_requests (status, requested_at DESC);

CREATE TABLE IF NOT EXISTS payout_batches_projection (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    period_start  DATE        NOT NULL,
    period_end    DATE        NOT NULL,
    status        TEXT        NOT NULL CHECK (status IN ('pending', 'processing', 'paid', 'failed')),
    doctor_count  INT         NOT NULL DEFAULT 0,
    total_cents   BIGINT      NOT NULL DEFAULT 0,
    currency      TEXT        NOT NULL DEFAULT 'LKR',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at  TIMESTAMPTZ,
    CONSTRAINT payout_batches_projection_period UNIQUE (period_start, period_end)
);

CREATE INDEX idx_payout_batches_projection_created
    ON payout_batches_projection (created_at DESC);

GRANT SELECT, INSERT, UPDATE ON finance_refund_requests TO telemed_admin_app;
GRANT SELECT, INSERT, UPDATE ON payout_batches_projection TO telemed_admin_app;
