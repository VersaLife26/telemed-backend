package payment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
)

// repository is the pgx implementation of Store and Tx.
//
// It contains SQL and nothing else. It never builds an HTTP error, never
// decides a business rule, and never logs. Every statement is parameterised;
// there is no string concatenation anywhere in this file.

const paymentColumns = `
	id, appointment_id, patient_id, doctor_id, specialty, corporate_client,
	amount_cents, gross_amount_cents, discount_cents, promo_code,
	currency, provider, provider_intent_id, provider_reference, status,
	commission_cents, provider_fee_cents, doctor_payout_cents, refunded_cents,
	refunded_commission_cents, refunded_payout_cents,
	commission_rule_id, commission_rule_key, commission_rule_ver,
	idempotency_key, payout_id, failure_reason, succeeded_at,
	authorized_at, consultation_ended_at, completed_at, scheduled_start_at, authorization_token,
	created_at, updated_at, deleted_at, version`

const payoutColumns = `
	id, doctor_id, period_start, period_end, amount_cents, currency, payment_count,
	status, provider, transfer_id, failure_reason, idempotency_key,
	initiated_at, paid_at, attempts, created_at, updated_at, version`

const refundColumns = `
	id, payment_id, amount_cents, currency, reason, policy, percent,
	provider_refund_id, status, failure_reason, idempotency_key,
	created_at, updated_at, version`

const ruleColumns = `
	id, rule_key, version, scope, match_value, rate_bps, provider_fee_bps,
	provider_fee_fixed_cents, rounding, effective_from, effective_to, note,
	created_at, updated_at`

// Repository implements Store over a pgx pool.
type Repository struct {
	pool   database.Pool
	outbox *events.Outbox
}

// NewRepository wires the pool and the outbox together. The outbox is held
// here, not in the service, so that Tx.Enqueue is only reachable from inside a
// transaction.
func NewRepository(pool database.Pool, outbox *events.Outbox) *Repository {
	return &Repository{pool: pool, outbox: outbox}
}

var _ Store = (*Repository)(nil)

// InTx runs fn in one transaction, exposing the transactional port.
func (r *Repository) InTx(ctx context.Context, fn func(context.Context, Tx) error) error {
	return database.InTx(ctx, r.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return fn(ctx, &txRepo{tx: tx, outbox: r.outbox})
	})
}

// --- row scanning ----------------------------------------------------------

type rowScanner interface {
	Scan(dest ...any) error
}

func scanPayment(row rowScanner) (Payment, error) {
	var p Payment
	var ruleID, payoutID pgtype.UUID
	if err := row.Scan(
		&p.ID, &p.AppointmentID, &p.PatientID, &p.DoctorID, &p.Specialty, &p.CorporateClient,
		&p.AmountCents, &p.GrossAmountCents, &p.DiscountCents, &p.PromoCode,
		&p.Currency, &p.Provider, &p.ProviderIntentID, &p.ProviderReference, &p.Status,
		&p.CommissionCents, &p.ProviderFeeCents, &p.DoctorPayoutCents, &p.RefundedCents,
		&p.RefundedCommissionCents, &p.RefundedPayoutCents,
		&ruleID, &p.CommissionRuleKey, &p.CommissionRuleVer,
		&p.IdempotencyKey, &payoutID, &p.FailureReason, &p.SucceededAt,
		&p.AuthorizedAt, &p.ConsultationEndedAt, &p.CompletedAt, &p.ScheduledStartAt, &p.AuthorizationToken,
		&p.CreatedAt, &p.UpdatedAt, &p.DeletedAt, &p.Version,
	); err != nil {
		return Payment{}, err
	}
	p.CommissionRuleID = fromPgUUID(ruleID)
	p.PayoutID = fromPgUUID(payoutID)
	return p, nil
}

func scanPayout(row rowScanner) (Payout, error) {
	var p Payout
	if err := row.Scan(
		&p.ID, &p.DoctorID, &p.PeriodStart, &p.PeriodEnd, &p.AmountCents, &p.Currency, &p.PaymentCount,
		&p.Status, &p.Provider, &p.TransferID, &p.FailureReason, &p.IdempotencyKey,
		&p.InitiatedAt, &p.PaidAt, &p.Attempts, &p.CreatedAt, &p.UpdatedAt, &p.Version,
	); err != nil {
		return Payout{}, err
	}
	return p, nil
}

func scanRefund(row rowScanner) (Refund, error) {
	var rf Refund
	if err := row.Scan(
		&rf.ID, &rf.PaymentID, &rf.AmountCents, &rf.Currency, &rf.Reason, &rf.Policy, &rf.Percent,
		&rf.ProviderRefundID, &rf.Status, &rf.FailureReason, &rf.IdempotencyKey,
		&rf.CreatedAt, &rf.UpdatedAt, &rf.Version,
	); err != nil {
		return Refund{}, err
	}
	return rf, nil
}

func scanRule(row rowScanner) (CommissionRule, error) {
	var c CommissionRule
	if err := row.Scan(
		&c.ID, &c.RuleKey, &c.Version, &c.Scope, &c.MatchValue, &c.RateBps, &c.ProviderFeeBps,
		&c.ProviderFeeFixedCents, &c.Rounding, &c.EffectiveFrom, &c.EffectiveTo, &c.Note,
		&c.CreatedAt, &c.UpdatedAt,
	); err != nil {
		return CommissionRule{}, err
	}
	return c, nil
}

func fromPgUUID(v pgtype.UUID) *uuid.UUID {
	if !v.Valid {
		return nil
	}
	id := uuid.UUID(v.Bytes)
	return &id
}

func toPgUUID(v *uuid.UUID) pgtype.UUID {
	if v == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *v, Valid: true}
}

// notFound translates pgx's no-rows sentinel into the domain sentinel so the
// service layer never imports pgx.
func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// --- Store reads -----------------------------------------------------------

// GetPayment loads one payment by id.
func (r *Repository) GetPayment(ctx context.Context, id uuid.UUID) (Payment, error) {
	q := `SELECT ` + paymentColumns + ` FROM payments WHERE id = $1 AND deleted_at IS NULL`
	p, err := scanPayment(r.pool.QueryRow(ctx, q, id))
	if err != nil {
		return Payment{}, notFound(err)
	}
	return p, nil
}

// GetPaymentByAppointment loads the single payment for an appointment.
func (r *Repository) GetPaymentByAppointment(ctx context.Context, appointmentID uuid.UUID) (Payment, error) {
	q := `SELECT ` + paymentColumns + ` FROM payments WHERE appointment_id = $1 AND deleted_at IS NULL`
	p, err := scanPayment(r.pool.QueryRow(ctx, q, appointmentID))
	if err != nil {
		return Payment{}, notFound(err)
	}
	return p, nil
}

func (r *Repository) listPayments(ctx context.Context, where string, id uuid.UUID, p Page) ([]Payment, int64, error) {
	var total int64
	countQ := `SELECT COUNT(*) FROM payments WHERE ` + where + ` = $1 AND deleted_at IS NULL`
	if err := r.pool.QueryRow(ctx, countQ, id).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("payment: count payments: %w", err)
	}
	if total == 0 {
		return []Payment{}, 0, nil
	}

	q := `SELECT ` + paymentColumns + ` FROM payments WHERE ` + where +
		` = $1 AND deleted_at IS NULL ORDER BY created_at DESC LIMIT $2 OFFSET $3`
	rows, err := r.pool.Query(ctx, q, id, p.PerPage, p.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("payment: list payments: %w", err)
	}
	defer rows.Close()

	out := make([]Payment, 0, p.PerPage)
	for rows.Next() {
		item, err := scanPayment(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("payment: scan payment: %w", err)
		}
		out = append(out, item)
	}
	return out, total, rows.Err()
}

// ListPaymentsForPatient returns a patient's own payments, newest first.
func (r *Repository) ListPaymentsForPatient(ctx context.Context, patientID uuid.UUID, p Page) ([]Payment, int64, error) {
	return r.listPayments(ctx, "patient_id", patientID, p)
}

// ListPaymentsForDoctor returns a doctor's payments, newest first.
func (r *Repository) ListPaymentsForDoctor(ctx context.Context, doctorID uuid.UUID, p Page) ([]Payment, int64, error) {
	return r.listPayments(ctx, "doctor_id", doctorID, p)
}

// ListPaymentsForPayout returns every payment settled by one payout, which is
// the line-item detail on a doctor's remittance advice.
func (r *Repository) ListPaymentsForPayout(ctx context.Context, payoutID uuid.UUID) ([]Payment, error) {
	q := `SELECT ` + paymentColumns + ` FROM payments WHERE payout_id = $1 ORDER BY succeeded_at`
	rows, err := r.pool.Query(ctx, q, payoutID)
	if err != nil {
		return nil, fmt.Errorf("payment: list payout payments: %w", err)
	}
	defer rows.Close()

	var out []Payment
	for rows.Next() {
		item, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ListRefundsForPayment returns every refund against a payment.
func (r *Repository) ListRefundsForPayment(ctx context.Context, paymentID uuid.UUID) ([]Refund, error) {
	q := `SELECT ` + refundColumns + ` FROM refunds WHERE payment_id = $1 ORDER BY created_at`
	rows, err := r.pool.Query(ctx, q, paymentID)
	if err != nil {
		return nil, fmt.Errorf("payment: list refunds: %w", err)
	}
	defer rows.Close()

	out := []Refund{}
	for rows.Next() {
		item, err := scanRefund(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// GetPayout loads one payout.
func (r *Repository) GetPayout(ctx context.Context, id uuid.UUID) (Payout, error) {
	q := `SELECT ` + payoutColumns + ` FROM payouts WHERE id = $1 AND deleted_at IS NULL`
	p, err := scanPayout(r.pool.QueryRow(ctx, q, id))
	if err != nil {
		return Payout{}, notFound(err)
	}
	return p, nil
}

func (r *Repository) listPayouts(ctx context.Context, where string, args []any, p Page) ([]Payout, int64, error) {
	var total int64
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM payouts WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("payment: count payouts: %w", err)
	}
	if total == 0 {
		return []Payout{}, 0, nil
	}
	q := `SELECT ` + payoutColumns + ` FROM payouts WHERE ` + where +
		fmt.Sprintf(` ORDER BY period_start DESC, created_at DESC LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)
	rows, err := r.pool.Query(ctx, q, append(args, p.PerPage, p.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("payment: list payouts: %w", err)
	}
	defer rows.Close()

	out := make([]Payout, 0, p.PerPage)
	for rows.Next() {
		item, err := scanPayout(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, item)
	}
	return out, total, rows.Err()
}

// ListPayoutsForDoctor returns one doctor's settlement history.
func (r *Repository) ListPayoutsForDoctor(ctx context.Context, doctorID uuid.UUID, p Page) ([]Payout, int64, error) {
	return r.listPayouts(ctx, `doctor_id = $1 AND deleted_at IS NULL`, []any{doctorID}, p)
}

// ListAllPayouts returns every payout, for finance and super_admin.
func (r *Repository) ListAllPayouts(ctx context.Context, p Page) ([]Payout, int64, error) {
	return r.listPayouts(ctx, `deleted_at IS NULL`, nil, p)
}

// UnsettledPayoutIDs lists payouts that still owe a provider transfer. It
// deliberately includes payouts created by a previous, crashed run: resuming
// them is what makes the daily job safe to re-run.
func (r *Repository) UnsettledPayoutIDs(ctx context.Context, limit int) ([]uuid.UUID, error) {
	const q = `
		SELECT id FROM payouts
		WHERE status IN ('pending', 'processing') AND deleted_at IS NULL
		ORDER BY created_at
		LIMIT $1`
	rows, err := r.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("payment: unsettled payouts: %w", err)
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

// DoctorsWithDuePayments finds doctors holding settleable money. The `before`
// cutoff implements the hold period: money is only paid out once the chargeback
// window has had a day to produce a dispute.
func (r *Repository) DoctorsWithDuePayments(ctx context.Context, before time.Time, limit int) ([]DoctorDue, error) {
	const q = `
		SELECT doctor_id,
		       SUM(doctor_payout_cents - refunded_payout_cents)::BIGINT AS amount_cents,
		       COUNT(*)::INT AS payment_count,
		       MIN(currency) AS currency
		FROM payments
		WHERE payout_id IS NULL
		  AND deleted_at IS NULL
		  AND status IN ('succeeded', 'partially_refunded')
		  AND succeeded_at IS NOT NULL
		  AND succeeded_at < $1
		  AND doctor_payout_cents - refunded_payout_cents > 0
		GROUP BY doctor_id
		ORDER BY doctor_id
		LIMIT $2`
	rows, err := r.pool.Query(ctx, q, before, limit)
	if err != nil {
		return nil, fmt.Errorf("payment: doctors with due payments: %w", err)
	}
	defer rows.Close()

	var out []DoctorDue
	for rows.Next() {
		var d DoctorDue
		if err := rows.Scan(&d.DoctorID, &d.AmountCents, &d.PaymentCount, &d.Currency); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ActiveRules returns every currently open commission rule version.
func (r *Repository) ActiveRules(ctx context.Context) ([]CommissionRule, error) {
	q := `SELECT ` + ruleColumns + ` FROM commission_rules
	      WHERE effective_to IS NULL AND effective_from <= NOW()
	      ORDER BY rule_key`
	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("payment: active rules: %w", err)
	}
	defer rows.Close()

	out := []CommissionRule{}
	for rows.Next() {
		c, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetRule loads one specific rule version by id. This is what makes an invoice
// reproducible: the payment stored the id, so the exact historical rate is
// always retrievable regardless of what the current rate is.
func (r *Repository) GetRule(ctx context.Context, id uuid.UUID) (CommissionRule, error) {
	q := `SELECT ` + ruleColumns + ` FROM commission_rules WHERE id = $1`
	c, err := scanRule(r.pool.QueryRow(ctx, q, id))
	if err != nil {
		return CommissionRule{}, notFound(err)
	}
	return c, nil
}

// CountRules reports how many rules exist, used to decide whether the boot
// seed should run.
func (r *Repository) CountRules(ctx context.Context) (int, error) {
	var n int
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM commission_rules`).Scan(&n); err != nil {
		return 0, fmt.Errorf("payment: count rules: %w", err)
	}
	return n, nil
}

// SeedRules inserts bootstrap rules, skipping any rule_key/version that already
// exists. It is safe to run on every boot of every replica.
func (r *Repository) SeedRules(ctx context.Context, rules []CommissionRule) (int, error) {
	const q = `
		INSERT INTO commission_rules
			(id, rule_key, version, scope, match_value, rate_bps, provider_fee_bps,
			 provider_fee_fixed_cents, rounding, effective_from, note)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (rule_key, version) DO NOTHING`
	inserted := 0
	for i := range rules {
		c := &rules[i]
		tag, err := r.pool.Exec(ctx, q, c.ID, c.RuleKey, c.Version, c.Scope, c.MatchValue,
			c.RateBps, c.ProviderFeeBps, c.ProviderFeeFixedCents, c.Rounding, c.EffectiveFrom, c.Note)
		if err != nil {
			return inserted, fmt.Errorf("payment: seed rule %s: %w", c.RuleKey, err)
		}
		inserted += int(tag.RowsAffected())
	}
	return inserted, nil
}

// LedgerForPayment returns every ledger leg touching a payment, oldest first.
func (r *Repository) LedgerForPayment(ctx context.Context, paymentID uuid.UUID) ([]LedgerEntry, error) {
	const q = `
		SELECT id, entry_id, entry_type, account, direction, amount_cents, currency,
		       payment_id, payout_id, refund_id, doctor_id, description, occurred_at, created_at
		FROM ledger_entries
		WHERE payment_id = $1
		ORDER BY id`
	rows, err := r.pool.Query(ctx, q, paymentID)
	if err != nil {
		return nil, fmt.Errorf("payment: ledger for payment: %w", err)
	}
	defer rows.Close()

	out := []LedgerEntry{}
	for rows.Next() {
		var e LedgerEntry
		var pid, poid, rid, did pgtype.UUID
		if err := rows.Scan(&e.ID, &e.EntryID, &e.EntryType, &e.Account, &e.Direction,
			&e.AmountCents, &e.Currency, &pid, &poid, &rid, &did,
			&e.Description, &e.OccurredAt, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.PaymentID, e.PayoutID, e.RefundID, e.DoctorID = fromPgUUID(pid), fromPgUUID(poid), fromPgUUID(rid), fromPgUUID(did)
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- Tx --------------------------------------------------------------------

type txRepo struct {
	tx     pgx.Tx
	outbox *events.Outbox
}

var _ Tx = (*txRepo)(nil)

// InsertPayment writes a new payment row.
func (t *txRepo) InsertPayment(ctx context.Context, p *Payment) error {
	const q = `
		INSERT INTO payments (
			id, appointment_id, patient_id, doctor_id, specialty, corporate_client,
			amount_cents, gross_amount_cents, discount_cents, promo_code,
			currency, provider, provider_intent_id, provider_reference, status,
			commission_cents, provider_fee_cents, doctor_payout_cents, refunded_cents,
			refunded_commission_cents, refunded_payout_cents,
			commission_rule_id, commission_rule_key, commission_rule_ver,
			idempotency_key, failure_reason, succeeded_at,
			authorized_at, consultation_ended_at, completed_at, scheduled_start_at, authorization_token
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32)
		RETURNING created_at, updated_at, version`
	err := t.tx.QueryRow(ctx, q,
		p.ID, p.AppointmentID, p.PatientID, p.DoctorID, p.Specialty, p.CorporateClient,
		p.AmountCents, p.GrossAmountCents, p.DiscountCents, p.PromoCode,
		p.Currency, p.Provider, p.ProviderIntentID, p.ProviderReference, p.Status,
		p.CommissionCents, p.ProviderFeeCents, p.DoctorPayoutCents, p.RefundedCents,
		p.RefundedCommissionCents, p.RefundedPayoutCents,
		toPgUUID(p.CommissionRuleID), p.CommissionRuleKey, p.CommissionRuleVer,
		p.IdempotencyKey, p.FailureReason, p.SucceededAt,
		p.AuthorizedAt, p.ConsultationEndedAt, p.CompletedAt, p.ScheduledStartAt, p.AuthorizationToken,
	).Scan(&p.CreatedAt, &p.UpdatedAt, &p.Version)
	if err != nil {
		if database.IsUniqueViolation(err) {
			return fmt.Errorf("%w: payment for appointment %s", ErrDuplicate, p.AppointmentID)
		}
		return fmt.Errorf("payment: insert payment: %w", err)
	}
	return nil
}

func (t *txRepo) lockPayment(ctx context.Context, where string, args ...any) (Payment, error) {
	q := `SELECT ` + paymentColumns + ` FROM payments WHERE ` + where + ` AND deleted_at IS NULL FOR UPDATE`
	p, err := scanPayment(t.tx.QueryRow(ctx, q, args...))
	if err != nil {
		return Payment{}, notFound(err)
	}
	return p, nil
}

// LockPayment takes a row lock so concurrent webhooks serialise.
func (t *txRepo) LockPayment(ctx context.Context, id uuid.UUID) (Payment, error) {
	return t.lockPayment(ctx, `id = $1`, id)
}

// LockPaymentByIntent finds and locks the payment a provider event refers to.
func (t *txRepo) LockPaymentByIntent(ctx context.Context, provider ProviderName, intentID string) (Payment, error) {
	return t.lockPayment(ctx, `provider = $1 AND provider_intent_id = $2`, provider, intentID)
}

// LockPaymentByAppointment finds and locks by appointment.
func (t *txRepo) LockPaymentByAppointment(ctx context.Context, appointmentID uuid.UUID) (Payment, error) {
	return t.lockPayment(ctx, `appointment_id = $1`, appointmentID)
}

// UpdatePayment writes the mutable fields under an optimistic-lock guard.
func (t *txRepo) UpdatePayment(ctx context.Context, p *Payment) error {
	const q = `
		UPDATE payments SET
			provider = $2, provider_intent_id = $3, provider_reference = $4, status = $5,
			commission_cents = $6, provider_fee_cents = $7, doctor_payout_cents = $8,
			refunded_cents = $9, refunded_commission_cents = $10, refunded_payout_cents = $11,
			commission_rule_id = $12, commission_rule_key = $13, commission_rule_ver = $14,
			payout_id = $15, failure_reason = $16, succeeded_at = $17,
			amount_cents = $19, discount_cents = $20, promo_code = $21,
			authorized_at = $22, consultation_ended_at = $23, completed_at = $24, authorization_token = $25,
			version = version + 1
		WHERE id = $1 AND version = $18 AND deleted_at IS NULL
		RETURNING version, updated_at`
	// gross_amount_cents is deliberately absent from the SET list: it is the
	// doctor's list price, frozen when the payment was created, and nothing
	// after creation has any business moving it. A promotion changes
	// amount_cents and discount_cents; the CHECK constraint then proves the
	// three still reconstitute each other.
	err := t.tx.QueryRow(ctx, q,
		p.ID, p.Provider, p.ProviderIntentID, p.ProviderReference, p.Status,
		p.CommissionCents, p.ProviderFeeCents, p.DoctorPayoutCents,
		p.RefundedCents, p.RefundedCommissionCents, p.RefundedPayoutCents,
		toPgUUID(p.CommissionRuleID), p.CommissionRuleKey, p.CommissionRuleVer,
		toPgUUID(p.PayoutID), p.FailureReason, p.SucceededAt, p.Version,
		p.AmountCents, p.DiscountCents, p.PromoCode,
		p.AuthorizedAt, p.ConsultationEndedAt, p.CompletedAt, p.AuthorizationToken,
	).Scan(&p.Version, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: payment %s at version %d", ErrVersionConflict, p.ID, p.Version)
	}
	if err != nil {
		return fmt.Errorf("payment: update payment: %w", err)
	}
	return nil
}

// ClaimWebhookEvent is the idempotency gate.
//
// ON CONFLICT DO NOTHING on (provider, event_id) means a redelivered webhook
// returns false and the caller does nothing else. Two concurrent deliveries of
// the same event serialise on the index: the second blocks until the first
// commits, then finds the conflict.
//
// READ THIS BEFORE ADDING A RETENTION JOB TO webhook_events.
//
// For PayHere this index is the primary replay defence, and it is the only one
// that depends on state we keep. PayHere's notify carries no timestamp and no
// nonce, so nothing in a captured body expires; the field-grammar validation in
// the provider makes the MD5 preimage injective, so an attacker cannot alter a
// field and keep the signature. What they can hold is a byte-identical copy of
// a genuine notify, for ever, and this row is what stops it settling twice.
//
// That makes the defence silently coupled to webhook_events never being
// pruned. It has no retention policy today. The day somebody adds a sensible
// 90-day one -- and SECURITY-REVIEW F24 recommends exactly that -- every notify
// older than the horizon becomes replayable through this gate.
//
// It is not the ONLY layer, and that is deliberate:
//
//   - applyWebhook short-circuits on Status.Settled() before applyCapture, so a
//     replay against an already-settled payment writes no second ledger leg.
//   - checkReportedAmount refuses a figure that disagrees with the payment row.
//   - sameRail refuses a callback for a payment on another provider.
//
// TestIntegrationPayHereReplayHasASecondLayer proves the first of those by
// DELETEing the dedup row and replaying a genuine captured notify.
//
// So a retention job is permissible, but it must be a deliberate decision that
// says "the second layer is now the primary one", not a tidy-up. If you add
// one, keep the horizon longer than any window in which a payment can still
// change state (the hold period plus the refund window), and say so here.
func (t *txRepo) ClaimWebhookEvent(ctx context.Context, rec *WebhookEventRecord) (bool, error) {
	const q = `
		INSERT INTO webhook_events (id, provider, event_id, event_type, payment_id, payload, signature_header, received_at, attempts)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,1)
		ON CONFLICT (provider, event_id) DO NOTHING
		RETURNING id, received_at`
	err := t.tx.QueryRow(ctx, q,
		rec.ID, rec.Provider, rec.EventID, rec.EventType, toPgUUID(rec.PaymentID),
		rec.Payload, rec.SignatureHeader, rec.ReceivedAt,
	).Scan(&rec.ID, &rec.ReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // already seen: this is the happy replay path
	}
	if err != nil {
		return false, fmt.Errorf("payment: claim webhook event: %w", err)
	}
	return true, nil
}

// MarkWebhookProcessed closes out the audit row.
func (t *txRepo) MarkWebhookProcessed(ctx context.Context, id uuid.UUID, paymentID *uuid.UUID, processingErr string) error {
	const q = `
		UPDATE webhook_events
		SET processed_at = NOW(), processing_error = $2, payment_id = COALESCE($3, payment_id)
		WHERE id = $1`
	if _, err := t.tx.Exec(ctx, q, id, processingErr, toPgUUID(paymentID)); err != nil {
		return fmt.Errorf("payment: mark webhook processed: %w", err)
	}
	return nil
}

// InsertRefund writes a new refund row.
func (t *txRepo) InsertRefund(ctx context.Context, r *Refund) error {
	const q = `
		INSERT INTO refunds (id, payment_id, amount_cents, currency, reason, policy, percent,
		                     provider_refund_id, status, failure_reason, idempotency_key)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING created_at, updated_at, version`
	err := t.tx.QueryRow(ctx, q, r.ID, r.PaymentID, r.AmountCents, r.Currency, r.Reason, r.Policy,
		r.Percent, r.ProviderRefundID, r.Status, r.FailureReason, r.IdempotencyKey,
	).Scan(&r.CreatedAt, &r.UpdatedAt, &r.Version)
	if err != nil {
		if database.IsUniqueViolation(err) {
			return fmt.Errorf("%w: refund %s", ErrDuplicate, r.IdempotencyKey)
		}
		return fmt.Errorf("payment: insert refund: %w", err)
	}
	return nil
}

// FindRefundByKey looks a refund up by its idempotency key so a repeated
// request returns the original refund rather than issuing a second one.
func (t *txRepo) FindRefundByKey(ctx context.Context, key string) (Refund, error) {
	q := `SELECT ` + refundColumns + ` FROM refunds WHERE idempotency_key = $1 FOR UPDATE`
	r, err := scanRefund(t.tx.QueryRow(ctx, q, key))
	if err != nil {
		return Refund{}, notFound(err)
	}
	return r, nil
}

// FindRefundByProviderID matches a rail's refund confirmation back to our row.
func (t *txRepo) FindRefundByProviderID(ctx context.Context, providerRefundID string) (Refund, error) {
	q := `SELECT ` + refundColumns + ` FROM refunds WHERE provider_refund_id = $1 FOR UPDATE`
	r, err := scanRefund(t.tx.QueryRow(ctx, q, providerRefundID))
	if err != nil {
		return Refund{}, notFound(err)
	}
	return r, nil
}

// UpdateRefund writes the provider outcome back.
func (t *txRepo) UpdateRefund(ctx context.Context, r *Refund) error {
	const q = `
		UPDATE refunds SET provider_refund_id = $2, status = $3, failure_reason = $4, version = version + 1
		WHERE id = $1 AND version = $5
		RETURNING version, updated_at`
	err := t.tx.QueryRow(ctx, q, r.ID, r.ProviderRefundID, r.Status, r.FailureReason, r.Version).
		Scan(&r.Version, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: refund %s", ErrVersionConflict, r.ID)
	}
	if err != nil {
		return fmt.Errorf("payment: update refund: %w", err)
	}
	return nil
}

// InsertPayout writes a new payout batch.
func (t *txRepo) InsertPayout(ctx context.Context, p *Payout) error {
	const q = `
		INSERT INTO payouts (id, doctor_id, period_start, period_end, amount_cents, currency,
		                     payment_count, status, provider, transfer_id, failure_reason,
		                     idempotency_key, initiated_at, paid_at, attempts)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		RETURNING created_at, updated_at, version`
	err := t.tx.QueryRow(ctx, q, p.ID, p.DoctorID, p.PeriodStart, p.PeriodEnd, p.AmountCents,
		p.Currency, p.PaymentCount, p.Status, p.Provider, p.TransferID, p.FailureReason,
		p.IdempotencyKey, p.InitiatedAt, p.PaidAt, p.Attempts,
	).Scan(&p.CreatedAt, &p.UpdatedAt, &p.Version)
	if err != nil {
		if database.IsUniqueViolation(err) {
			return fmt.Errorf("%w: payout for doctor %s period %s", ErrDuplicate, p.DoctorID, p.PeriodStart.Format(time.DateOnly))
		}
		return fmt.Errorf("payment: insert payout: %w", err)
	}
	return nil
}

// LockPayout takes a row lock on a payout batch.
func (t *txRepo) LockPayout(ctx context.Context, id uuid.UUID) (Payout, error) {
	q := `SELECT ` + payoutColumns + ` FROM payouts WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`
	p, err := scanPayout(t.tx.QueryRow(ctx, q, id))
	if err != nil {
		return Payout{}, notFound(err)
	}
	return p, nil
}

// FindPayoutByPeriod finds an existing batch for a doctor and period, which is
// how a re-run discovers what the crashed run already did.
func (t *txRepo) FindPayoutByPeriod(ctx context.Context, doctorID uuid.UUID, start, end time.Time) (Payout, error) {
	q := `SELECT ` + payoutColumns + ` FROM payouts
	      WHERE doctor_id = $1 AND period_start = $2 AND period_end = $3 FOR UPDATE`
	p, err := scanPayout(t.tx.QueryRow(ctx, q, doctorID, start, end))
	if err != nil {
		return Payout{}, notFound(err)
	}
	return p, nil
}

// UpdatePayout writes the settlement outcome under an optimistic-lock guard.
func (t *txRepo) UpdatePayout(ctx context.Context, p *Payout) error {
	const q = `
		UPDATE payouts SET amount_cents = $2, payment_count = $3, status = $4, transfer_id = $5,
		                   failure_reason = $6, initiated_at = $7, paid_at = $8, attempts = $9,
		                   version = version + 1
		WHERE id = $1 AND version = $10
		RETURNING version, updated_at`
	err := t.tx.QueryRow(ctx, q, p.ID, p.AmountCents, p.PaymentCount, p.Status, p.TransferID,
		p.FailureReason, p.InitiatedAt, p.PaidAt, p.Attempts, p.Version,
	).Scan(&p.Version, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: payout %s", ErrVersionConflict, p.ID)
	}
	if err != nil {
		return fmt.Errorf("payment: update payout: %w", err)
	}
	return nil
}

// ClaimPaymentsForPayout stamps a payout onto every eligible unclaimed payment.
//
// `payout_id IS NULL` in the predicate is the entire double-payment defence: a
// payment can only ever be claimed once, so a re-run after a crash finds
// nothing left to claim and the doctor is not paid twice.
func (t *txRepo) ClaimPaymentsForPayout(ctx context.Context, payoutID, doctorID uuid.UUID, before time.Time) (count int, totalCents int64, err error) {
	const q = `
		UPDATE payments
		SET payout_id = $1, version = version + 1
		WHERE doctor_id = $2
		  AND payout_id IS NULL
		  AND deleted_at IS NULL
		  AND status IN ('succeeded', 'partially_refunded')
		  AND succeeded_at IS NOT NULL
		  AND succeeded_at < $3
		  AND doctor_payout_cents - refunded_payout_cents > 0
		RETURNING doctor_payout_cents - refunded_payout_cents`
	rows, err := t.tx.Query(ctx, q, payoutID, doctorID, before)
	if err != nil {
		return 0, 0, fmt.Errorf("payment: claim payments for payout: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var net int64
		if err := rows.Scan(&net); err != nil {
			return 0, 0, err
		}
		count++
		totalCents += net
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	return count, totalCents, nil
}

// ReleasePaymentsFromPayout unclaims payments when a batch is abandoned, so the
// money becomes eligible again on the next run rather than being stranded.
func (t *txRepo) ReleasePaymentsFromPayout(ctx context.Context, payoutID uuid.UUID) (int, error) {
	const q = `UPDATE payments SET payout_id = NULL, version = version + 1 WHERE payout_id = $1`
	tag, err := t.tx.Exec(ctx, q, payoutID)
	if err != nil {
		return 0, fmt.Errorf("payment: release payments from payout: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// WriteLedger appends a balanced transaction. An unbalanced set is refused
// before it reaches the database, because an append-only table cannot be
// corrected afterwards -- that is the point of it.
func (t *txRepo) WriteLedger(ctx context.Context, lt LedgerTransaction) error {
	if !lt.Balanced() {
		return fmt.Errorf("payment: refusing to write unbalanced ledger transaction %s (%s)", lt.EntryID, lt.EntryType)
	}
	const q = `
		INSERT INTO ledger_entries (entry_id, entry_type, account, direction, amount_cents,
		                            currency, payment_id, payout_id, refund_id, doctor_id,
		                            description, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`
	for i := range lt.Legs {
		leg := &lt.Legs[i]
		if _, err := t.tx.Exec(ctx, q, lt.EntryID, lt.EntryType, leg.Account, leg.Direction,
			leg.AmountCents, leg.Currency, toPgUUID(leg.PaymentID), toPgUUID(leg.PayoutID),
			toPgUUID(leg.RefundID), toPgUUID(leg.DoctorID), leg.Description, leg.OccurredAt); err != nil {
			return fmt.Errorf("payment: write ledger leg %s: %w", leg.Account, err)
		}
	}
	return nil
}

// Enqueue writes a domain event to the outbox on this transaction.
func (t *txRepo) Enqueue(ctx context.Context, subject events.Subject, aggregateID string, payload any) error {
	return t.outbox.Enqueue(ctx, t.tx, subject, aggregateID, payload)
}
