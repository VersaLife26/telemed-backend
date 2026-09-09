-- Reverses 000002_payments.up.sql exactly, in dependency order.
-- Verified by running: migrate up && migrate down 1 && migrate up.

DROP TRIGGER IF EXISTS trg_commission_rules_updated_at ON commission_rules;
DROP TRIGGER IF EXISTS trg_webhook_events_updated_at ON webhook_events;
DROP TRIGGER IF EXISTS trg_refunds_updated_at ON refunds;
DROP TRIGGER IF EXISTS trg_payouts_updated_at ON payouts;
DROP TRIGGER IF EXISTS trg_payments_updated_at ON payments;
DROP FUNCTION IF EXISTS set_updated_at();

DROP TRIGGER IF EXISTS trg_ledger_no_truncate ON ledger_entries;
DROP TRIGGER IF EXISTS trg_ledger_no_update ON ledger_entries;
DROP FUNCTION IF EXISTS ledger_entries_append_only();

DROP TABLE IF EXISTS ledger_entries;
DROP TABLE IF EXISTS refunds;
DROP TABLE IF EXISTS webhook_events;
DROP TABLE IF EXISTS payments;
DROP TABLE IF EXISTS payouts;
DROP TABLE IF EXISTS commission_rules;
