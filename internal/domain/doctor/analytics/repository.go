package analytics

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Repository is the only place in this package that speaks SQL. Every write
// method takes the caller's transaction, so a fact and the rollups derived from
// it land together or not at all.
type Repository struct{}

// NewRepository returns a stateless repository, same convention as
// internal/availability.
func NewRepository() *Repository { return &Repository{} }

// querier is the read surface: satisfied by both a pool and a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// bucketKey identifies one hour-of-week cell.
type bucketKey struct {
	DayOfWeek int
	HourOfDay int
}

// dateIn buckets an instant into a calendar date in loc, carried as UTC
// midnight because that is what a DATE column round-trips cleanly.
func dateIn(t time.Time, loc *time.Location) time.Time {
	local := t.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
}

// ---------------------------------------------------------------------------
// Fact writes
// ---------------------------------------------------------------------------

// ApplyAppointment records one appointment's terminal outcome and refreshes
// every rollup it feeds.
//
// The upsert is a no-op -- by design, not by accident -- when the incoming
// event is the one already applied (an exact redelivery) or is OLDER than the
// one already applied (a message that lost a race and arrived late). Guarding
// on the producer's event time rather than on arrival order is what makes the
// projection converge to the same numbers however the stream is shuffled.
//
// The CTE reads the row's PREVIOUS date and hour bucket in the same statement,
// before the upsert touches it. That matters for a producer that corrects a
// start time: without it the old day's rollup would keep counting an
// appointment that has moved, and nothing would ever recompute it.
func (r *Repository) ApplyAppointment(ctx context.Context, tx pgx.Tx, f AppointmentFact) (bool, error) {
	const q = `
		WITH prev AS (
			SELECT local_date AS old_date, day_of_week AS old_dow, hour_of_day AS old_hour
			FROM doctor_appointment_fact WHERE appointment_id = $1
		),
		ups AS (
			INSERT INTO doctor_appointment_fact
				(appointment_id, doctor_id, start_at, local_date, day_of_week, hour_of_day,
				 outcome, last_event_id, last_event_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW(), NOW())
			ON CONFLICT (appointment_id) DO UPDATE SET
				doctor_id     = EXCLUDED.doctor_id,
				start_at      = EXCLUDED.start_at,
				local_date    = EXCLUDED.local_date,
				day_of_week   = EXCLUDED.day_of_week,
				hour_of_day   = EXCLUDED.hour_of_day,
				outcome       = EXCLUDED.outcome,
				last_event_id = EXCLUDED.last_event_id,
				last_event_at = EXCLUDED.last_event_at,
				updated_at    = NOW()
			WHERE doctor_appointment_fact.last_event_id IS DISTINCT FROM EXCLUDED.last_event_id
			  AND EXCLUDED.last_event_at >= doctor_appointment_fact.last_event_at
			RETURNING doctor_id, local_date, day_of_week, hour_of_day
		)
		SELECT ups.doctor_id, ups.local_date, ups.day_of_week, ups.hour_of_day,
		       prev.old_date, prev.old_dow, prev.old_hour
		FROM ups LEFT JOIN prev ON TRUE`

	var doctorID uuid.UUID
	var newDate time.Time
	var newDOW, newHour int
	var oldDate *time.Time
	var oldDOW, oldHour *int

	err := tx.QueryRow(ctx, q,
		f.AppointmentID, f.DoctorID, f.StartAt, f.LocalDate, f.DayOfWeek, f.HourOfDay,
		string(f.Outcome), f.LastEventID, f.LastEventAt,
	).Scan(&doctorID, &newDate, &newDOW, &newHour, &oldDate, &oldDOW, &oldHour)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Duplicate or stale out-of-order event: correctly ignored.
			return false, nil
		}
		return false, fmt.Errorf("analytics: apply appointment fact: %w", err)
	}

	dates := []time.Time{newDate}
	if oldDate != nil && !oldDate.Equal(newDate) {
		dates = append(dates, *oldDate)
	}
	for _, d := range dates {
		if err := r.recomputeAppointmentDaily(ctx, tx, doctorID, d); err != nil {
			return false, err
		}
	}

	buckets := []bucketKey{{newDOW, newHour}}
	if oldDOW != nil && oldHour != nil && (*oldDOW != newDOW || *oldHour != newHour) {
		buckets = append(buckets, bucketKey{*oldDOW, *oldHour})
	}
	for _, b := range buckets {
		if err := r.recomputePeakHour(ctx, tx, doctorID, b); err != nil {
			return false, err
		}
	}
	return true, nil
}

// ApplyConsultation records one ended call and refreshes the day's duration
// rollup.
func (r *Repository) ApplyConsultation(ctx context.Context, tx pgx.Tx, f ConsultationFact) (bool, error) {
	const q = `
		WITH prev AS (
			SELECT local_date AS old_date FROM doctor_consultation_fact WHERE consultation_id = $1
		),
		ups AS (
			INSERT INTO doctor_consultation_fact
				(consultation_id, doctor_id, appointment_id, ended_at, local_date,
				 duration_seconds, end_reason, last_event_id, last_event_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW(), NOW())
			ON CONFLICT (consultation_id) DO UPDATE SET
				doctor_id        = EXCLUDED.doctor_id,
				appointment_id   = EXCLUDED.appointment_id,
				ended_at         = EXCLUDED.ended_at,
				local_date       = EXCLUDED.local_date,
				duration_seconds = EXCLUDED.duration_seconds,
				end_reason       = EXCLUDED.end_reason,
				last_event_id    = EXCLUDED.last_event_id,
				last_event_at    = EXCLUDED.last_event_at,
				updated_at       = NOW()
			WHERE doctor_consultation_fact.last_event_id IS DISTINCT FROM EXCLUDED.last_event_id
			  AND EXCLUDED.last_event_at >= doctor_consultation_fact.last_event_at
			RETURNING doctor_id, local_date
		)
		SELECT ups.doctor_id, ups.local_date, prev.old_date FROM ups LEFT JOIN prev ON TRUE`

	var doctorID uuid.UUID
	var newDate time.Time
	var oldDate *time.Time

	err := tx.QueryRow(ctx, q,
		f.ConsultationID, f.DoctorID, f.AppointmentID, f.EndedAt, f.LocalDate,
		f.DurationSeconds, f.EndReason, f.LastEventID, f.LastEventAt,
	).Scan(&doctorID, &newDate, &oldDate)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("analytics: apply consultation fact: %w", err)
	}

	if err := r.recomputeConsultationDaily(ctx, tx, doctorID, newDate); err != nil {
		return false, err
	}
	if oldDate != nil && !oldDate.Equal(newDate) {
		if err := r.recomputeConsultationDaily(ctx, tx, doctorID, *oldDate); err != nil {
			return false, err
		}
	}
	return true, nil
}

// ApplyPayment records one settled payment and refreshes the day's money
// rollup.
func (r *Repository) ApplyPayment(ctx context.Context, tx pgx.Tx, f PaymentFact) (bool, error) {
	const q = `
		WITH prev AS (
			SELECT local_date AS old_date FROM doctor_payment_fact WHERE payment_id = $1
		),
		ups AS (
			INSERT INTO doctor_payment_fact
				(payment_id, doctor_id, appointment_id, succeeded_at, local_date,
				 gross_cents, commission_cents, net_cents, currency,
				 last_event_id, last_event_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NOW(), NOW())
			ON CONFLICT (payment_id) DO UPDATE SET
				doctor_id        = EXCLUDED.doctor_id,
				appointment_id   = EXCLUDED.appointment_id,
				succeeded_at     = EXCLUDED.succeeded_at,
				local_date       = EXCLUDED.local_date,
				gross_cents      = EXCLUDED.gross_cents,
				commission_cents = EXCLUDED.commission_cents,
				net_cents        = EXCLUDED.net_cents,
				currency         = EXCLUDED.currency,
				last_event_id    = EXCLUDED.last_event_id,
				last_event_at    = EXCLUDED.last_event_at,
				updated_at       = NOW()
			WHERE doctor_payment_fact.last_event_id IS DISTINCT FROM EXCLUDED.last_event_id
			  AND EXCLUDED.last_event_at >= doctor_payment_fact.last_event_at
			RETURNING doctor_id, local_date
		)
		SELECT ups.doctor_id, ups.local_date, prev.old_date FROM ups LEFT JOIN prev ON TRUE`

	var doctorID uuid.UUID
	var newDate time.Time
	var oldDate *time.Time

	err := tx.QueryRow(ctx, q,
		f.PaymentID, f.DoctorID, f.AppointmentID, f.SucceededAt, f.LocalDate,
		f.GrossCents, f.CommissionCents, f.NetCents, f.Currency,
		f.LastEventID, f.LastEventAt,
	).Scan(&doctorID, &newDate, &oldDate)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("analytics: apply payment fact: %w", err)
	}

	if err := r.recomputePaymentDaily(ctx, tx, doctorID, newDate); err != nil {
		return false, err
	}
	if oldDate != nil && !oldDate.Equal(newDate) {
		if err := r.recomputePaymentDaily(ctx, tx, doctorID, *oldDate); err != nil {
			return false, err
		}
	}
	return true, nil
}

// ApplyPayout records one settled payout. It feeds no daily rollup: a payout
// covers a PERIOD, and attributing it to a single day would make the day it was
// sent look like a day of enormous earnings.
func (r *Repository) ApplyPayout(ctx context.Context, tx pgx.Tx, f PayoutFact) (bool, error) {
	const q = `
		INSERT INTO doctor_payout_fact
			(payout_id, doctor_id, amount_cents, currency, period_start, period_end,
			 transfer_id, sent_at, last_event_id, last_event_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW(), NOW())
		ON CONFLICT (payout_id) DO UPDATE SET
			doctor_id     = EXCLUDED.doctor_id,
			amount_cents  = EXCLUDED.amount_cents,
			currency      = EXCLUDED.currency,
			period_start  = EXCLUDED.period_start,
			period_end    = EXCLUDED.period_end,
			transfer_id   = EXCLUDED.transfer_id,
			sent_at       = EXCLUDED.sent_at,
			last_event_id = EXCLUDED.last_event_id,
			last_event_at = EXCLUDED.last_event_at,
			updated_at    = NOW()
		WHERE doctor_payout_fact.last_event_id IS DISTINCT FROM EXCLUDED.last_event_id
		  AND EXCLUDED.last_event_at >= doctor_payout_fact.last_event_at
		RETURNING payout_id`

	var id uuid.UUID
	err := tx.QueryRow(ctx, q,
		f.PayoutID, f.DoctorID, f.AmountCents, f.Currency, f.PeriodStart, f.PeriodEnd,
		f.TransferID, f.SentAt, f.LastEventID, f.LastEventAt,
	).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("analytics: apply payout fact: %w", err)
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// Rollup recomputes
// ---------------------------------------------------------------------------
//
// Each of these rebuilds ONE (doctor, date) row's slice of the rollup from the
// facts. They touch disjoint column sets, so the money recompute cannot clobber
// the session counts and vice versa -- the ON CONFLICT clause names only the
// columns that recompute owns, and every other column keeps its stored value.

func (r *Repository) recomputeAppointmentDaily(ctx context.Context, tx pgx.Tx, doctorID uuid.UUID, date time.Time) error {
	const q = `
		INSERT INTO doctor_analytics_daily
			(doctor_id, date, completed_count, no_show_count, cancelled_count, updated_at)
		SELECT doctor_id, local_date,
		       COUNT(*) FILTER (WHERE outcome = 'completed'),
		       COUNT(*) FILTER (WHERE outcome = 'no_show'),
		       COUNT(*) FILTER (WHERE outcome = 'cancelled'),
		       NOW()
		FROM doctor_appointment_fact
		WHERE doctor_id = $1 AND local_date = $2
		GROUP BY doctor_id, local_date
		ON CONFLICT (doctor_id, date) DO UPDATE SET
			completed_count = EXCLUDED.completed_count,
			no_show_count   = EXCLUDED.no_show_count,
			cancelled_count = EXCLUDED.cancelled_count,
			updated_at      = NOW()`
	if _, err := tx.Exec(ctx, q, doctorID, date); err != nil {
		return fmt.Errorf("analytics: recompute appointment daily: %w", err)
	}
	return nil
}

func (r *Repository) recomputeConsultationDaily(ctx context.Context, tx pgx.Tx, doctorID uuid.UUID, date time.Time) error {
	// Only end_reason = 'completed' contributes. A four-second failed
	// connection is a real ConsultationEnded and averaging it in would drag
	// every doctor's reported consultation length down -- a number they are
	// shown as a quality metric and would have no way to explain.
	const q = `
		INSERT INTO doctor_analytics_daily
			(doctor_id, date, consultation_count, consultation_seconds, updated_at)
		SELECT doctor_id, local_date,
		       COUNT(*) FILTER (WHERE end_reason = 'completed'),
		       COALESCE(SUM(duration_seconds) FILTER (WHERE end_reason = 'completed'), 0),
		       NOW()
		FROM doctor_consultation_fact
		WHERE doctor_id = $1 AND local_date = $2
		GROUP BY doctor_id, local_date
		ON CONFLICT (doctor_id, date) DO UPDATE SET
			consultation_count   = EXCLUDED.consultation_count,
			consultation_seconds = EXCLUDED.consultation_seconds,
			updated_at           = NOW()`
	if _, err := tx.Exec(ctx, q, doctorID, date); err != nil {
		return fmt.Errorf("analytics: recompute consultation daily: %w", err)
	}
	return nil
}

func (r *Repository) recomputePaymentDaily(ctx context.Context, tx pgx.Tx, doctorID uuid.UUID, date time.Time) error {
	// currency is only set when the day is unambiguous. A day that somehow
	// settled in two currencies leaves it empty rather than picking one, so a
	// client rendering "LKR 12,300" cannot be silently shown a total that is
	// part rupees and part something else. The earnings endpoint does not rely
	// on this column -- it groups the fact table by currency, which is exact.
	const q = `
		INSERT INTO doctor_analytics_daily
			(doctor_id, date, gross_cents, commission_cents, net_cents, currency, payment_count, updated_at)
		SELECT doctor_id, local_date,
		       COALESCE(SUM(gross_cents), 0),
		       COALESCE(SUM(commission_cents), 0),
		       COALESCE(SUM(net_cents), 0),
		       CASE WHEN COUNT(DISTINCT currency) = 1 THEN MIN(currency) ELSE '' END,
		       COUNT(*),
		       NOW()
		FROM doctor_payment_fact
		WHERE doctor_id = $1 AND local_date = $2
		GROUP BY doctor_id, local_date
		ON CONFLICT (doctor_id, date) DO UPDATE SET
			gross_cents      = EXCLUDED.gross_cents,
			commission_cents = EXCLUDED.commission_cents,
			net_cents        = EXCLUDED.net_cents,
			currency         = EXCLUDED.currency,
			payment_count    = EXCLUDED.payment_count,
			updated_at       = NOW()`
	if _, err := tx.Exec(ctx, q, doctorID, date); err != nil {
		return fmt.Errorf("analytics: recompute payment daily: %w", err)
	}
	return nil
}

func (r *Repository) recomputePeakHour(ctx context.Context, tx pgx.Tx, doctorID uuid.UUID, b bucketKey) error {
	const q = `
		INSERT INTO doctor_peak_hours
			(doctor_id, day_of_week, hour_of_day, booking_count,
			 completed_count, no_show_count, cancelled_count, updated_at)
		SELECT doctor_id, day_of_week, hour_of_day,
		       COUNT(*),
		       COUNT(*) FILTER (WHERE outcome = 'completed'),
		       COUNT(*) FILTER (WHERE outcome = 'no_show'),
		       COUNT(*) FILTER (WHERE outcome = 'cancelled'),
		       NOW()
		FROM doctor_appointment_fact
		WHERE doctor_id = $1 AND day_of_week = $2 AND hour_of_day = $3
		GROUP BY doctor_id, day_of_week, hour_of_day
		ON CONFLICT (doctor_id, day_of_week, hour_of_day) DO UPDATE SET
			booking_count   = EXCLUDED.booking_count,
			completed_count = EXCLUDED.completed_count,
			no_show_count   = EXCLUDED.no_show_count,
			cancelled_count = EXCLUDED.cancelled_count,
			updated_at      = NOW()`
	if _, err := tx.Exec(ctx, q, doctorID, b.DayOfWeek, b.HourOfDay); err != nil {
		return fmt.Errorf("analytics: recompute peak hour: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// ListDaily returns the rollup rows for [from, to] inclusive, oldest first.
// Days with no activity have no row and are simply absent -- the client charts
// a sparse series, and materialising empty days would mean writing rows for
// every doctor for every day they did not work.
func (r *Repository) ListDaily(ctx context.Context, q querier, doctorID uuid.UUID, from, to time.Time) ([]DailyRow, error) {
	const query = `
		SELECT date, completed_count, no_show_count, cancelled_count,
		       consultation_count, consultation_seconds,
		       gross_cents, commission_cents, net_cents, currency, payment_count
		FROM doctor_analytics_daily
		WHERE doctor_id = $1 AND date >= $2 AND date <= $3
		ORDER BY date`
	rows, err := q.Query(ctx, query, doctorID, from, to)
	if err != nil {
		return nil, fmt.Errorf("analytics: list daily: %w", err)
	}
	defer rows.Close()

	out := make([]DailyRow, 0)
	for rows.Next() {
		var d DailyRow
		if err := rows.Scan(&d.Date, &d.CompletedCount, &d.NoShowCount, &d.CancelledCount,
			&d.ConsultationCount, &d.ConsultationSeconds,
			&d.GrossCents, &d.CommissionCents, &d.NetCents, &d.Currency, &d.PaymentCount); err != nil {
			return nil, fmt.Errorf("analytics: scan daily: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("analytics: iterate daily: %w", err)
	}
	return out, nil
}

// ListPeakHours returns every non-empty hour-of-week cell for a doctor.
func (r *Repository) ListPeakHours(ctx context.Context, q querier, doctorID uuid.UUID) ([]HourBucket, error) {
	const query = `
		SELECT day_of_week, hour_of_day, booking_count, completed_count, no_show_count, cancelled_count
		FROM doctor_peak_hours
		WHERE doctor_id = $1 AND booking_count > 0
		ORDER BY day_of_week, hour_of_day`
	rows, err := q.Query(ctx, query, doctorID)
	if err != nil {
		return nil, fmt.Errorf("analytics: list peak hours: %w", err)
	}
	defer rows.Close()

	out := make([]HourBucket, 0)
	for rows.Next() {
		var b HourBucket
		if err := rows.Scan(&b.DayOfWeek, &b.HourOfDay, &b.BookingCount,
			&b.CompletedCount, &b.NoShowCount, &b.CancelledCount); err != nil {
			return nil, fmt.Errorf("analytics: scan peak hour: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("analytics: iterate peak hours: %w", err)
	}
	return out, nil
}

// ReviewStats reads the rating a doctor earned inside a window, and the
// lifetime aggregate the marketplace shows.
//
// reviews lives in THIS database, so this is a local query and not a
// cross-service join: doctor-service owns reviews, and the rating on screen 9
// is the same number patients see on the doctor's profile.
//
// The window is compared against created_at, which is a TIMESTAMPTZ, so the
// bounds arrive as instants already resolved from the caller's civil dates.
func (r *Repository) ReviewStats(ctx context.Context, q querier, doctorID uuid.UUID, from, to time.Time) (windowAvg float64, windowCount int, lifetimeAvg float64, lifetimeCount int, err error) {
	const query = `
		SELECT
			COALESCE(AVG(rating) FILTER (WHERE created_at >= $2 AND created_at < $3), 0)::float8,
			COUNT(*) FILTER (WHERE created_at >= $2 AND created_at < $3),
			COALESCE(AVG(rating), 0)::float8,
			COUNT(*)
		FROM reviews
		WHERE doctor_id = $1 AND is_published = TRUE AND deleted_at IS NULL`
	if err := q.QueryRow(ctx, query, doctorID, from, to).
		Scan(&windowAvg, &windowCount, &lifetimeAvg, &lifetimeCount); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("analytics: review stats: %w", err)
	}
	return windowAvg, windowCount, lifetimeAvg, lifetimeCount, nil
}

// EarningsByCurrency totals a window from the payment facts, grouped by
// currency.
//
// It reads the FACTS rather than the daily rollup on purpose. The rollup sums
// cents per day with a single currency column, which is exact for the one
// currency this platform uses and would silently add rupees to dollars the day
// it is not. Grouping the facts is exact in every case and costs one index
// range scan.
//
// paid_cents is the part of net_cents that a settled payout actually covers,
// matched by DATE: a payout for 1-15 August covers every payment fact dated in
// that range. Matching by date rather than by summing payout amounts is what
// makes the number answer the question a doctor is asking -- "of what I earned
// in this window, how much has reached me" -- rather than "how much was
// transferred during it", which is a different figure whenever a payout period
// straddles the window edge.
func (r *Repository) EarningsByCurrency(ctx context.Context, q querier, doctorID uuid.UUID, from, to time.Time) ([]CurrencyTotals, error) {
	const query = `
		SELECT p.currency,
		       COALESCE(SUM(p.gross_cents), 0),
		       COALESCE(SUM(p.commission_cents), 0),
		       COALESCE(SUM(p.net_cents), 0),
		       COUNT(*),
		       COALESCE(SUM(p.net_cents) FILTER (WHERE EXISTS (
		           SELECT 1 FROM doctor_payout_fact po
		           WHERE po.doctor_id = p.doctor_id
		             AND po.currency = p.currency
		             AND p.local_date >= po.period_start
		             AND p.local_date <= po.period_end
		       )), 0)
		FROM doctor_payment_fact p
		WHERE p.doctor_id = $1 AND p.local_date >= $2 AND p.local_date <= $3
		GROUP BY p.currency
		ORDER BY SUM(p.gross_cents) DESC, p.currency`
	rows, err := q.Query(ctx, query, doctorID, from, to)
	if err != nil {
		return nil, fmt.Errorf("analytics: earnings by currency: %w", err)
	}
	defer rows.Close()

	out := make([]CurrencyTotals, 0)
	for rows.Next() {
		var c CurrencyTotals
		if err := rows.Scan(&c.Currency, &c.GrossCents, &c.CommissionCents,
			&c.NetCents, &c.PaymentCount, &c.PaidCents); err != nil {
			return nil, fmt.Errorf("analytics: scan earnings: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("analytics: iterate earnings: %w", err)
	}
	return out, nil
}

// ListPayouts returns the settlements whose period overlaps [from, to], newest
// first. Overlap, not containment: a payout for 1-15 August is exactly what a
// doctor asking about the first week of August wants to see.
func (r *Repository) ListPayouts(ctx context.Context, q querier, doctorID uuid.UUID, from, to time.Time) ([]PayoutFact, error) {
	const query = `
		SELECT payout_id, doctor_id, amount_cents, currency, period_start, period_end,
		       transfer_id, sent_at
		FROM doctor_payout_fact
		WHERE doctor_id = $1 AND period_start <= $3 AND period_end >= $2
		ORDER BY period_end DESC, sent_at DESC`
	rows, err := q.Query(ctx, query, doctorID, from, to)
	if err != nil {
		return nil, fmt.Errorf("analytics: list payouts: %w", err)
	}
	defer rows.Close()

	out := make([]PayoutFact, 0)
	for rows.Next() {
		var p PayoutFact
		if err := rows.Scan(&p.PayoutID, &p.DoctorID, &p.AmountCents, &p.Currency,
			&p.PeriodStart, &p.PeriodEnd, &p.TransferID, &p.SentAt); err != nil {
			return nil, fmt.Errorf("analytics: scan payout: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("analytics: iterate payouts: %w", err)
	}
	return out, nil
}

// AppointmentFactFor reads one appointment's projected state. Exported for
// tests; production code reads only the rollups.
func (r *Repository) AppointmentFactFor(ctx context.Context, q querier, appointmentID uuid.UUID) (AppointmentFact, bool, error) {
	const query = `
		SELECT appointment_id, doctor_id, start_at, local_date, day_of_week, hour_of_day,
		       outcome, last_event_id, last_event_at
		FROM doctor_appointment_fact WHERE appointment_id = $1`
	var f AppointmentFact
	var outcome string
	err := q.QueryRow(ctx, query, appointmentID).Scan(&f.AppointmentID, &f.DoctorID, &f.StartAt,
		&f.LocalDate, &f.DayOfWeek, &f.HourOfDay, &outcome, &f.LastEventID, &f.LastEventAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AppointmentFact{}, false, nil
		}
		return AppointmentFact{}, false, fmt.Errorf("analytics: appointment fact: %w", err)
	}
	f.Outcome = Outcome(outcome)
	return f, true, nil
}
