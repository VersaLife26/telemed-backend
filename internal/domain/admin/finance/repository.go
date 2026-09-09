package finance

import (
	"context"
	"fmt"

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
