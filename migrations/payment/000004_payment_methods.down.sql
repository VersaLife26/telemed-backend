-- Reverses 000004_payment_methods.up.sql exactly.
--
-- Dropping these tables orphans the tokens at the provider: the Stripe
-- PaymentMethods stay attached to their Customers and simply stop being
-- listed here. That is the correct direction for a rollback -- deleting them
-- at the rail on the way down would make the migration destructive and
-- irreversible, and a patient's saved card would vanish because of a deploy.

DROP TRIGGER IF EXISTS trg_payment_methods_updated_at ON payment_methods;
DROP TRIGGER IF EXISTS trg_payment_customers_updated_at ON payment_customers;

DROP INDEX IF EXISTS idx_payment_methods_patient;
DROP INDEX IF EXISTS idx_payment_methods_one_default;
DROP TABLE IF EXISTS payment_methods;

DROP TABLE IF EXISTS payment_customers;
