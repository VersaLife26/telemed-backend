-- Reverses 000005_pin_challenges.up.sql exactly.
--
-- Rolling back loses the challenge history but not any money: a challenge is
-- an intermediate authorisation state, and a payment that was captured
-- through one is already recorded in payments and ledger_entries.

DROP TRIGGER IF EXISTS trg_pin_challenges_updated_at ON pin_challenges;

DROP INDEX IF EXISTS idx_pin_challenges_expiring;
DROP INDEX IF EXISTS idx_pin_challenges_payment;
DROP INDEX IF EXISTS idx_pin_challenges_payment_pending;
DROP TABLE IF EXISTS pin_challenges;
