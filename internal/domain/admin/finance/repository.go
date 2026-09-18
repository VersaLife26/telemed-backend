package finance

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
)

type Repository struct {
	pool database.Pool
}

func NewRepository(pool database.Pool) *Repository { return &Repository{pool: pool} }

func (r *Repository) List(ctx context.Context, f LedgerFilter) ([]LedgerEntry, int64, error) {
	where, args := ledgerFilter(f)

	var total int64
	if err := r.pool.QueryRow(ctx, "SELECT COUNT(*) FROM payments_projection"+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("finance: count: %w", err)
	}

	perPage := f.PerPage
	if perPage <= 0 {
		perPage = 20
	}
	page := f.Page
	if page <= 0 {
		page = 1
	}
	args = append(args, perPage, (page-1)*perPage)
	q := fmt.Sprintf(`
		SELECT payment_id, appointment_id, doctor_id, patient_id, amount_cents, commission_cents,
		       currency, status, provider, occurred_at
		FROM payments_projection%s ORDER BY occurred_at DESC LIMIT $%d OFFSET $%d`,
		where, len(args)-1, len(args))

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("finance: list: %w", err)
	}
	defer rows.Close()

	out, err := scanAll(rows)
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// CountMatching is how many rows an export would produce. It runs before any
// bytes go on the wire so an oversized or unbounded request is refused with a
// status code rather than with a half-written file.
func (r *Repository) CountMatching(ctx context.Context, f LedgerFilter) (int64, error) {
	where, args := ledgerFilter(f)
	var n int64
	if err := r.pool.QueryRow(ctx, "SELECT COUNT(*) FROM payments_projection"+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("finance: count matching: %w", err)
	}
	return n, nil
}

// StreamExport calls emit for every entry matching f, oldest first, without
// materialising the result set -- a financial report can cover a wide date
// range.
func (r *Repository) StreamExport(ctx context.Context, f LedgerFilter, emit func(LedgerEntry) error) error {
	where, args := ledgerFilter(f)
	q := fmt.Sprintf(`
		SELECT payment_id, appointment_id, doctor_id, patient_id, amount_cents, commission_cents,
		       currency, status, provider, occurred_at
		FROM payments_projection%s ORDER BY occurred_at ASC`, where)

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("finance: export query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return err
		}
		if err := emit(e); err != nil {
			return err
		}
	}
	return rows.Err()
}

func ledgerFilter(f LedgerFilter) (clause string, args []any) {
	var clauses []string
	if f.Status != "" {
		args = append(args, f.Status)
		clauses = append(clauses, fmt.Sprintf("status = $%d", len(args)))
	}
	if f.DoctorID != uuid.Nil {
		args = append(args, f.DoctorID)
		clauses = append(clauses, fmt.Sprintf("doctor_id = $%d", len(args)))
	}
	if !f.From.IsZero() {
		args = append(args, f.From)
		clauses = append(clauses, fmt.Sprintf("occurred_at >= $%d", len(args)))
	}
	if !f.To.IsZero() {
		args = append(args, f.To)
		clauses = append(clauses, fmt.Sprintf("occurred_at <= $%d", len(args)))
	}
	if len(clauses) == 0 {
		return "", args
	}
	where := " WHERE "
	for i, c := range clauses {
		if i > 0 {
			where += " AND "
		}
		where += c
	}
	return where, args
}

func scanAll(rows pgx.Rows) ([]LedgerEntry, error) {
	var out []LedgerEntry
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scan(row rowScanner) (LedgerEntry, error) {
	var e LedgerEntry
	var appt, doc, pat *uuid.UUID
	var provider *string
	err := row.Scan(&e.PaymentID, &appt, &doc, &pat, &e.AmountCents, &e.CommissionCents,
		&e.Currency, &e.Status, &provider, &e.OccurredAt)
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("finance: scan: %w", err)
	}
	if appt != nil {
		e.AppointmentID = *appt
	}
	if doc != nil {
		e.DoctorID = *doc
	}
	if pat != nil {
		e.PatientID = *pat
	}
	if provider != nil {
		e.Provider = *provider
	}
	return e, nil
}

func (r *Repository) ListRefunds(ctx context.Context, status string, page, perPage int) ([]RefundRequest, int64, error) {
	if perPage <= 0 {
		perPage = 20
	}
	if page <= 0 {
		page = 1
	}
	args := []any{}
	where := ""
	if status != "" {
		args = append(args, status)
		where = " WHERE status = $1"
	}
	var total int64
	if err := r.pool.QueryRow(ctx, "SELECT COUNT(*) FROM finance_refund_requests"+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("finance: count refunds: %w", err)
	}
	args = append(args, perPage, (page-1)*perPage)
	q := fmt.Sprintf(`
		SELECT id, payment_id, appointment_id, dispute_id, amount_cents, currency, reason,
		       status, requested_at, decided_at, decided_by
		FROM finance_refund_requests%s
		ORDER BY requested_at DESC LIMIT $%d OFFSET $%d`, where, len(args)-1, len(args))
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("finance: list refunds: %w", err)
	}
	defer rows.Close()
	var out []RefundRequest
	for rows.Next() {
		item, err := scanRefund(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, item)
	}
	return out, total, rows.Err()
}

func (r *Repository) GetRefund(ctx context.Context, id uuid.UUID) (RefundRequest, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT id, payment_id, appointment_id, dispute_id, amount_cents, currency, reason,
		       status, requested_at, decided_at, decided_by
		FROM finance_refund_requests WHERE id = $1`, id)
	return scanRefund(row)
}

func (r *Repository) InsertRefund(ctx context.Context, req RefundRequest) (RefundRequest, error) {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO finance_refund_requests (
			payment_id, appointment_id, dispute_id, amount_cents, currency, reason, status
		) VALUES ($1, NULLIF($2, '00000000-0000-0000-0000-000000000000'::uuid),
		          NULLIF($3, '00000000-0000-0000-0000-000000000000'::uuid),
		          $4, $5, $6, 'pending')
		ON CONFLICT (payment_id) WHERE status = 'pending' DO UPDATE SET reason = EXCLUDED.reason
		RETURNING id, payment_id, appointment_id, dispute_id, amount_cents, currency, reason,
		          status, requested_at, decided_at, decided_by`,
		req.PaymentID, req.AppointmentID, req.DisputeID, req.AmountCents, req.Currency, req.Reason)
	return scanRefund(row)
}

func (r *Repository) DecideRefund(ctx context.Context, id, actor uuid.UUID, status string) (RefundRequest, error) {
	row := r.pool.QueryRow(ctx, `
		UPDATE finance_refund_requests
		SET status = $2, decided_at = NOW(), decided_by = $3
		WHERE id = $1 AND status = 'pending'
		RETURNING id, payment_id, appointment_id, dispute_id, amount_cents, currency, reason,
		          status, requested_at, decided_at, decided_by`,
		id, status, actor)
	return scanRefund(row)
}

func (r *Repository) PaymentByAppointment(ctx context.Context, appointmentID uuid.UUID) (LedgerEntry, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT payment_id, appointment_id, doctor_id, patient_id, amount_cents, commission_cents,
		       currency, status, provider, occurred_at
		FROM payments_projection
		WHERE appointment_id = $1
		ORDER BY occurred_at DESC
		LIMIT 1`, appointmentID)
	return scan(row)
}

func (r *Repository) ListPayoutBatches(ctx context.Context, page, perPage int) ([]PayoutBatch, int64, error) {
	if perPage <= 0 {
		perPage = 10
	}
	if page <= 0 {
		page = 1
	}
	var total int64
	if err := r.pool.QueryRow(ctx, "SELECT COUNT(*) FROM payout_batches_projection").Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("finance: count payout batches: %w", err)
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id, status, doctor_count, total_cents, currency, created_at, completed_at
		FROM payout_batches_projection
		ORDER BY created_at DESC LIMIT $1 OFFSET $2`, perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, fmt.Errorf("finance: list payout batches: %w", err)
	}
	defer rows.Close()
	var out []PayoutBatch
	for rows.Next() {
		var b PayoutBatch
		if err := rows.Scan(&b.ID, &b.Status, &b.DoctorCount, &b.TotalCents, &b.Currency, &b.CreatedAt, &b.CompletedAt); err != nil {
			return nil, 0, fmt.Errorf("finance: scan payout batch: %w", err)
		}
		out = append(out, b)
	}
	return out, total, rows.Err()
}

func (r *Repository) UpsertPayoutBatchRequest(ctx context.Context, from, to time.Time) error {
	start := from.UTC().Format("2006-01-02")
	end := to.UTC().Format("2006-01-02")
	_, err := r.pool.Exec(ctx, `
		INSERT INTO payout_batches_projection (period_start, period_end, status, created_at)
		VALUES ($1::date, $2::date, 'pending', NOW())
		ON CONFLICT (period_start, period_end) DO NOTHING`, start, end)
	return err
}

func (r *Repository) ApplyPayoutSent(ctx context.Context, periodStart, periodEnd string, amountCents int64, currency string, sentAt time.Time) error {
	if periodStart == "" || periodEnd == "" {
		periodStart = sentAt.UTC().Format("2006-01-02")
		periodEnd = periodStart
	}
	if currency == "" {
		currency = "LKR"
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO payout_batches_projection (
			period_start, period_end, status, doctor_count, total_cents, currency, created_at, completed_at
		) VALUES ($1::date, $2::date, 'paid', 1, $3, $4, $5, $5)
		ON CONFLICT (period_start, period_end) DO UPDATE SET
			doctor_count = payout_batches_projection.doctor_count + 1,
			total_cents = payout_batches_projection.total_cents + EXCLUDED.total_cents,
			status = 'paid',
			completed_at = EXCLUDED.completed_at,
			currency = COALESCE(NULLIF(payout_batches_projection.currency, ''), EXCLUDED.currency)`,
		periodStart, periodEnd, amountCents, currency, sentAt)
	return err
}

func scanRefund(row rowScanner) (RefundRequest, error) {
	var req RefundRequest
	var appt, dispute, decidedBy *uuid.UUID
	var decidedAt *time.Time
	err := row.Scan(&req.ID, &req.PaymentID, &appt, &dispute, &req.AmountCents, &req.Currency,
		&req.Reason, &req.Status, &req.RequestedAt, &decidedAt, &decidedBy)
	if err != nil {
		return RefundRequest{}, fmt.Errorf("finance: scan refund: %w", err)
	}
	if appt != nil {
		req.AppointmentID = *appt
	}
	if dispute != nil {
		req.DisputeID = *dispute
	}
	req.DecidedAt = decidedAt
	if decidedBy != nil {
		req.DecidedBy = *decidedBy
	}
	return req, nil
}
