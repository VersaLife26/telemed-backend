-- Reverses 000003_promo_codes.up.sql exactly.
--
-- The redemption rows go first because they reference both promo_codes and
-- payments; the payments columns go last because dropping them while a
-- redemption still points at the payment would leave the discount recorded in
-- one table and not the other for the duration of the migration.

DROP TRIGGER IF EXISTS trg_promo_redemptions_updated_at ON promo_redemptions;
DROP TRIGGER IF EXISTS trg_promo_codes_updated_at ON promo_codes;

DROP INDEX IF EXISTS idx_promo_redemptions_expiring;
DROP INDEX IF EXISTS idx_promo_redemptions_user_live;
DROP INDEX IF EXISTS idx_promo_redemptions_payment_live;
DROP TABLE IF EXISTS promo_redemptions;

DROP INDEX IF EXISTS idx_promo_codes_active;
DROP TABLE IF EXISTS promo_codes;

ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_discount_has_code;
ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_discount_balances;
ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_discount_nonneg;

ALTER TABLE payments DROP COLUMN IF EXISTS promo_code;
ALTER TABLE payments DROP COLUMN IF EXISTS discount_cents;
ALTER TABLE payments DROP COLUMN IF EXISTS gross_amount_cents;
