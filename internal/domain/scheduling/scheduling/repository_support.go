package scheduling

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Doctor schedule settings
// ---------------------------------------------------------------------------

const settingsColumns = `doctor_id, slot_duration_minutes, buffer_minutes, max_per_day,
	timezone, overbooking_percent, advance_days, is_active, version`

func scanSettings(row pgx.Row) (ScheduleSettings, error) {
	var s ScheduleSettings
	err := row.Scan(&s.DoctorID, &s.SlotDurationMinutes, &s.BufferMinutes, &s.MaxPerDay,
		&s.Timezone, &s.OverbookingPercent, &s.AdvanceDays, &s.IsActive, &s.Version)
	return s, err
}

// GetScheduleSettings reads one doctor's generation policy.
func (r *Repository) GetScheduleSettings(ctx context.Context, q Querier, doctorID uuid.UUID) (ScheduleSettings, error) {
	s, err := scanSettings(q.QueryRow(ctx,
		`SELECT `+settingsColumns+` FROM doctor_schedule_settings WHERE doctor_id = $1 AND deleted_at IS NULL`, doctorID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ScheduleSettings{}, ErrDoctorNotConfigured
	}
	if err != nil {
		return ScheduleSettings{}, fmt.Errorf("scheduling: get schedule settings: %w", err)
	}
	return s, nil
}

// UpsertScheduleSettings writes the policy mirrored from doctor-service.
func (r *Repository) UpsertScheduleSettings(ctx context.Context, tx pgx.Tx, s ScheduleSettings) error {
	const q = `
		INSERT INTO doctor_schedule_settings
			(doctor_id, slot_duration_minutes, buffer_minutes, max_per_day, timezone,
			 advance_days, is_active, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 0, NOW(), NOW())
		ON CONFLICT (doctor_id) DO UPDATE SET
			slot_duration_minutes = EXCLUDED.slot_duration_minutes,
			buffer_minutes        = EXCLUDED.buffer_minutes,
			max_per_day           = EXCLUDED.max_per_day,
			timezone              = EXCLUDED.timezone,
			advance_days          = EXCLUDED.advance_days,
			is_active             = EXCLUDED.is_active,
			deleted_at            = NULL,
			version               = doctor_schedule_settings.version + 1,
			updated_at            = NOW()`
	// overbooking_percent is deliberately absent from the update list: it is
	// owned by this service's nightly no-show job, not by doctor-service.
	if _, err := tx.Exec(ctx, q, s.DoctorID, s.SlotDurationMinutes, s.BufferMinutes,
		s.MaxPerDay, s.Timezone, s.AdvanceDays, s.IsActive); err != nil {
		return fmt.Errorf("scheduling: upsert schedule settings: %w", err)
	}
	return nil
}

// SetOverbookingPercent records the nightly overbooking decision.
func (r *Repository) SetOverbookingPercent(ctx context.Context, tx pgx.Tx, doctorID uuid.UUID, percent int) error {
	const q = `
		UPDATE doctor_schedule_settings
		SET overbooking_percent = $2, version = version + 1, updated_at = NOW()
		WHERE doctor_id = $1 AND overbooking_percent <> $2`
	if _, err := tx.Exec(ctx, q, doctorID, percent); err != nil {
		return fmt.Errorf("scheduling: set overbooking percent: %w", err)
	}
	return nil
}

// ListActiveDoctors returns every doctor the generator should run for.
func (r *Repository) ListActiveDoctors(ctx context.Context, q Querier) ([]uuid.UUID, error) {
	const query = `
		SELECT doctor_id FROM doctor_schedule_settings
		WHERE is_active AND deleted_at IS NULL
		ORDER BY doctor_id`
	rows, err := q.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("scheduling: list active doctors: %w", err)
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scheduling: scan doctor id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scheduling: iterate doctor ids: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Working hours (read model, owned by doctor-service)
// ---------------------------------------------------------------------------

// ReplaceWorkingHours swaps a doctor's whole week atomically. Replace rather
// than merge: doctor-service publishes the complete set, so a removed Tuesday
// must actually disappear rather than linger because no event mentioned it.
func (r *Repository) ReplaceWorkingHours(ctx context.Context, tx pgx.Tx, doctorID uuid.UUID, hours []WorkingHour) error {
	if _, err := tx.Exec(ctx, `DELETE FROM working_hours WHERE doctor_id = $1`, doctorID); err != nil {
		return fmt.Errorf("scheduling: clear working hours: %w", err)
	}
	const q = `
		INSERT INTO working_hours (doctor_id, day_of_week, start_time, end_time, is_available)
		VALUES ($1, $2, $3::time, $4::time, $5)
		ON CONFLICT (doctor_id, day_of_week, start_time) DO UPDATE SET
			end_time     = EXCLUDED.end_time,
			is_available = EXCLUDED.is_available,
			version      = working_hours.version + 1,
			updated_at   = NOW()`
	for _, h := range hours {
		// DayOfWeek is a time.Weekday, validated to 0..6 before it reaches
		// here, so the narrowing is safe -- but "safe because of a check
		// somewhere else" is how overflow bugs are written, so it is clamped.
		if _, err := tx.Exec(ctx, q, doctorID, clampInt16(int(h.DayOfWeek)),
			minutesToClock(h.StartMinute), minutesToClock(h.EndMinute), h.IsAvailable); err != nil {
			return fmt.Errorf("scheduling: insert working hour: %w", err)
		}
	}
	return nil
}

// minutesToClock renders minutes-since-midnight as HH:MM:SS for a TIME column.
func minutesToClock(m int) string { return fmt.Sprintf("%02d:%02d:00", m/60, m%60) }

// ListWorkingHours returns a doctor's week, ordered for the generator.
func (r *Repository) ListWorkingHours(ctx context.Context, q Querier, doctorID uuid.UUID) ([]WorkingHour, error) {
	const query = `
		SELECT id, doctor_id, day_of_week,
		       EXTRACT(HOUR FROM start_time)::int * 60 + EXTRACT(MINUTE FROM start_time)::int,
		       EXTRACT(HOUR FROM end_time)::int   * 60 + EXTRACT(MINUTE FROM end_time)::int,
		       is_available
		FROM working_hours
		WHERE doctor_id = $1 AND deleted_at IS NULL
		ORDER BY day_of_week, start_time`
	rows, err := q.Query(ctx, query, doctorID)
	if err != nil {
		return nil, fmt.Errorf("scheduling: list working hours: %w", err)
	}
	defer rows.Close()

	var out []WorkingHour
	for rows.Next() {
		var h WorkingHour
		var dow int16
		if err := rows.Scan(&h.ID, &h.DoctorID, &dow, &h.StartMinute, &h.EndMinute, &h.IsAvailable); err != nil {
			return nil, fmt.Errorf("scheduling: scan working hour: %w", err)
		}
		h.DayOfWeek = time.Weekday(dow)
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scheduling: iterate working hours: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Holidays
// ---------------------------------------------------------------------------

// ListHolidays returns the dates in [from, to] on which doctorID does not work,
// combining platform-wide entries (doctor_id IS NULL) with the doctor's own.
func (r *Repository) ListHolidays(ctx context.Context, q Querier, doctorID uuid.UUID, from, to Date) (map[Date]struct{}, error) {
	const query = `
		SELECT holiday_date FROM holidays
		WHERE (doctor_id IS NULL OR doctor_id = $1)
		  AND holiday_date >= $2 AND holiday_date <= $3
		  AND deleted_at IS NULL`
	rows, err := q.Query(ctx, query, doctorID, from, to)
	if err != nil {
		return nil, fmt.Errorf("scheduling: list holidays: %w", err)
	}
	defer rows.Close()

	out := map[Date]struct{}{}
	for rows.Next() {
		var d Date
		if err := rows.Scan(&d); err != nil {
			return nil, fmt.Errorf("scheduling: scan holiday: %w", err)
		}
		out[d] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scheduling: iterate holidays: %w", err)
	}
	return out, nil
}

const holidayColumns = `id, doctor_id, holiday_date, reason, version, created_at, updated_at`

func scanHoliday(row pgx.Row) (Holiday, error) {
	var h Holiday
	if err := row.Scan(&h.ID, &h.DoctorID, &h.Date, &h.Reason, &h.Version, &h.CreatedAt, &h.UpdatedAt); err != nil {
		return Holiday{}, err
	}
	h.CreatedAt, h.UpdatedAt = utc(h.CreatedAt), utc(h.UpdatedAt)
	return h, nil
}

// UpsertHoliday records a non-working day. doctorID nil means platform-wide.
//
// It returns the stored row, including the id the doctor-facing DELETE needs.
// The ON CONFLICT clears deleted_at, so re-registering leave that was
// previously lifted revives the original row rather than colliding with it --
// holidays_uq is UNIQUE NULLS NOT DISTINCT over (doctor_id, holiday_date) and
// does not exclude soft-deleted rows, so an INSERT would otherwise fail with a
// 23505 on a date the doctor sees as free.
func (r *Repository) UpsertHoliday(ctx context.Context, tx pgx.Tx, doctorID *uuid.UUID, date Date, reason string) (Holiday, error) {
	const q = `
		INSERT INTO holidays (doctor_id, holiday_date, reason)
		VALUES ($1, $2, $3)
		ON CONFLICT (doctor_id, holiday_date) DO UPDATE SET
			reason = EXCLUDED.reason, deleted_at = NULL,
			version = holidays.version + 1, updated_at = NOW()
		RETURNING ` + holidayColumns
	h, err := scanHoliday(tx.QueryRow(ctx, q, doctorID, date, reason))
	if err != nil {
		return Holiday{}, fmt.Errorf("scheduling: upsert holiday: %w", err)
	}
	return h, nil
}

// ListDoctorHolidays returns the leave that applies to one doctor in [from, to]
// -- their own entries and the platform-wide ones together, because both stop
// them being booked.
//
// Ordered platform-wide-last within a date so a UI that renders one row per day
// shows the doctor's own reason in preference to "Poya day".
func (r *Repository) ListDoctorHolidays(ctx context.Context, q Querier, doctorID uuid.UUID, from, to Date) ([]Holiday, error) {
	const query = `
		SELECT ` + holidayColumns + `
		FROM holidays
		WHERE (doctor_id IS NULL OR doctor_id = $1)
		  AND holiday_date >= $2 AND holiday_date <= $3
		  AND deleted_at IS NULL
		ORDER BY holiday_date, (doctor_id IS NULL)`
	rows, err := q.Query(ctx, query, doctorID, from, to)
	if err != nil {
		return nil, fmt.Errorf("scheduling: list doctor holidays: %w", err)
	}
	defer rows.Close()

	out := make([]Holiday, 0)
	for rows.Next() {
		h, err := scanHoliday(rows)
		if err != nil {
			return nil, fmt.Errorf("scheduling: scan doctor holiday: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scheduling: iterate doctor holidays: %w", err)
	}
	return out, nil
}

// LockDoctorHoliday reads one of a doctor's OWN holidays FOR UPDATE.
//
// The doctor_id predicate is the authorization check, not a filter: a
// platform-wide entry has doctor_id IS NULL and can never match, so a doctor
// cannot delete Independence Day, and one doctor cannot delete another's leave.
// Both failures collapse into ErrHolidayNotFound so the endpoint cannot be used
// to discover which holiday ids exist.
func (r *Repository) LockDoctorHoliday(ctx context.Context, tx pgx.Tx, doctorID, holidayID uuid.UUID) (Holiday, error) {
	const q = `
		SELECT ` + holidayColumns + `
		FROM holidays
		WHERE id = $1 AND doctor_id = $2 AND deleted_at IS NULL
		FOR UPDATE`
	h, err := scanHoliday(tx.QueryRow(ctx, q, holidayID, doctorID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Holiday{}, ErrHolidayNotFound
	}
	if err != nil {
		return Holiday{}, fmt.Errorf("scheduling: lock doctor holiday: %w", err)
	}
	return h, nil
}

// SoftDeleteHoliday retires a holiday row. Soft, not hard, because
// holidays_uq is UNIQUE NULLS NOT DISTINCT and a hard delete would lose the
// record of leave that displaced real appointments -- the audit trail for a
// refund a patient may query months later.
func (r *Repository) SoftDeleteHoliday(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	const q = `
		UPDATE holidays
		SET deleted_at = NOW(), version = version + 1, updated_at = NOW()
		WHERE id = $1 AND deleted_at IS NULL`
	tag, err := tx.Exec(ctx, q, id)
	if err != nil {
		return fmt.Errorf("scheduling: soft delete holiday: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrHolidayNotFound
	}
	return nil
}

// LockSlotsInRange takes a row lock on every one of a doctor's slots in
// [from, to), in start_at order.
//
// Ordering matters: two concurrent holiday registrations for overlapping ranges
// that locked in different orders would deadlock. start_at is the partition key
// and the natural order, so it is both cheap and total.
//
// Archived slots are not included; a day 90 days in the past has no bookable
// slots left to withdraw, and holidays cannot be registered in the past anyway.
func (r *Repository) LockSlotsInRange(ctx context.Context, tx pgx.Tx, doctorID uuid.UUID, from, to time.Time) ([]Slot, error) {
	const query = `
		SELECT ` + slotColumns + `
		FROM slots
		WHERE doctor_id = $1 AND start_at >= $2 AND start_at < $3
		ORDER BY start_at
		FOR UPDATE`
	rows, err := tx.Query(ctx, query, doctorID, from, to)
	if err != nil {
		return nil, fmt.Errorf("scheduling: lock slots in range: %w", err)
	}
	defer rows.Close()

	var out []Slot
	for rows.Next() {
		s, err := scanSlot(rows)
		if err != nil {
			return nil, fmt.Errorf("scheduling: scan locked slot: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scheduling: iterate locked slots: %w", err)
	}
	return out, nil
}

// LockLiveAppointmentsInRange takes a row lock on every appointment that still
// holds a slot in [from, to), in id order.
//
// "Live" here means pending_payment or confirmed -- the two statuses a
// cancellation can still act on. A completed or no-show appointment in the past
// is history and is not displaced by leave registered afterwards.
//
// Callers must take this lock BEFORE LockSlotsInRange. That is the order
// CancelAppointment uses, and taking them the other way round in one place is
// all it takes to deadlock against every cancellation on the platform.
func (r *Repository) LockLiveAppointmentsInRange(ctx context.Context, tx pgx.Tx, doctorID uuid.UUID, from, to time.Time) ([]Appointment, error) {
	const query = `
		SELECT ` + appointmentColumns + `
		FROM appointments
		WHERE doctor_id = $1
		  AND slot_start_at >= $2 AND slot_start_at < $3
		  AND status = ANY($4)
		ORDER BY id
		FOR UPDATE`
	live := []string{string(AppointmentPendingPayment), string(AppointmentConfirmed)}
	rows, err := tx.Query(ctx, query, doctorID, from, to, live)
	if err != nil {
		return nil, fmt.Errorf("scheduling: lock live appointments in range: %w", err)
	}
	defer rows.Close()

	var out []Appointment
	for rows.Next() {
		a, err := scanAppointment(rows)
		if err != nil {
			return nil, fmt.Errorf("scheduling: scan locked appointment: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scheduling: iterate locked appointments: %w", err)
	}
	return out, nil
}

// CountLiveAppointmentsInRange counts without locking, for the preflight that
// tells a doctor how many patients registering leave would displace.
//
// The number is advisory: it is read outside any transaction and a booking can
// land a millisecond later. AddHoliday re-checks under lock, which is where the
// guarantee lives.
func (r *Repository) CountLiveAppointmentsInRange(ctx context.Context, q Querier, doctorID uuid.UUID, from, to time.Time) (int, error) {
	const query = `
		SELECT COUNT(*)
		FROM appointments
		WHERE doctor_id = $1
		  AND slot_start_at >= $2 AND slot_start_at < $3
		  AND status = ANY($4)`
	live := []string{string(AppointmentPendingPayment), string(AppointmentConfirmed)}
	var n int
	if err := q.QueryRow(ctx, query, doctorID, from, to, live).Scan(&n); err != nil {
		return 0, fmt.Errorf("scheduling: count live appointments in range: %w", err)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Waitlist
// ---------------------------------------------------------------------------

const waitlistColumns = `id, patient_id, doctor_id, preferred_date, status, notified_at,
	offered_slot_id, offer_expires_at, offer_count, queued_at, version, created_at, updated_at`

func scanWaitlist(row pgx.Row) (WaitlistEntry, error) {
	var w WaitlistEntry
	err := row.Scan(&w.ID, &w.PatientID, &w.DoctorID, &w.PreferredDate, &w.Status, &w.NotifiedAt,
		&w.OfferedSlotID, &w.OfferExpiresAt, &w.OfferCount, &w.QueuedAt, &w.Version, &w.CreatedAt, &w.UpdatedAt)
	if err != nil {
		return WaitlistEntry{}, err
	}
	w.QueuedAt, w.CreatedAt, w.UpdatedAt = utc(w.QueuedAt), utc(w.CreatedAt), utc(w.UpdatedAt)
	w.NotifiedAt, w.OfferExpiresAt = utcPtr(w.NotifiedAt), utcPtr(w.OfferExpiresAt)
	return w, nil
}

// InsertWaitlistEntry queues a patient. A 23505 means uq_waitlists_live caught a
// duplicate join.
func (r *Repository) InsertWaitlistEntry(ctx context.Context, tx pgx.Tx, w *WaitlistEntry) error {
	const q = `
		INSERT INTO waitlists (id, patient_id, doctor_id, preferred_date, status, queued_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'waiting', NOW(), NOW(), NOW())
		RETURNING queued_at, created_at, updated_at`
	if err := tx.QueryRow(ctx, q, w.ID, w.PatientID, w.DoctorID, w.PreferredDate).
		Scan(&w.QueuedAt, &w.CreatedAt, &w.UpdatedAt); err != nil {
		return fmt.Errorf("scheduling: insert waitlist entry: %w", err)
	}
	w.QueuedAt, w.CreatedAt, w.UpdatedAt = utc(w.QueuedAt), utc(w.CreatedAt), utc(w.UpdatedAt)
	w.Status = WaitlistWaiting
	return nil
}

// GetWaitlistEntry reads one entry.
func (r *Repository) GetWaitlistEntry(ctx context.Context, q Querier, id uuid.UUID) (WaitlistEntry, error) {
	w, err := scanWaitlist(q.QueryRow(ctx,
		`SELECT `+waitlistColumns+` FROM waitlists WHERE id = $1 AND deleted_at IS NULL`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return WaitlistEntry{}, ErrWaitlistNotFound
	}
	if err != nil {
		return WaitlistEntry{}, fmt.Errorf("scheduling: get waitlist entry: %w", err)
	}
	return w, nil
}

// ListWaitingEntries returns the queue for one doctor-day in join order. This is
// the authoritative ordering; the Redis sorted set only mirrors it.
func (r *Repository) ListWaitingEntries(ctx context.Context, q Querier, doctorID uuid.UUID, date Date, limit int) ([]WaitlistEntry, error) {
	const query = `
		SELECT ` + waitlistColumns + `
		FROM waitlists
		WHERE doctor_id = $1 AND preferred_date = $2 AND status = 'waiting' AND deleted_at IS NULL
		ORDER BY queued_at, id
		LIMIT $3`
	rows, err := q.Query(ctx, query, doctorID, date, limit)
	if err != nil {
		return nil, fmt.Errorf("scheduling: list waiting entries: %w", err)
	}
	defer rows.Close()

	var out []WaitlistEntry
	for rows.Next() {
		w, err := scanWaitlist(rows)
		if err != nil {
			return nil, fmt.Errorf("scheduling: scan waitlist entry: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scheduling: iterate waitlist entries: %w", err)
	}
	return out, nil
}

// ClaimWaitlistEntry marks an entry notified and records the offer. The version
// guard means two concurrent promotions cannot both claim the same patient.
func (r *Repository) ClaimWaitlistEntry(ctx context.Context, tx pgx.Tx, id uuid.UUID, expectedVersion int, slotID uuid.UUID, expiresAt, now time.Time) (int64, error) {
	const q = `
		UPDATE waitlists
		SET status = 'notified',
		    notified_at = $5,
		    offered_slot_id = $3,
		    offer_expires_at = $4,
		    offer_count = offer_count + 1,
		    version = version + 1,
		    updated_at = NOW()
		WHERE id = $1 AND version = $2 AND status = 'waiting'`
	tag, err := tx.Exec(ctx, q, id, expectedVersion, slotID, expiresAt, now)
	if err != nil {
		return 0, fmt.Errorf("scheduling: claim waitlist entry: %w", err)
	}
	return tag.RowsAffected(), nil
}

// SetWaitlistStatus moves an entry to a terminal or requeued state. Returning
// an entry to 'waiting' resets queued_at, which sends it to the back of the
// queue -- otherwise one patient who never answers their phone would be
// re-offered every freed slot forever while everybody behind them starved.
func (r *Repository) SetWaitlistStatus(ctx context.Context, tx pgx.Tx, id uuid.UUID, status WaitlistStatus, clearOffer bool) (int64, error) {
	q := `
		UPDATE waitlists
		SET status = $2, version = version + 1, updated_at = NOW()`
	if status == WaitlistWaiting {
		q += `, queued_at = NOW()`
	}
	if clearOffer {
		q += `, offered_slot_id = NULL, offer_expires_at = NULL`
	}
	q += ` WHERE id = $1 AND deleted_at IS NULL`
	tag, err := tx.Exec(ctx, q, id, string(status))
	if err != nil {
		return 0, fmt.Errorf("scheduling: set waitlist status: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ListLapsedOffers finds waitlist entries whose five minutes are up.
func (r *Repository) ListLapsedOffers(ctx context.Context, q Querier, now time.Time, limit int) ([]WaitlistEntry, error) {
	const query = `
		SELECT ` + waitlistColumns + `
		FROM waitlists
		WHERE status = 'notified' AND offer_expires_at IS NOT NULL AND offer_expires_at <= $1
		  AND deleted_at IS NULL
		ORDER BY offer_expires_at
		LIMIT $2`
	rows, err := q.Query(ctx, query, now, limit)
	if err != nil {
		return nil, fmt.Errorf("scheduling: list lapsed offers: %w", err)
	}
	defer rows.Close()

	var out []WaitlistEntry
	for rows.Next() {
		w, err := scanWaitlist(rows)
		if err != nil {
			return nil, fmt.Errorf("scheduling: scan lapsed offer: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scheduling: iterate lapsed offers: %w", err)
	}
	return out, nil
}

// GetWaitingEntriesByID resolves ids from the Redis index back to rows, keeping
// only those still waiting and preserving the queue order. The index can name an
// entry that has since been promoted or withdrawn; Postgres arbitrates.
func (r *Repository) GetWaitingEntriesByID(ctx context.Context, q Querier, ids []uuid.UUID) ([]WaitlistEntry, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	const query = `
		SELECT ` + waitlistColumns + `
		FROM waitlists
		WHERE id = ANY($1) AND status = 'waiting' AND deleted_at IS NULL
		ORDER BY queued_at, id`
	rows, err := q.Query(ctx, query, ids)
	if err != nil {
		return nil, fmt.Errorf("scheduling: get waiting entries by id: %w", err)
	}
	defer rows.Close()

	var out []WaitlistEntry
	for rows.Next() {
		w, err := scanWaitlist(rows)
		if err != nil {
			return nil, fmt.Errorf("scheduling: scan waitlist entry: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scheduling: iterate waitlist entries: %w", err)
	}
	return out, nil
}

// ListWaitlistForPatient returns a patient's entries, newest first.
func (r *Repository) ListWaitlistForPatient(ctx context.Context, q Querier, patientID uuid.UUID) ([]WaitlistEntry, error) {
	const query = `
		SELECT ` + waitlistColumns + `
		FROM waitlists
		WHERE patient_id = $1 AND deleted_at IS NULL AND status IN ('waiting', 'notified')
		ORDER BY preferred_date, queued_at`
	rows, err := q.Query(ctx, query, patientID)
	if err != nil {
		return nil, fmt.Errorf("scheduling: list waitlist for patient: %w", err)
	}
	defer rows.Close()

	var out []WaitlistEntry
	for rows.Next() {
		w, err := scanWaitlist(rows)
		if err != nil {
			return nil, fmt.Errorf("scheduling: scan waitlist entry: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scheduling: iterate waitlist entries: %w", err)
	}
	return out, nil
}

// MarkWaitlistBookedBySlot closes the entry whose offer was just taken up.
func (r *Repository) MarkWaitlistBookedBySlot(ctx context.Context, tx pgx.Tx, slotID, patientID uuid.UUID) (int64, error) {
	const q = `
		UPDATE waitlists
		SET status = 'booked', offer_expires_at = NULL, version = version + 1, updated_at = NOW()
		WHERE offered_slot_id = $1 AND patient_id = $2 AND status = 'notified'`
	tag, err := tx.Exec(ctx, q, slotID, patientID)
	if err != nil {
		return 0, fmt.Errorf("scheduling: mark waitlist booked: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ---------------------------------------------------------------------------
// No-show statistics
// ---------------------------------------------------------------------------

// GetNoShowStats reads a patient's counters, returning a zero-value record for a
// patient who has never booked. "No history" and "perfect history" are the same
// thing for the prepayment rule, so there is no error case here.
func (r *Repository) GetNoShowStats(ctx context.Context, q Querier, patientID uuid.UUID) (NoShowStats, error) {
	const query = `
		SELECT patient_id, total_appointments, no_show_count, completed_count, cancelled_count, last_no_show_at
		FROM no_show_stats WHERE patient_id = $1`
	var s NoShowStats
	err := q.QueryRow(ctx, query, patientID).Scan(&s.PatientID, &s.TotalAppointments,
		&s.NoShowCount, &s.CompletedCount, &s.CancelledCount, &s.LastNoShowAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return NoShowStats{PatientID: patientID}, nil
	}
	if err != nil {
		return NoShowStats{}, fmt.Errorf("scheduling: get no-show stats: %w", err)
	}
	s.LastNoShowAt = utcPtr(s.LastNoShowAt)
	return s, nil
}

// noShowCounterColumn maps an outcome to the counter it increments.
var noShowCounterColumn = map[string]string{
	"booked":    "total_appointments",
	"no_show":   "no_show_count",
	"completed": "completed_count",
	"cancelled": "cancelled_count",
}

// BumpNoShowCounter increments one counter for a patient, creating the row on
// first sight. outcome must be a key of noShowCounterColumn.
//
// The insert tuple always carries total_appointments = 1, even when it is not
// the counter being bumped. That is not redundancy: PostgreSQL evaluates CHECK
// constraints against the *proposed* row of an INSERT ... ON CONFLICT DO UPDATE
// before it resolves the conflict, so a proposed (no_show_count = 1,
// total_appointments = 0) row trips no_show_counts_chk even though it would
// never be stored. Seeding the denominator makes the proposed row valid, and it
// is also the right value for the case where it really is an insert: a no-show
// implies an appointment.
func (r *Repository) BumpNoShowCounter(ctx context.Context, tx pgx.Tx, patientID uuid.UUID, outcome string, at time.Time) error {
	column, ok := noShowCounterColumn[outcome]
	if !ok {
		return fmt.Errorf("scheduling: unknown no-show outcome %q", outcome)
	}

	// column comes from a package-level allowlist, never from a request.
	insertCols, insertVals := "patient_id, "+column, "$1, 1"
	if column != "total_appointments" {
		insertCols = "patient_id, total_appointments, " + column
		insertVals = "$1, 1, 1"
	}
	q := fmt.Sprintf(`
		INSERT INTO no_show_stats (%s, last_no_show_at, created_at, updated_at)
		VALUES (%s, $2, NOW(), NOW())
		ON CONFLICT (patient_id) DO UPDATE SET
			%s = no_show_stats.%s + 1,
			last_no_show_at = COALESCE($2, no_show_stats.last_no_show_at),
			version = no_show_stats.version + 1,
			updated_at = NOW()`, insertCols, insertVals, column, column)

	var lastNoShow *time.Time
	if outcome == "no_show" {
		lastNoShow = &at
	}
	if _, err := tx.Exec(ctx, q, patientID, lastNoShow); err != nil {
		return fmt.Errorf("scheduling: bump no-show counter %s: %w", outcome, err)
	}
	return nil
}

// DoctorNoShowRate is one doctor's recent no-show rate.
type DoctorNoShowRate struct {
	DoctorID uuid.UUID
	Total    int
	NoShows  int
	Rate     float64
}

// DoctorNoShowRates aggregates finished appointments since a cutoff. It is the
// nightly job's input for the overbooking decision, and it is deliberately a
// plain aggregate rather than a model: "AI job predicts no-shows" in the source
// documentation is aspiration, and shipping a heuristic honestly beats shipping
// a stub that claims to be a model.
func (r *Repository) DoctorNoShowRates(ctx context.Context, q Querier, since time.Time, minSample int) ([]DoctorNoShowRate, error) {
	const query = `
		SELECT doctor_id,
		       COUNT(*)::int AS total,
		       COUNT(*) FILTER (WHERE status = 'no_show')::int AS no_shows
		FROM appointments
		WHERE slot_start_at >= $1
		  AND status IN ('completed', 'no_show')
		  AND deleted_at IS NULL
		GROUP BY doctor_id
		HAVING COUNT(*) >= $2`
	rows, err := q.Query(ctx, query, since, minSample)
	if err != nil {
		return nil, fmt.Errorf("scheduling: doctor no-show rates: %w", err)
	}
	defer rows.Close()

	var out []DoctorNoShowRate
	for rows.Next() {
		var d DoctorNoShowRate
		if err := rows.Scan(&d.DoctorID, &d.Total, &d.NoShows); err != nil {
			return nil, fmt.Errorf("scheduling: scan doctor no-show rate: %w", err)
		}
		if d.Total > 0 {
			d.Rate = float64(d.NoShows) / float64(d.Total)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scheduling: iterate doctor no-show rates: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Consumer idempotency
// ---------------------------------------------------------------------------

// MarkEventConsumed records that a consumer has handled an envelope, returning
// false when it had already done so. Called inside the same transaction as the
// effect, which is what makes at-least-once delivery safe.
func (r *Repository) MarkEventConsumed(ctx context.Context, tx pgx.Tx, consumer string, eventID uuid.UUID, subject string) (bool, error) {
	const q = `
		INSERT INTO consumed_events (consumer, event_id, subject)
		VALUES ($1, $2, $3)
		ON CONFLICT (consumer, event_id) DO NOTHING`
	tag, err := tx.Exec(ctx, q, consumer, eventID, subject)
	if err != nil {
		return false, fmt.Errorf("scheduling: mark event consumed: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// PruneConsumedEvents drops idempotency records older than the JetStream
// retention window, which is the longest a redelivery can arrive after the fact.
func (r *Repository) PruneConsumedEvents(ctx context.Context, q Querier, before time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `DELETE FROM consumed_events WHERE processed_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("scheduling: prune consumed events: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ---------------------------------------------------------------------------
// Job coordination
// ---------------------------------------------------------------------------

// TryJobLock takes a session-scoped Postgres advisory lock so that only one
// replica runs a given cron job. Advisory locks are the right tool here: they
// cost nothing, they are released automatically if the pod dies, and unlike a
// Redis lock they cannot be lost to a failover while the job is still running.
func (r *Repository) TryJobLock(ctx context.Context, q Querier, key int64) (bool, error) {
	var ok bool
	if err := q.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&ok); err != nil {
		return false, fmt.Errorf("scheduling: try job lock: %w", err)
	}
	return ok, nil
}

// ReleaseJobLock drops the advisory lock.
func (r *Repository) ReleaseJobLock(ctx context.Context, q Querier, key int64) error {
	if _, err := q.Exec(ctx, `SELECT pg_advisory_unlock($1)`, key); err != nil {
		return fmt.Errorf("scheduling: release job lock: %w", err)
	}
	return nil
}

// clampInt16 narrows to a Postgres SMALLINT without wrapping.
func clampInt16(n int) int16 {
	switch {
	case n > math.MaxInt16:
		return math.MaxInt16
	case n < math.MinInt16:
		return math.MinInt16
	default:
		return int16(n)
	}
}
