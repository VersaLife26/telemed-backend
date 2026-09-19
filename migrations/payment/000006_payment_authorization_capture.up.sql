-- Add 'authorized' status to payments table check constraint
ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_status_check;
ALTER TABLE payments ADD CONSTRAINT payments_status_check
    CHECK (status IN ('pending', 'requires_action', 'requires_pin', 'authorized', 'succeeded',
                      'failed', 'partially_refunded', 'refunded'));

-- Add columns to track hold authorization, scheduling start, and completion triggers
ALTER TABLE payments ADD COLUMN IF NOT EXISTS authorized_at TIMESTAMPTZ;
ALTER TABLE payments ADD COLUMN IF NOT EXISTS consultation_ended_at TIMESTAMPTZ;
ALTER TABLE payments ADD COLUMN IF NOT EXISTS completed_at TIMESTAMPTZ;
ALTER TABLE payments ADD COLUMN IF NOT EXISTS scheduled_start_at TIMESTAMPTZ;
ALTER TABLE payments ADD COLUMN IF NOT EXISTS authorization_token TEXT NOT NULL DEFAULT '';
