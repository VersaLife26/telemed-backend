package payment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
)

// Persistence for the three surfaces the mobile apps needed and this service
// did not have: promotional codes, carrier-billing PIN challenges, and
// tokenised payment methods.
//
// Same rules as repository.go: SQL and nothing else, every statement
// parameterised, no business decision and no HTTP error anywhere in the file.

const promoCodeColumns = `
	id, code, description, discount_type, percent_bps, amount_off_cents,
	max_discount_cents, min_amount_cents, currency, valid_from, valid_until,
	max_redemptions, max_per_user, redemption_count, active,
	created_at, updated_at, deleted_at, version`

const promoRedemptionColumns = `
	id, promo_code_id, code, user_id, payment_id, appointment_id,
	discount_cents, currency, status, reserved_at, expires_at,
	consumed_at, released_at, release_reason, created_at, updated_at, version`

const pinChallengeColumns = `
	id, payment_id, provider, reference, masked_msisdn, status,
	attempts, max_attempts, expires_at, confirmed_at, settled_at, failure_reason,
	created_at, updated_at, version`

const paymentMethodColumns = `
	id, patient_id, provider, provider_customer_id, provider_token,
	method_type, brand, last4, exp_month, exp_year, is_default,
	created_at, updated_at, deleted_at, version`

func scanPromoCode(row rowScanner) (PromoCode, error) {
	var c PromoCode
	var maxRedemptions *int
	if err := row.Scan(
		&c.ID, &c.Code, &c.Description, &c.DiscountType, &c.PercentBps, &c.AmountOffCents,
		&c.MaxDiscountCents, &c.MinAmountCents, &c.Currency, &c.ValidFrom, &c.ValidUntil,
		&maxRedemptions, &c.MaxPerUser, &c.RedemptionCount, &c.Active,
		&c.CreatedAt, &c.UpdatedAt, &c.DeletedAt, &c.Version,
	); err != nil {
		return PromoCode{}, err
	}
	c.MaxRedemptions = maxRedemptions
	return c, nil
}

func scanPromoRedemption(row rowScanner) (PromoRedemption, error) {
	var r PromoRedemption
	if err := row.Scan(
		&r.ID, &r.PromoCodeID, &r.Code, &r.UserID, &r.PaymentID, &r.AppointmentID,
		&r.DiscountCents, &r.Currency, &r.Status, &r.ReservedAt, &r.ExpiresAt,
		&r.ConsumedAt, &r.ReleasedAt, &r.ReleaseReason, &r.CreatedAt, &r.UpdatedAt, &r.Version,
	); err != nil {
		return PromoRedemption{}, err
	}
	return r, nil
}

func scanPINChallenge(row rowScanner) (PINChallenge, error) {
	var c PINChallenge
	if err := row.Scan(
		&c.ID, &c.PaymentID, &c.Provider, &c.Reference, &c.MaskedMSISDN, &c.Status,
		&c.Attempts, &c.MaxAttempts, &c.ExpiresAt, &c.ConfirmedAt, &c.SettledAt, &c.FailureReason,
		&c.CreatedAt, &c.UpdatedAt, &c.Version,
	); err != nil {
		return PINChallenge{}, err
	}
	return c, nil
}

func scanPaymentMethod(row rowScanner) (PaymentMethod, error) {
	var m PaymentMethod
	if err := row.Scan(
		&m.ID, &m.PatientID, &m.Provider, &m.ProviderCustomerID, &m.ProviderToken,
		&m.MethodType, &m.Brand, &m.Last4, &m.ExpMonth, &m.ExpYear, &m.IsDefault,
		&m.CreatedAt, &m.UpdatedAt, &m.DeletedAt, &m.Version,
	); err != nil {
		return PaymentMethod{}, err
	}
	return m, nil
}

// --- Store reads: promotions ------------------------------------------------

// ExpiredReservationIDs lists holds the sweeper should release. Ids only: the
// sweeper takes one short transaction per row rather than holding locks across
// a whole batch, so a large backlog never blocks a patient applying a code.
func (r *Repository) ExpiredReservationIDs(ctx context.Context, before time.Time, limit int) ([]uuid.UUID, error) {
	const q = `
		SELECT id FROM promo_redemptions
		WHERE status = 'reserved' AND expires_at <= $1
		ORDER BY expires_at
		LIMIT $2`
	rows, err := r.pool.Query(ctx, q, before, limit)
	if err != nil {
		return nil, fmt.Errorf("payment: list expired promo reservations: %w", err)
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// GetPromoCodeByCode reads one code without locking it. Used by the admin
// read paths only; redemption always goes through Tx.FindPromoCode plus
// LockPromoCodeByID.
func (r *Repository) GetPromoCodeByCode(ctx context.Context, code string) (PromoCode, error) {
	q := `SELECT ` + promoCodeColumns + ` FROM promo_codes WHERE code = $1 AND deleted_at IS NULL`
	c, err := scanPromoCode(r.pool.QueryRow(ctx, q, code))
	if err != nil {
		return PromoCode{}, notFound(err)
	}
	return c, nil
}

// ListPromoCodes is the ops view of what is on offer.
func (r *Repository) ListPromoCodes(ctx context.Context, includeInactive bool, p Page) ([]PromoCode, int64, error) {
	where := `deleted_at IS NULL`
	if !includeInactive {
		where += ` AND active`
	}

	var total int64
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM promo_codes WHERE `+where).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("payment: count promo codes: %w", err)
	}
	if total == 0 {
		return []PromoCode{}, 0, nil
	}

	q := `SELECT ` + promoCodeColumns + ` FROM promo_codes WHERE ` + where +
		` ORDER BY created_at DESC LIMIT $1 OFFSET $2`
	rows, err := r.pool.Query(ctx, q, p.PerPage, p.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("payment: list promo codes: %w", err)
	}
	defer rows.Close()

	out := make([]PromoCode, 0, p.PerPage)
	for rows.Next() {
		c, err := scanPromoCode(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

// --- Store reads: PIN challenges --------------------------------------------

// LatestPINChallenge returns the most recent challenge for a payment,
// whatever its status. Support uses it to answer "did the PIN ever arrive".
func (r *Repository) LatestPINChallenge(ctx context.Context, paymentID uuid.UUID) (PINChallenge, bool, error) {
	q := `SELECT ` + pinChallengeColumns + ` FROM pin_challenges
	      WHERE payment_id = $1 ORDER BY created_at DESC LIMIT 1`
	c, err := scanPINChallenge(r.pool.QueryRow(ctx, q, paymentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return PINChallenge{}, false, nil
	}
	if err != nil {
		return PINChallenge{}, false, fmt.Errorf("payment: latest pin challenge: %w", err)
	}
	return c, true, nil
}

// ExpiredPINChallengeIDs feeds the challenge sweeper.
func (r *Repository) ExpiredPINChallengeIDs(ctx context.Context, before time.Time, limit int) ([]uuid.UUID, error) {
	const q = `
		SELECT id FROM pin_challenges
		WHERE status = 'pending' AND expires_at <= $1
		ORDER BY expires_at
		LIMIT $2`
	rows, err := r.pool.Query(ctx, q, before, limit)
	if err != nil {
		return nil, fmt.Errorf("payment: list expired pin challenges: %w", err)
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// --- Store reads: saved payment methods --------------------------------------

// GetPaymentCustomer reads a patient's handle at one rail.
func (r *Repository) GetPaymentCustomer(ctx context.Context, patientID uuid.UUID, provider ProviderName) (PaymentCustomer, error) {
	const q = `
		SELECT id, patient_id, provider, provider_customer_id, created_at, updated_at, version
		FROM payment_customers
		WHERE patient_id = $1 AND provider = $2 AND deleted_at IS NULL`
	var c PaymentCustomer
	err := r.pool.QueryRow(ctx, q, patientID, provider).Scan(
		&c.ID, &c.PatientID, &c.Provider, &c.ProviderCustomerID, &c.CreatedAt, &c.UpdatedAt, &c.Version)
	if err != nil {
		return PaymentCustomer{}, notFound(err)
	}
	return c, nil
}

// ListPaymentMethods returns a patient's cards, default first then newest.
func (r *Repository) ListPaymentMethods(ctx context.Context, patientID uuid.UUID) ([]PaymentMethod, error) {
	q := `SELECT ` + paymentMethodColumns + ` FROM payment_methods
	      WHERE patient_id = $1 AND deleted_at IS NULL
	      ORDER BY is_default DESC, created_at DESC`
	rows, err := r.pool.Query(ctx, q, patientID)
	if err != nil {
		return nil, fmt.Errorf("payment: list payment methods: %w", err)
	}
	defer rows.Close()

	out := []PaymentMethod{}
	for rows.Next() {
		m, err := scanPaymentMethod(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetPaymentMethod reads one card.
func (r *Repository) GetPaymentMethod(ctx context.Context, id uuid.UUID) (PaymentMethod, error) {
	q := `SELECT ` + paymentMethodColumns + ` FROM payment_methods WHERE id = $1 AND deleted_at IS NULL`
	m, err := scanPaymentMethod(r.pool.QueryRow(ctx, q, id))
	if err != nil {
		return PaymentMethod{}, notFound(err)
	}
	return m, nil
}

// FindPaymentMethodByToken is how the two recording paths recognise each
// other's work.
func (r *Repository) FindPaymentMethodByToken(ctx context.Context, provider ProviderName, token string) (PaymentMethod, bool, error) {
	q := `SELECT ` + paymentMethodColumns + ` FROM payment_methods
	      WHERE provider = $1 AND provider_token = $2 AND deleted_at IS NULL`
	m, err := scanPaymentMethod(r.pool.QueryRow(ctx, q, provider, token))
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentMethod{}, false, nil
	}
	if err != nil {
		return PaymentMethod{}, false, fmt.Errorf("payment: find payment method by token: %w", err)
	}
	return m, true, nil
}

// --- Tx: promotions ---------------------------------------------------------

// FindPromoCode reads a code by its normalised form, without locking.
func (t *txRepo) FindPromoCode(ctx context.Context, code string) (PromoCode, error) {
	q := `SELECT ` + promoCodeColumns + ` FROM promo_codes WHERE code = $1 AND deleted_at IS NULL`
	c, err := scanPromoCode(t.tx.QueryRow(ctx, q, code))
	if err != nil {
		return PromoCode{}, notFound(err)
	}
	return c, nil
}

// LockPromoCodeByID is the serialisation point for every redemption of that
// code. Two patients racing for the last one queue here; exactly one proceeds
// at a time, reads the true count, and writes.
func (t *txRepo) LockPromoCodeByID(ctx context.Context, id uuid.UUID) (PromoCode, error) {
	q := `SELECT ` + promoCodeColumns + ` FROM promo_codes WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`
	c, err := scanPromoCode(t.tx.QueryRow(ctx, q, id))
	if err != nil {
		return PromoCode{}, notFound(err)
	}
	return c, nil
}

// AdjustRedemptionCount moves the live counter. The promo_codes_budget CHECK
// refuses an increment past the cap and a decrement below zero, so a lost lock
// surfaces as a failed transaction rather than an over-spent promotion.
func (t *txRepo) AdjustRedemptionCount(ctx context.Context, id uuid.UUID, delta int) error {
	const q = `
		UPDATE promo_codes
		SET redemption_count = redemption_count + $2, version = version + 1
		WHERE id = $1 AND deleted_at IS NULL`
	tag, err := t.tx.Exec(ctx, q, id, delta)
	if err != nil {
		return fmt.Errorf("payment: adjust redemption count for %s by %d: %w", id, delta, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CountLiveRedemptions is the per-user cap check. "Live" is reserved or
// consumed and not past its hold, which is the same definition
// ExpireStaleReservations enforces.
func (t *txRepo) CountLiveRedemptions(ctx context.Context, codeID, userID uuid.UUID) (int, error) {
	const q = `
		SELECT COUNT(*) FROM promo_redemptions
		WHERE promo_code_id = $1 AND user_id = $2 AND status <> 'released'`
	var n int
	if err := t.tx.QueryRow(ctx, q, codeID, userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("payment: count live redemptions: %w", err)
	}
	return n, nil
}

// ExpireStaleReservations releases this code's abandoned holds and decrements
// the counter by the same number, in one statement pair under the caller's row
// lock so the two can never disagree.
func (t *txRepo) ExpireStaleReservations(ctx context.Context, codeID uuid.UUID, now time.Time) (int, error) {
	const q = `
		UPDATE promo_redemptions
		SET status = 'released', released_at = $3, release_reason = 'expired', version = version + 1
		WHERE promo_code_id = $1 AND status = 'reserved' AND expires_at <= $2`
	tag, err := t.tx.Exec(ctx, q, codeID, now, now)
	if err != nil {
		return 0, fmt.Errorf("payment: expire stale reservations: %w", err)
	}
	n := int(tag.RowsAffected())
	if n == 0 {
		return 0, nil
	}
	if err := t.AdjustRedemptionCount(ctx, codeID, -n); err != nil {
		return 0, err
	}
	return n, nil
}

// InsertRedemption records one hold.
func (t *txRepo) InsertRedemption(ctx context.Context, r *PromoRedemption) error {
	const q = `
		INSERT INTO promo_redemptions (
			id, promo_code_id, code, user_id, payment_id, appointment_id,
			discount_cents, currency, status, reserved_at, expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING created_at, updated_at, version`
	err := t.tx.QueryRow(ctx, q,
		r.ID, r.PromoCodeID, r.Code, r.UserID, r.PaymentID, r.AppointmentID,
		r.DiscountCents, r.Currency, r.Status, r.ReservedAt, r.ExpiresAt,
	).Scan(&r.CreatedAt, &r.UpdatedAt, &r.Version)
	if err != nil {
		if database.IsUniqueViolation(err) {
			return fmt.Errorf("%w: payment %s already holds a promo code", ErrDuplicate, r.PaymentID)
		}
		return fmt.Errorf("payment: insert promo redemption: %w", err)
	}
	return nil
}

// FindLiveRedemptionForPayment returns the reserved-or-consumed hold on a
// payment, if any. The partial unique index guarantees there is at most one.
func (t *txRepo) FindLiveRedemptionForPayment(ctx context.Context, paymentID uuid.UUID) (PromoRedemption, bool, error) {
	q := `SELECT ` + promoRedemptionColumns + ` FROM promo_redemptions
	      WHERE payment_id = $1 AND status <> 'released' FOR UPDATE`
	r, err := scanPromoRedemption(t.tx.QueryRow(ctx, q, paymentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return PromoRedemption{}, false, nil
	}
	if err != nil {
		return PromoRedemption{}, false, fmt.Errorf("payment: find live redemption: %w", err)
	}
	return r, true, nil
}

// LockRedemption takes a row lock on one redemption, for the sweeper.
func (t *txRepo) LockRedemption(ctx context.Context, id uuid.UUID) (PromoRedemption, bool, error) {
	q := `SELECT ` + promoRedemptionColumns + ` FROM promo_redemptions WHERE id = $1 FOR UPDATE`
	r, err := scanPromoRedemption(t.tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return PromoRedemption{}, false, nil
	}
	if err != nil {
		return PromoRedemption{}, false, fmt.Errorf("payment: lock redemption: %w", err)
	}
	return r, true, nil
}

// SetRedemptionStatus moves a hold to a terminal state, stamping the matching
// timestamp so the promo_redemptions_terminal CHECK is satisfied.
func (t *txRepo) SetRedemptionStatus(ctx context.Context, id uuid.UUID, status PromoRedemptionStatus, reason string, at time.Time) error {
	const q = `
		UPDATE promo_redemptions SET
			status         = $2,
			consumed_at    = CASE WHEN $2 = 'consumed' THEN $3 ELSE consumed_at END,
			released_at    = CASE WHEN $2 = 'released' THEN $3 ELSE released_at END,
			release_reason = CASE WHEN $2 = 'released' THEN $4 ELSE release_reason END,
			version        = version + 1
		WHERE id = $1`
	tag, err := t.tx.Exec(ctx, q, id, string(status), at, reason)
	if err != nil {
		return fmt.Errorf("payment: set redemption %s status: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchRedemption extends a hold.
func (t *txRepo) TouchRedemption(ctx context.Context, id uuid.UUID, expiresAt time.Time) error {
	const q = `
		UPDATE promo_redemptions SET expires_at = $2, version = version + 1
		WHERE id = $1 AND status = 'reserved'`
	if _, err := t.tx.Exec(ctx, q, id, expiresAt); err != nil {
		return fmt.Errorf("payment: touch redemption %s: %w", id, err)
	}
	return nil
}

// InsertPromoCode creates a promotion.
func (t *txRepo) InsertPromoCode(ctx context.Context, c *PromoCode) error {
	const q = `
		INSERT INTO promo_codes (
			id, code, description, discount_type, percent_bps, amount_off_cents,
			max_discount_cents, min_amount_cents, currency, valid_from, valid_until,
			max_redemptions, max_per_user, active
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		RETURNING redemption_count, created_at, updated_at, version`
	err := t.tx.QueryRow(ctx, q,
		c.ID, c.Code, c.Description, c.DiscountType, c.PercentBps, c.AmountOffCents,
		c.MaxDiscountCents, c.MinAmountCents, c.Currency, c.ValidFrom, c.ValidUntil,
		c.MaxRedemptions, c.MaxPerUser, c.Active,
	).Scan(&c.RedemptionCount, &c.CreatedAt, &c.UpdatedAt, &c.Version)
	if err != nil {
		if database.IsUniqueViolation(err) {
			return fmt.Errorf("%w: promo code %s already exists", ErrDuplicate, c.Code)
		}
		return fmt.Errorf("payment: insert promo code: %w", err)
	}
	return nil
}

// DeactivatePromoCode stops a code being redeemed without deleting the
// redemptions that already happened. Existing reserved holds are left alone:
// a patient mid-checkout keeps the price they were quoted.
func (t *txRepo) DeactivatePromoCode(ctx context.Context, code string) error {
	const q = `UPDATE promo_codes SET active = FALSE, version = version + 1
	           WHERE code = $1 AND deleted_at IS NULL`
	tag, err := t.tx.Exec(ctx, q, code)
	if err != nil {
		return fmt.Errorf("payment: deactivate promo code: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// --- Tx: PIN challenges ------------------------------------------------------

// InsertPINChallenge records a freshly issued challenge.
func (t *txRepo) InsertPINChallenge(ctx context.Context, c *PINChallenge) error {
	const q = `
		INSERT INTO pin_challenges (
			id, payment_id, provider, reference, masked_msisdn, status,
			attempts, max_attempts, expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING created_at, updated_at, version`
	err := t.tx.QueryRow(ctx, q,
		c.ID, c.PaymentID, c.Provider, c.Reference, c.MaskedMSISDN, c.Status,
		c.Attempts, c.MaxAttempts, c.ExpiresAt,
	).Scan(&c.CreatedAt, &c.UpdatedAt, &c.Version)
	if err != nil {
		if database.IsUniqueViolation(err) {
			return fmt.Errorf("%w: payment %s already has a pending PIN challenge", ErrDuplicate, c.PaymentID)
		}
		return fmt.Errorf("payment: insert pin challenge: %w", err)
	}
	return nil
}

// CountPINChallengesSince is how many PIN challenges have been opened against
// one payment inside a window, used to bound resends.
func (t *txRepo) CountPINChallengesSince(ctx context.Context, paymentID uuid.UUID, since time.Time) (int, error) {
	const q = `SELECT COUNT(*) FROM pin_challenges WHERE payment_id = $1 AND created_at >= $2`
	var n int
	if err := t.tx.QueryRow(ctx, q, paymentID, since).Scan(&n); err != nil {
		return 0, fmt.Errorf("payment: count pin challenges: %w", err)
	}
	return n, nil
}

// SupersedePendingPINChallenges retires whatever was outstanding so a resend
// can issue a new one. Two live references would mean the patient's SMS might
// correspond to either.
func (t *txRepo) SupersedePendingPINChallenges(ctx context.Context, paymentID uuid.UUID, at time.Time) (int, error) {
	const q = `
		UPDATE pin_challenges
		SET status = 'superseded', settled_at = $2, failure_reason = 'a newer PIN was requested',
		    version = version + 1
		WHERE payment_id = $1 AND status = 'pending'`
	tag, err := t.tx.Exec(ctx, q, paymentID, at)
	if err != nil {
		return 0, fmt.Errorf("payment: supersede pin challenges: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// LockPendingPINChallenge takes a row lock on the one live challenge.
func (t *txRepo) LockPendingPINChallenge(ctx context.Context, paymentID uuid.UUID) (PINChallenge, bool, error) {
	q := `SELECT ` + pinChallengeColumns + ` FROM pin_challenges
	      WHERE payment_id = $1 AND status = 'pending' FOR UPDATE`
	c, err := scanPINChallenge(t.tx.QueryRow(ctx, q, paymentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return PINChallenge{}, false, nil
	}
	if err != nil {
		return PINChallenge{}, false, fmt.Errorf("payment: lock pending pin challenge: %w", err)
	}
	return c, true, nil
}

// LockPINChallenge takes a row lock on one challenge by id, for the sweeper.
func (t *txRepo) LockPINChallenge(ctx context.Context, id uuid.UUID) (PINChallenge, bool, error) {
	q := `SELECT ` + pinChallengeColumns + ` FROM pin_challenges WHERE id = $1 FOR UPDATE`
	c, err := scanPINChallenge(t.tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return PINChallenge{}, false, nil
	}
	if err != nil {
		return PINChallenge{}, false, fmt.Errorf("payment: lock pin challenge: %w", err)
	}
	return c, true, nil
}

// UpdatePINChallenge writes the mutable fields under an optimistic-lock guard.
func (t *txRepo) UpdatePINChallenge(ctx context.Context, c *PINChallenge) error {
	const q = `
		UPDATE pin_challenges SET
			status = $2, attempts = $3, confirmed_at = $4, settled_at = $5,
			failure_reason = $6, version = version + 1
		WHERE id = $1 AND version = $7
		RETURNING version, updated_at`
	err := t.tx.QueryRow(ctx, q,
		c.ID, c.Status, c.Attempts, c.ConfirmedAt, c.SettledAt, c.FailureReason, c.Version,
	).Scan(&c.Version, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: pin challenge %s at version %d", ErrVersionConflict, c.ID, c.Version)
	}
	if err != nil {
		return fmt.Errorf("payment: update pin challenge: %w", err)
	}
	return nil
}

// --- Tx: saved payment methods -----------------------------------------------

// InsertPaymentCustomer stores a rail's customer handle for a patient.
func (t *txRepo) InsertPaymentCustomer(ctx context.Context, c *PaymentCustomer) error {
	const q = `
		INSERT INTO payment_customers (id, patient_id, provider, provider_customer_id)
		VALUES ($1,$2,$3,$4)
		RETURNING created_at, updated_at, version`
	err := t.tx.QueryRow(ctx, q, c.ID, c.PatientID, c.Provider, c.ProviderCustomerID).
		Scan(&c.CreatedAt, &c.UpdatedAt, &c.Version)
	if err != nil {
		if database.IsUniqueViolation(err) {
			return fmt.Errorf("%w: customer handle for patient %s at %s", ErrDuplicate, c.PatientID, c.Provider)
		}
		return fmt.Errorf("payment: insert payment customer: %w", err)
	}
	return nil
}

// InsertPaymentMethod saves a tokenised card.
func (t *txRepo) InsertPaymentMethod(ctx context.Context, m *PaymentMethod) error {
	const q = `
		INSERT INTO payment_methods (
			id, patient_id, provider, provider_customer_id, provider_token,
			method_type, brand, last4, exp_month, exp_year, is_default
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING created_at, updated_at, version`
	err := t.tx.QueryRow(ctx, q,
		m.ID, m.PatientID, m.Provider, m.ProviderCustomerID, m.ProviderToken,
		m.MethodType, m.Brand, m.Last4, m.ExpMonth, m.ExpYear, m.IsDefault,
	).Scan(&m.CreatedAt, &m.UpdatedAt, &m.Version)
	if err != nil {
		if database.IsUniqueViolation(err) {
			return fmt.Errorf("%w: this card is already saved", ErrDuplicate)
		}
		return fmt.Errorf("payment: insert payment method: %w", err)
	}
	return nil
}

// LockPaymentMethod takes a row lock on one saved card.
func (t *txRepo) LockPaymentMethod(ctx context.Context, id uuid.UUID) (PaymentMethod, error) {
	q := `SELECT ` + paymentMethodColumns + ` FROM payment_methods
	      WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`
	m, err := scanPaymentMethod(t.tx.QueryRow(ctx, q, id))
	if err != nil {
		return PaymentMethod{}, notFound(err)
	}
	return m, nil
}

// UpdatePaymentMethod writes display metadata and the default flag under an
// optimistic-lock guard. provider_token is absent from the SET list on
// purpose: a saved card's token is what it *is*, and rewriting it would point
// an existing row at a different instrument.
func (t *txRepo) UpdatePaymentMethod(ctx context.Context, m *PaymentMethod) error {
	const q = `
		UPDATE payment_methods SET
			brand = $2, last4 = $3, exp_month = $4, exp_year = $5, is_default = $6,
			version = version + 1
		WHERE id = $1 AND version = $7 AND deleted_at IS NULL
		RETURNING version, updated_at`
	err := t.tx.QueryRow(ctx, q, m.ID, m.Brand, m.Last4, m.ExpMonth, m.ExpYear, m.IsDefault, m.Version).
		Scan(&m.Version, &m.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: payment method %s at version %d", ErrVersionConflict, m.ID, m.Version)
	}
	if err != nil {
		return fmt.Errorf("payment: update payment method: %w", err)
	}
	return nil
}

// FindPaymentMethodByToken is the transactional twin of the Store read, used
// inside recordMethod so the check and the insert are one atomic decision.
func (t *txRepo) FindPaymentMethodByToken(ctx context.Context, provider ProviderName, token string) (PaymentMethod, bool, error) {
	q := `SELECT ` + paymentMethodColumns + ` FROM payment_methods
	      WHERE provider = $1 AND provider_token = $2 AND deleted_at IS NULL FOR UPDATE`
	m, err := scanPaymentMethod(t.tx.QueryRow(ctx, q, provider, token))
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentMethod{}, false, nil
	}
	if err != nil {
		return PaymentMethod{}, false, fmt.Errorf("payment: find payment method by token: %w", err)
	}
	return m, true, nil
}

// CountPaymentMethods is how a first saved card learns it is the default.
func (t *txRepo) CountPaymentMethods(ctx context.Context, patientID uuid.UUID) (int, error) {
	const q = `SELECT COUNT(*) FROM payment_methods WHERE patient_id = $1 AND deleted_at IS NULL`
	var n int
	if err := t.tx.QueryRow(ctx, q, patientID).Scan(&n); err != nil {
		return 0, fmt.Errorf("payment: count payment methods: %w", err)
	}
	return n, nil
}

// ClearDefaultPaymentMethod unsets whichever card is currently the default.
func (t *txRepo) ClearDefaultPaymentMethod(ctx context.Context, patientID uuid.UUID) error {
	const q = `
		UPDATE payment_methods SET is_default = FALSE, version = version + 1
		WHERE patient_id = $1 AND is_default AND deleted_at IS NULL`
	if _, err := t.tx.Exec(ctx, q, patientID); err != nil {
		return fmt.Errorf("payment: clear default payment method: %w", err)
	}
	return nil
}

// SoftDeletePaymentMethod removes a card from the patient's list.
//
// It also clears is_default, because the partial unique index only ignores
// deleted rows and a deleted-but-still-default row would block the next card
// from becoming the default.
func (t *txRepo) SoftDeletePaymentMethod(ctx context.Context, id uuid.UUID, at time.Time) error {
	const q = `
		UPDATE payment_methods SET deleted_at = $2, is_default = FALSE, version = version + 1
		WHERE id = $1 AND deleted_at IS NULL`
	tag, err := t.tx.Exec(ctx, q, id, at)
	if err != nil {
		return fmt.Errorf("payment: soft delete payment method: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// PromoteOldestPaymentMethod gives a patient a default again after they
// deleted the one they had.
func (t *txRepo) PromoteOldestPaymentMethod(ctx context.Context, patientID uuid.UUID) error {
	const q = `
		UPDATE payment_methods SET is_default = TRUE, version = version + 1
		WHERE id = (
			SELECT id FROM payment_methods
			WHERE patient_id = $1 AND deleted_at IS NULL
			ORDER BY created_at
			LIMIT 1
		)`
	if _, err := t.tx.Exec(ctx, q, patientID); err != nil {
		return fmt.Errorf("payment: promote default payment method: %w", err)
	}
	return nil
}
