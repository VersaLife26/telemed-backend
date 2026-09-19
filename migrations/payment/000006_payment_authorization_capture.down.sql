ALTER TABLE payments DROP COLUMN IF EXISTS authorization_token;
ALTER TABLE payments DROP COLUMN IF EXISTS scheduled_start_at;
ALTER TABLE payments DROP COLUMN IF EXISTS completed_at;
ALTER TABLE payments DROP COLUMN IF EXISTS consultation_ended_at;
ALTER TABLE payments DROP COLUMN IF EXISTS authorized_at;

ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_status_check;
ALTER TABLE payments ADD CONSTRAINT payments_status_check
    CHECK (status IN ('pending', 'requires_action', 'requires_pin', 'succeeded',
                      'failed', 'partially_refunded', 'refunded'));
