package analytics

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
)

type Repository struct {
	pool database.Pool
}

func NewRepository(pool database.Pool) *Repository { return &Repository{pool: pool} }

// --- projection writes (event consumers call these) -----------------------

// UpsertPayment applies one payment.* event to payments_projection.
// Idempotent per the platform contract: a redelivery of the same event_id
// hits the unique constraint and is silently absorbed.
func (r *Repository) UpsertPayment(ctx context.Context, eventID uuid.UUID, status string, p PaymentFact) error {
	const q = `
		INSERT INTO payments_projection
			(payment_id, event_id, appointment_id, doctor_id, patient_id, specialty_code, district,
			 amount_cents, commission_cents, currency, status, provider, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (payment_id) DO UPDATE SET
			status = EXCLUDED.status, commission_cents = EXCLUDED.commission_cents,
			occurred_at = EXCLUDED.occurred_at
		WHERE payments_projection.event_id <> EXCLUDED.event_id
		  AND EXCLUDED.occurred_at >= payments_projection.occurred_at`
	_, err := r.pool.Exec(ctx, q, p.PaymentID, eventID, p.AppointmentID, p.DoctorID, p.PatientID,
		p.SpecialtyCode, p.District, p.AmountCents, p.CommissionCents, p.Currency, status, p.Provider, p.OccurredAt)
	if err != nil {
		return fmt.Errorf("analytics: upsert payment projection: %w", err)
	}
	return nil
}

// UpsertAppointment applies one appointment.* event to
// appointments_projection, last-write-wins by occurred_at so an
// out-of-order redelivery of an older status cannot regress newer state.
//
// scheduled_at is written on INSERT only. Later lifecycle events (confirm,
// cancel, complete) do not carry a new visit time, so ON CONFLICT leaves
// that column alone. appointment.rescheduled is the exception: use
// RescheduleAppointment, which is allowed to move scheduled_at.
func (r *Repository) UpsertAppointment(ctx context.Context, eventID uuid.UUID, status string, a AppointmentFact) error {
	const q = `
		INSERT INTO appointments_projection
			(appointment_id, event_id, doctor_id, patient_id, specialty_code, district, status, scheduled_at, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (appointment_id) DO UPDATE SET
			event_id = EXCLUDED.event_id, status = EXCLUDED.status, occurred_at = EXCLUDED.occurred_at
		WHERE EXCLUDED.occurred_at >= appointments_projection.occurred_at`
	_, err := r.pool.Exec(ctx, q, a.AppointmentID, eventID, a.DoctorID, a.PatientID, a.SpecialtyCode,
		a.District, status, a.ScheduledAt, a.OccurredAt)
	if err != nil {
		return fmt.Errorf("analytics: upsert appointment projection: %w", err)
	}
	return nil
}

// RescheduleAppointment moves a paid booking's projected time. Status stays
// confirmed; scheduled_at becomes the proposed instant.
func (r *Repository) RescheduleAppointment(ctx context.Context, eventID uuid.UUID, a AppointmentFact) error {
	const q = `
		INSERT INTO appointments_projection
			(appointment_id, event_id, doctor_id, patient_id, specialty_code, district, status, scheduled_at, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,'confirmed',$7,$8)
		ON CONFLICT (appointment_id) DO UPDATE SET
			event_id = EXCLUDED.event_id,
			status = 'confirmed',
			scheduled_at = EXCLUDED.scheduled_at,
			occurred_at = EXCLUDED.occurred_at
		WHERE EXCLUDED.occurred_at >= appointments_projection.occurred_at`
	_, err := r.pool.Exec(ctx, q, a.AppointmentID, eventID, a.DoctorID, a.PatientID, a.SpecialtyCode,
		a.District, a.ScheduledAt, a.OccurredAt)
	if err != nil {
		return fmt.Errorf("analytics: reschedule appointment projection: %w", err)
	}
	return nil
}

// AppointmentContext returns the specialty and district already projected for
// an appointment, so a payment event -- which carries neither -- can be filed
// under the same specialty as the booking it paid for.
//
// This is a lookup inside this service's OWN database, not a cross-service
// call: appointments_projection was populated from appointment.created, which
// does carry the specialty. Asking payment-service to denormalise it onto
// every payment event would put the same fact in two places and let them
// drift.
//
// A miss (the payment arrived before its appointment.created) leaves both
// empty; the row is still projected, because a payment missing from the
// revenue total is a worse error than a payment filed under no specialty.
func (r *Repository) AppointmentContext(ctx context.Context, appointmentID uuid.UUID) (specialty, district string, err error) {
	const q = `SELECT specialty_code, district FROM appointments_projection WHERE appointment_id = $1`
	err = r.pool.QueryRow(ctx, q, appointmentID).Scan(&specialty, &district)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("analytics: read appointment context: %w", err)
	}
	return specialty, district, nil
}

// --- materialized view refresh ---------------------------------------------

// RefreshAll refreshes every analytics materialized view.
//
// It calls one SQL function rather than issuing four
// REFRESH MATERIALIZED VIEW CONCURRENTLY statements directly, because REFRESH
// is restricted to the view's OWNER and no GRANT can confer it. The runtime
// role (telemed_admin_app) deliberately owns nothing in this database -- an
// owner can DROP, ALTER and re-GRANT, which is the whole reason the app role
// and the migration role are separate (security review F14). refresh_analytics_views()
// is SECURITY DEFINER, owned by the migration role, with EXECUTE granted only
// to the app role, so the app can refresh these four views and do nothing else
// to them. See migrations/000004_analytics.up.sql.
//
// The refresh is CONCURRENTLY inside that function, which requires the unique
// index each view was created with -- without it the refresh fails outright
// rather than silently taking the exclusive lock CONCURRENTLY exists to avoid,
// which would block dashboard reads for its duration.
func (r *Repository) RefreshAll(ctx context.Context) error {
	if _, err := r.pool.Exec(ctx, "SELECT refresh_analytics_views()"); err != nil {
		return fmt.Errorf("analytics: refresh materialized views: %w", err)
	}
	return nil
}

// --- reads -------------------------------------------------------------

func (r *Repository) Revenue(ctx context.Context, dr DateRange) ([]RevenueDay, error) {
	where, args := dayRangeFilter(dr)
	q := "SELECT day, currency, gross_cents, commission_cents, payment_count FROM revenue_daily" + where + " ORDER BY day"
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("analytics: revenue: %w", err)
	}
	defer rows.Close()
	var out []RevenueDay
	for rows.Next() {
		var v RevenueDay
		if err := rows.Scan(&v.Day, &v.Currency, &v.GrossCents, &v.CommissionCents, &v.PaymentCount); err != nil {
			return nil, fmt.Errorf("analytics: scan revenue: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r *Repository) Bookings(ctx context.Context, dr DateRange) ([]BookingDay, error) {
	where, args := dayRangeFilter(dr)
	q := "SELECT day, specialty_code, status, booking_count FROM bookings_daily" + where + " ORDER BY day"
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("analytics: bookings: %w", err)
	}
	defer rows.Close()
	var out []BookingDay
	for rows.Next() {
		var v BookingDay
		if err := rows.Scan(&v.Day, &v.SpecialtyCode, &v.Status, &v.BookingCount); err != nil {
			return nil, fmt.Errorf("analytics: scan booking: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r *Repository) UtilizationDaily(ctx context.Context, doctorID uuid.UUID, dr DateRange) ([]DoctorUtilizationDay, error) {
	where, args := dayRangeFilter(dr)
	if doctorID != uuid.Nil {
		args = append(args, doctorID)
		if where == "" {
			where = fmt.Sprintf(" WHERE doctor_id = $%d", len(args))
		} else {
			where += fmt.Sprintf(" AND doctor_id = $%d", len(args))
		}
	}
	q := "SELECT day, doctor_id, completed_count, no_show_count, cancelled_count, total_count FROM doctor_utilization_daily" + where + " ORDER BY day"
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("analytics: utilization: %w", err)
	}
	defer rows.Close()
	var out []DoctorUtilizationDay
	for rows.Next() {
		var v DoctorUtilizationDay
		if err := rows.Scan(&v.Day, &v.DoctorID, &v.CompletedCount, &v.NoShowCount, &v.CancelledCount, &v.TotalCount); err != nil {
			return nil, fmt.Errorf("analytics: scan utilization: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r *Repository) TopDoctors(ctx context.Context, dr DateRange, limit int) ([]DoctorTotals, error) {
	where, args := dayRangeFilter(dr)
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	args = append(args, limit)
	q := fmt.Sprintf(`
		SELECT doctor_id, SUM(completed_count), SUM(no_show_count), SUM(cancelled_count), SUM(total_count)
		FROM doctor_utilization_daily%s
		GROUP BY doctor_id
		ORDER BY SUM(total_count) DESC
		LIMIT $%d`, where, len(args))
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("analytics: top doctors: %w", err)
	}
	defer rows.Close()
	var out []DoctorTotals
	for rows.Next() {
		var v DoctorTotals
		if err := rows.Scan(&v.DoctorID, &v.CompletedCount, &v.NoShowCount, &v.CancelledCount, &v.TotalCount); err != nil {
			return nil, fmt.Errorf("analytics: scan top doctors: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r *Repository) Districts(ctx context.Context, dr DateRange) ([]DistrictDay, error) {
	where, args := dayRangeFilter(dr)
	q := "SELECT day, district, booking_count FROM district_activity_daily" + where + " ORDER BY day"
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("analytics: districts: %w", err)
	}
	defer rows.Close()
	var out []DistrictDay
	for rows.Next() {
		var v DistrictDay
		if err := rows.Scan(&v.Day, &v.District, &v.BookingCount); err != nil {
			return nil, fmt.Errorf("analytics: scan district: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DoctorUtilisationSummary rolls doctor_utilization_daily up across the range
// and attaches a display name from doctor_projection when one exists.
func (r *Repository) DoctorUtilisationSummary(ctx context.Context, dr DateRange, limit int) ([]DoctorUtilRow, error) {
	where, args := dayRangeFilterOn("u.day", dr)
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	args = append(args, limit)
	q := fmt.Sprintf(`
		SELECT u.doctor_id, d.full_name,
		       SUM(u.completed_count), SUM(u.no_show_count), SUM(u.cancelled_count), SUM(u.total_count)
		FROM doctor_utilization_daily u
		LEFT JOIN doctor_projection d ON d.doctor_id = u.doctor_id
		%s
		GROUP BY u.doctor_id, d.full_name
		ORDER BY SUM(u.total_count) DESC
		LIMIT $%d`, where, len(args))
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("analytics: doctor utilisation: %w", err)
	}
	defer rows.Close()
	var out []DoctorUtilRow
	for rows.Next() {
		var v DoctorUtilRow
		var name *string
		if err := rows.Scan(&v.DoctorID, &name, &v.CompletedCount, &v.NoShowCount, &v.CancelledCount, &v.TotalCount); err != nil {
			return nil, fmt.Errorf("analytics: scan doctor utilisation: %w", err)
		}
		v.DoctorName = name
		out = append(out, v)
	}
	return out, rows.Err()
}

// DistrictSummary rolls district_activity_daily up across the range.
func (r *Repository) DistrictSummary(ctx context.Context, dr DateRange) ([]DistrictActivity, error) {
	where, args := dayRangeFilter(dr)
	q := "SELECT district, SUM(booking_count) FROM district_activity_daily" + where + " GROUP BY district ORDER BY SUM(booking_count) DESC"
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("analytics: district summary: %w", err)
	}
	defer rows.Close()
	var out []DistrictActivity
	for rows.Next() {
		var v DistrictActivity
		if err := rows.Scan(&v.District, &v.BookingCount); err != nil {
			return nil, fmt.Errorf("analytics: scan district summary: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ActiveUsers counts distinct patients and doctors seen in appointments over
// the range. Projection-only — no cross-service call.
func (r *Repository) ActiveUsers(ctx context.Context, dr DateRange) (int64, error) {
	where1, args1 := dayRangeFilterOn("occurred_at", dr)
	where2, args2 := dayRangeFilterOnShifted("occurred_at", dr, len(args1))
	q := fmt.Sprintf(`SELECT COUNT(*) FROM (
		SELECT patient_id AS id FROM appointments_projection%s AND patient_id IS NOT NULL
		UNION
		SELECT doctor_id AS id FROM appointments_projection%s AND doctor_id IS NOT NULL
	) t`, where1, where2)
	// A fresh slice rather than append(args1, ...): args1 may share a backing
	// array with a caller's slice, and appending into it would scribble on
	// theirs the moment it has spare capacity.
	args := make([]any, 0, len(args1)+len(args2))
	args = append(args, args1...)
	args = append(args, args2...)
	// When the range is unbounded the WHERE is empty and "AND ..." is invalid.
	if where1 == "" {
		q = `SELECT COUNT(*) FROM (
			SELECT patient_id AS id FROM appointments_projection WHERE patient_id IS NOT NULL
			UNION
			SELECT doctor_id AS id FROM appointments_projection WHERE doctor_id IS NOT NULL
		) t`
		args = nil
	}
	var n int64
	if err := r.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("analytics: active users: %w", err)
	}
	return n, nil
}

func dayRangeFilterOn(col string, dr DateRange) (clause string, args []any) {
	var clauses []string
	if !dr.From.IsZero() {
		args = append(args, dr.From)
		clauses = append(clauses, fmt.Sprintf("%s >= $%d", col, len(args)))
	}
	if !dr.To.IsZero() {
		args = append(args, dr.To)
		clauses = append(clauses, fmt.Sprintf("%s <= $%d", col, len(args)))
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

func dayRangeFilterOnShifted(col string, dr DateRange, offset int) (clause string, args []any) {
	var clauses []string
	n := offset
	if !dr.From.IsZero() {
		args = append(args, dr.From)
		n++
		clauses = append(clauses, fmt.Sprintf("%s >= $%d", col, n))
	}
	if !dr.To.IsZero() {
		args = append(args, dr.To)
		n++
		clauses = append(clauses, fmt.Sprintf("%s <= $%d", col, n))
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

// dayRangeFilter builds the WHERE clause bounding an analytics rollup by date.
//
// The column name is a constant rather than a parameter: every rollup table in
// this package keys on "day", and a caller-supplied column name interpolated
// into SQL with Sprintf is a shape worth not having at all, even when every
// call site today passes a literal.
func dayRangeFilter(dr DateRange) (clause string, args []any) {
	const col = "day"
	var clauses []string
	if !dr.From.IsZero() {
		args = append(args, dr.From)
		clauses = append(clauses, fmt.Sprintf("%s >= $%d", col, len(args)))
	}
	if !dr.To.IsZero() {
		args = append(args, dr.To)
		clauses = append(clauses, fmt.Sprintf("%s <= $%d", col, len(args)))
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
