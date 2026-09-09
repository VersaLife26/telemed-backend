package scheduling

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the read/write surface shared by *pgxpool.Pool and pgx.Tx. Every
// repository method takes one, so the caller decides whether the statement joins
// an existing transaction. Methods that MUST be transactional take pgx.Tx
// instead -- that is a compile-time statement about the correctness argument,
// not a style preference.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Repository is the only place in this service that speaks SQL. It never
// returns an httpx error and never makes a policy decision; it moves rows.
type Repository struct{}

// NewRepository constructs the repository. It is stateless -- the connection
// arrives with every call -- which is what lets the same instance serve pool
// queries and transactional ones without any coupling.
func NewRepository() *Repository { return &Repository{} }

const slotColumns = `id, doctor_id, start_at, end_at, status, appointment_id,
	reserved_for, reserved_until, version, created_at, updated_at`

// utc normalises an instant to UTC.
//
// pgx decodes TIMESTAMPTZ through time.Unix, which yields a time.Time in
// time.Local -- the same instant, but carrying whatever zone the container
// happens to have. Left alone, that zone leaks into every JSON event payload,
// so an event emitted from a Colombo laptop and one emitted from a UTC pod look
// different on the wire for no reason. Normalising once, here, means everything
// downstream is UTC by construction.
func utc(t time.Time) time.Time { return t.UTC() }

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.UTC()
	return &v
}

func scanSlot(row pgx.Row) (Slot, error) {
	var s Slot
	err := row.Scan(&s.ID, &s.DoctorID, &s.StartAt, &s.EndAt, &s.Status, &s.AppointmentID,
		&s.ReservedFor, &s.ReservedUntil, &s.Version, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return Slot{}, err
	}
	s.StartAt, s.EndAt = utc(s.StartAt), utc(s.EndAt)
	s.CreatedAt, s.UpdatedAt = utc(s.CreatedAt), utc(s.UpdatedAt)
	s.ReservedUntil = utcPtr(s.ReservedUntil)
	return s, nil
}

// ---------------------------------------------------------------------------
// Slots
// ---------------------------------------------------------------------------

// LockSlot takes a row-level exclusive lock on a slot and returns its current
// state. This is the first half of the booking correctness argument: every
// concurrent booker for the same slot queues here, and READ COMMITTED
// re-evaluates the row after the lock is granted, so the loser observes the
// winner's committed BOOKED status rather than the stale AVAILABLE it started
// from.
//
// It takes a pgx.Tx and not a Querier on purpose. FOR UPDATE outside a
// transaction releases the lock the instant the statement finishes, which would
// be a lock that protects nothing.
func (r *Repository) LockSlot(ctx context.Context, tx pgx.Tx, slotID uuid.UUID) (Slot, error) {
	// No start_at predicate is available here -- the caller has only the id the
	// client sent -- so this scans one small b-tree per partition. See
	// docs/DESIGN.md §2.3 for why that is the right trade.
	q := `SELECT ` + slotColumns + ` FROM slots WHERE id = $1 FOR UPDATE`
	s, err := scanSlot(tx.QueryRow(ctx, q, slotID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Slot{}, ErrSlotNotFound
	}
	if err != nil {
		return Slot{}, fmt.Errorf("scheduling: lock slot: %w", err)
	}
	return s, nil
}

// GetSlot reads a slot without locking it.
func (r *Repository) GetSlot(ctx context.Context, q Querier, slotID uuid.UUID) (Slot, error) {
	s, err := scanSlot(q.QueryRow(ctx, `SELECT `+slotColumns+` FROM slots WHERE id = $1`, slotID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Slot{}, ErrSlotNotFound
	}
	if err != nil {
		return Slot{}, fmt.Errorf("scheduling: get slot: %w", err)
	}
	return s, nil
}

// ListAvailableSlots returns the bookable slots for a doctor in [from, to).
// It is the query idx_slots_available exists for, and the partition key is in
// the predicate so the planner prunes to the months the range touches.
func (r *Repository) ListAvailableSlots(ctx context.Context, q Querier, doctorID uuid.UUID, from, to time.Time) ([]Slot, error) {
	const query = `
		SELECT ` + slotColumns + `
		FROM slots
		WHERE doctor_id = $1
		  AND start_at >= $2
		  AND start_at < $3
		  AND status = 'AVAILABLE'
		  AND (reserved_until IS NULL OR reserved_until <= NOW())
		ORDER BY start_at`
	rows, err := q.Query(ctx, query, doctorID, from, to)
	if err != nil {
		return nil, fmt.Errorf("scheduling: list available slots: %w", err)
	}
	defer rows.Close()

	var out []Slot
	for rows.Next() {
		s, err := scanSlot(rows)
		if err != nil {
			return nil, fmt.Errorf("scheduling: scan slot: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scheduling: iterate slots: %w", err)
	}
	return out, nil
}

// MarkSlotBooked is the version-guarded write. It returns the number of rows it
// changed; anything other than 1 means another transaction moved the row
// underneath us and the caller must abort.
//
// start_at is in the predicate purely so the planner prunes to one partition:
// the caller already read it under FOR UPDATE, so it cannot have changed.
func (r *Repository) MarkSlotBooked(ctx context.Context, tx pgx.Tx, slotID uuid.UUID, startAt time.Time, appointmentID uuid.UUID, expectedVersion int) (int64, error) {
	const q = `
		UPDATE slots
		SET status = 'BOOKED',
		    appointment_id = $4,
		    reserved_for = NULL,
		    reserved_until = NULL,
		    version = version + 1,
		    updated_at = NOW()
		WHERE id = $1 AND start_at = $2 AND version = $3 AND status IN ('AVAILABLE', 'BLOCKED')`
	tag, err := tx.Exec(ctx, q, slotID, startAt, expectedVersion, appointmentID)
	if err != nil {
		return 0, fmt.Errorf("scheduling: mark slot booked: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ReleaseSlot returns a slot to the market: cancellation, payment failure, or an
// unpaid booking that timed out.
func (r *Repository) ReleaseSlot(ctx context.Context, tx pgx.Tx, slotID uuid.UUID, startAt time.Time, expectedVersion int) (int64, error) {
	const q = `
		UPDATE slots
		SET status = 'AVAILABLE',
		    appointment_id = NULL,
		    reserved_for = NULL,
		    reserved_until = NULL,
		    version = version + 1,
		    updated_at = NOW()
		WHERE id = $1 AND start_at = $2 AND version = $3`
	tag, err := tx.Exec(ctx, q, slotID, startAt, expectedVersion)
	if err != nil {
		return 0, fmt.Errorf("scheduling: release slot: %w", err)
	}
	return tag.RowsAffected(), nil
}

// SetSlotStatus is the administrative override behind POST /admin/slots/{id}/block.
func (r *Repository) SetSlotStatus(ctx context.Context, tx pgx.Tx, slotID uuid.UUID, startAt time.Time, status SlotStatus, expectedVersion int) (int64, error) {
	const q = `
		UPDATE slots
		SET status = $4,
		    appointment_id = NULL,
		    reserved_for = NULL,
		    reserved_until = NULL,
		    version = version + 1,
		    updated_at = NOW()
		WHERE id = $1 AND start_at = $2 AND version = $3`
	tag, err := tx.Exec(ctx, q, slotID, startAt, expectedVersion, string(status))
	if err != nil {
		return 0, fmt.Errorf("scheduling: set slot status: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ReserveSlot parks a slot for one waitlisted patient. The slot becomes BLOCKED
// so it disappears from availability searches, and reserved_until bounds the
// hold in the database rather than in a Redis TTL that a failover can drop.
func (r *Repository) ReserveSlot(ctx context.Context, tx pgx.Tx, slotID uuid.UUID, startAt time.Time, patientID uuid.UUID, until time.Time, expectedVersion int) (int64, error) {
	const q = `
		UPDATE slots
		SET status = 'BLOCKED',
		    reserved_for = $4,
		    reserved_until = $5,
		    version = version + 1,
		    updated_at = NOW()
		WHERE id = $1 AND start_at = $2 AND version = $3 AND status = 'AVAILABLE'`
	tag, err := tx.Exec(ctx, q, slotID, startAt, expectedVersion, patientID, until)
	if err != nil {
		return 0, fmt.Errorf("scheduling: reserve slot: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ExpiredReservation is a lapsed waitlist hold that the sweeper must undo.
type ExpiredReservation struct {
	SlotID    uuid.UUID
	StartAt   time.Time
	DoctorID  uuid.UUID
	PatientID uuid.UUID
	Version   int
}

// ListExpiredReservations finds slots whose five minutes are up.
func (r *Repository) ListExpiredReservations(ctx context.Context, q Querier, now time.Time, limit int) ([]ExpiredReservation, error) {
	const query = `
		SELECT id, start_at, doctor_id, reserved_for, version
		FROM slots
		WHERE status = 'BLOCKED'
		  AND reserved_until IS NOT NULL
		  AND reserved_until <= $1
		ORDER BY reserved_until
		LIMIT $2`
	rows, err := q.Query(ctx, query, now, limit)
	if err != nil {
		return nil, fmt.Errorf("scheduling: list expired reservations: %w", err)
	}
	defer rows.Close()

	var out []ExpiredReservation
	for rows.Next() {
		var e ExpiredReservation
		if err := rows.Scan(&e.SlotID, &e.StartAt, &e.DoctorID, &e.PatientID, &e.Version); err != nil {
			return nil, fmt.Errorf("scheduling: scan expired reservation: %w", err)
		}
		e.StartAt = utc(e.StartAt)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scheduling: iterate expired reservations: %w", err)
	}
	return out, nil
}

// CopySlots bulk-inserts generated slots.
//
// pgx.CopyFrom is the fastest path into Postgres by an order of magnitude, and
// it is also the only one that cannot express ON CONFLICT -- COPY has no
// conflict machinery at the protocol level, so the documentation's
// "bulk insert via pgx.CopyFrom with ON CONFLICT (doctor_id, start_at) DO
// NOTHING" is not a thing that can be written.
//
// The resolution is two statements inside one transaction: COPY into an
// unlogged session-scoped staging table, then a set-based INSERT ... SELECT with
// the conflict clause. The COPY keeps the wire-format speed; the INSERT keeps
// the idempotency that lets the generator be re-run any number of times.
//
// DISTINCT ON collapses duplicates *within* the batch, which ON CONFLICT alone
// would not reliably do.
func (r *Repository) CopySlots(ctx context.Context, tx pgx.Tx, slots []Slot) (int64, error) {
	if len(slots) == 0 {
		return 0, nil
	}

	// ON COMMIT DROP ties the staging table's lifetime to this transaction, so
	// a crashed generation run cannot leave debris in pg_temp.
	const create = `
		CREATE TEMP TABLE slot_staging (
			id        UUID        NOT NULL,
			doctor_id UUID        NOT NULL,
			start_at  TIMESTAMPTZ NOT NULL,
			end_at    TIMESTAMPTZ NOT NULL,
			status    TEXT        NOT NULL
		) ON COMMIT DROP`
	if _, err := tx.Exec(ctx, create); err != nil {
		return 0, fmt.Errorf("scheduling: create slot staging: %w", err)
	}

	src := pgx.CopyFromSlice(len(slots), func(i int) ([]any, error) {
		s := slots[i]
		return []any{s.ID, s.DoctorID, s.StartAt, s.EndAt, string(s.Status)}, nil
	})
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"slot_staging"},
		[]string{"id", "doctor_id", "start_at", "end_at", "status"}, src); err != nil {
		return 0, fmt.Errorf("scheduling: copy slots into staging: %w", err)
	}

	const merge = `
		INSERT INTO slots (id, doctor_id, start_at, end_at, status)
		SELECT DISTINCT ON (doctor_id, start_at) id, doctor_id, start_at, end_at, status
		FROM slot_staging
		ORDER BY doctor_id, start_at, id
		ON CONFLICT (doctor_id, start_at) DO NOTHING`
	tag, err := tx.Exec(ctx, merge)
	if err != nil {
		return 0, fmt.Errorf("scheduling: merge staged slots: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ArchiveSlotsBefore moves stale slots out of the live partitions in batches.
// Batching matters: an unbounded DELETE on ninety days of a busy platform holds
// one transaction open long enough to bloat every other table's vacuum horizon.
func (r *Repository) ArchiveSlotsBefore(ctx context.Context, tx pgx.Tx, cutoff time.Time, batch int) (int64, error) {
	const q = `
		WITH victims AS (
			SELECT id, start_at FROM slots
			WHERE start_at < $1
			ORDER BY start_at
			LIMIT $2
		), moved AS (
			DELETE FROM slots s
			USING victims v
			WHERE s.id = v.id AND s.start_at = v.start_at
			RETURNING s.id, s.doctor_id, s.start_at, s.end_at, s.status, s.appointment_id,
			          s.reserved_for, s.reserved_until, s.version, s.created_at, s.updated_at
		)
		INSERT INTO slots_archive (id, doctor_id, start_at, end_at, status, appointment_id,
		                           reserved_for, reserved_until, version, created_at, updated_at)
		SELECT * FROM moved`
	tag, err := tx.Exec(ctx, q, cutoff, batch)
	if err != nil {
		return 0, fmt.Errorf("scheduling: archive slots: %w", err)
	}
	return tag.RowsAffected(), nil
}

// EnsurePartitions keeps a rolling runway of monthly partitions ahead of today.
func (r *Repository) EnsurePartitions(ctx context.Context, q Querier, from time.Time, months int) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT create_slot_partitions($1::date, $2)`, from.UTC().Format(time.DateOnly), months)
	if err != nil {
		return nil, fmt.Errorf("scheduling: ensure partitions: %w", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scheduling: scan partition name: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scheduling: iterate partition names: %w", err)
	}
	return names, nil
}

// CountDoubleBookedSlots is the double-booking post-condition. On a correct
// system it is 0 forever; a non-zero answer means the platform's core promise
// has been broken and is worth paging someone at 3am.
//
// It is a repository method rather than a query pasted into a test so the same
// assertion can run as a production alert.
func (r *Repository) CountDoubleBookedSlots(ctx context.Context, q Querier) (int64, error) {
	const query = `
		SELECT COUNT(*) FROM (
			SELECT slot_id
			FROM appointments
			WHERE status IN ('pending_payment', 'confirmed', 'completed', 'no_show')
			GROUP BY slot_id
			HAVING COUNT(*) > 1
		) AS doubled`
	var n int64
	if err := q.QueryRow(ctx, query).Scan(&n); err != nil {
		return 0, fmt.Errorf("scheduling: count double-booked slots: %w", err)
	}
	return n, nil
}

// CountInconsistentSlots counts slots whose booked state disagrees with the
// appointments table in either direction. The second half of the post-condition:
// uq_appointments_slot_live alone would not catch a slot marked BOOKED against
// an appointment that was rolled back.
func (r *Repository) CountInconsistentSlots(ctx context.Context, q Querier) (int64, error) {
	const query = `
		SELECT COUNT(*)
		FROM slots s
		LEFT JOIN appointments a
		       ON a.id = s.appointment_id
		      AND a.status IN ('pending_payment', 'confirmed', 'completed', 'no_show')
		WHERE (s.status = 'BOOKED' AND a.id IS NULL)
		   OR (s.status = 'BOOKED' AND a.slot_id <> s.id)
		   OR (s.status <> 'BOOKED' AND s.appointment_id IS NOT NULL)`
	var n int64
	if err := q.QueryRow(ctx, query).Scan(&n); err != nil {
		return 0, fmt.Errorf("scheduling: count inconsistent slots: %w", err)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Appointments
// ---------------------------------------------------------------------------

const appointmentColumns = `id, patient_id, doctor_id, slot_id, slot_start_at, slot_end_at,
	status, intake, family_member_id, prepayment_required, amount_cents, currency, specialty,
	payment_id, confirmed_at, completed_at, no_show_at,
	cancelled_at, cancelled_by, cancelled_by_role, cancellation_reason, refund_policy,
	version, created_at, updated_at`

func scanAppointment(row pgx.Row) (Appointment, error) {
	var a Appointment
	var role, reason *string
	var policy *string
	// amount_cents/currency/specialty are nullable for appointments booked
	// before pricing existed. Every appointment booked since carries all three.
	var amountCents *int64
	var currency, specialty *string
	err := row.Scan(&a.ID, &a.PatientID, &a.DoctorID, &a.SlotID, &a.SlotStartAt, &a.SlotEndAt,
		&a.Status, &a.Intake, &a.FamilyMemberID, &a.PrepaymentRequired, &amountCents, &currency, &specialty,
		&a.PaymentID, &a.ConfirmedAt, &a.CompletedAt,
		&a.NoShowAt, &a.CancelledAt, &a.CancelledBy, &role, &reason, &policy,
		&a.Version, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return Appointment{}, err
	}
	if amountCents != nil {
		a.AmountCents = *amountCents
	}
	if currency != nil {
		a.Currency = strings.TrimSpace(*currency)
	}
	if specialty != nil {
		a.Specialty = *specialty
	}
	if role != nil {
		a.CancelledByRole = *role
	}
	if reason != nil {
		a.CancellationReason = *reason
	}
	if policy != nil {
		p := RefundPolicy(*policy)
		a.RefundPolicy = &p
	}
	a.SlotStartAt, a.SlotEndAt = utc(a.SlotStartAt), utc(a.SlotEndAt)
	a.CreatedAt, a.UpdatedAt = utc(a.CreatedAt), utc(a.UpdatedAt)
	a.ConfirmedAt, a.CompletedAt = utcPtr(a.ConfirmedAt), utcPtr(a.CompletedAt)
	a.NoShowAt, a.CancelledAt = utcPtr(a.NoShowAt), utcPtr(a.CancelledAt)
	return a, nil
}

// ---------------------------------------------------------------------------
// doctor pricing projection
// ---------------------------------------------------------------------------

// UpsertDoctorPricing applies one doctor.approved or doctor.updated to the
// pricing projection.
//
// The WHERE clause on the DO UPDATE is the out-of-order guard, and it is the
// only thing standing between an at-least-once broker and a rolled-back price.
// A redelivered older event loses to the newer row already stored; an equal
// timestamp is also refused, because two events with the same instant carry no
// information about which came second. It reports whether the row was actually
// written so a caller can tell "applied" from "correctly ignored as stale".
func (r *Repository) UpsertDoctorPricing(ctx context.Context, q Querier, p DoctorPricing) (bool, error) {
	const stmt = `
		INSERT INTO doctor_pricing
			(doctor_id, specialty, fee_cents, currency, languages, status,
			 last_event_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW())
		ON CONFLICT (doctor_id) DO UPDATE SET
			specialty     = EXCLUDED.specialty,
			fee_cents     = EXCLUDED.fee_cents,
			currency      = EXCLUDED.currency,
			languages     = EXCLUDED.languages,
			status        = EXCLUDED.status,
			last_event_at = EXCLUDED.last_event_at,
			updated_at    = NOW()
		WHERE EXCLUDED.last_event_at > doctor_pricing.last_event_at
		RETURNING doctor_id`

	languages := p.Languages
	if languages == nil {
		languages = []string{}
	}
	var written uuid.UUID
	err := q.QueryRow(ctx, stmt,
		p.DoctorID, p.Specialty, p.FeeCents, p.Currency, languages, p.Status, p.LastEventAt,
	).Scan(&written)
	if errors.Is(err, pgx.ErrNoRows) {
		// The ON CONFLICT matched but the WHERE refused it: a stale or
		// duplicate event. Correctly a no-op, not an error.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("scheduling: upsert doctor pricing: %w", err)
	}
	return true, nil
}

// GetDoctorPricing reads one doctor's current list price.
//
// It returns ErrDoctorNotPriced rather than a zero-valued struct, because a
// missing price must surface as a refusal at booking time. Defaulting it to
// zero is exactly the bug this table exists to fix: the booking would succeed,
// the appointment would hold a slot, and payment-service would refuse it as
// unpayable with nobody watching the log line.
func (r *Repository) GetDoctorPricing(ctx context.Context, q Querier, doctorID uuid.UUID) (DoctorPricing, error) {
	const stmt = `
		SELECT doctor_id, specialty, fee_cents, currency, languages, status,
		       last_event_at, created_at, updated_at
		FROM doctor_pricing
		WHERE doctor_id = $1`

	var p DoctorPricing
	err := q.QueryRow(ctx, stmt, doctorID).Scan(
		&p.DoctorID, &p.Specialty, &p.FeeCents, &p.Currency, &p.Languages, &p.Status,
		&p.LastEventAt, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DoctorPricing{}, ErrDoctorNotPriced
	}
	if err != nil {
		return DoctorPricing{}, fmt.Errorf("scheduling: get doctor pricing: %w", err)
	}
	// CHAR(3) comes back blank-padded on some drivers; a currency with a
	// trailing space fails every ISO 4217 comparison downstream.
	p.Currency = strings.TrimSpace(p.Currency)
	p.LastEventAt = utc(p.LastEventAt)
	p.CreatedAt, p.UpdatedAt = utc(p.CreatedAt), utc(p.UpdatedAt)
	return p, nil
}

// InsertAppointment writes the booking. A 23505 here is the unique index doing
// its job; the service translates it, this layer just reports it faithfully.
func (r *Repository) InsertAppointment(ctx context.Context, tx pgx.Tx, a *Appointment) error {
	const q = `
		INSERT INTO appointments
			(id, patient_id, doctor_id, slot_id, slot_start_at, slot_end_at,
			 status, intake, family_member_id, prepayment_required,
			 amount_cents, currency, specialty, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 0, NOW(), NOW())
		RETURNING created_at, updated_at`
	intake := a.Intake
	if len(intake) == 0 {
		intake = []byte(`{}`)
	}
	err := tx.QueryRow(ctx, q, a.ID, a.PatientID, a.DoctorID, a.SlotID, a.SlotStartAt, a.SlotEndAt,
		string(a.Status), intake, a.FamilyMemberID, a.PrepaymentRequired,
		a.AmountCents, a.Currency, a.Specialty).Scan(&a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return fmt.Errorf("scheduling: insert appointment: %w", err)
	}
	a.CreatedAt, a.UpdatedAt = utc(a.CreatedAt), utc(a.UpdatedAt)
	return nil
}

// GetAppointment reads one appointment, ignoring soft-deleted rows.
func (r *Repository) GetAppointment(ctx context.Context, q Querier, id uuid.UUID) (Appointment, error) {
	a, err := scanAppointment(q.QueryRow(ctx,
		`SELECT `+appointmentColumns+` FROM appointments WHERE id = $1 AND deleted_at IS NULL`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Appointment{}, ErrAppointmentNotFound
	}
	if err != nil {
		return Appointment{}, fmt.Errorf("scheduling: get appointment: %w", err)
	}
	return a, nil
}

// LockAppointment reads an appointment FOR UPDATE. Cancellation touches both the
// appointment and its slot, so both rows are locked in the same transaction --
// always appointment first, then slot, to keep a consistent lock order and rule
// out a deadlock against the booking path.
func (r *Repository) LockAppointment(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Appointment, error) {
	a, err := scanAppointment(tx.QueryRow(ctx,
		`SELECT `+appointmentColumns+` FROM appointments WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Appointment{}, ErrAppointmentNotFound
	}
	if err != nil {
		return Appointment{}, fmt.Errorf("scheduling: lock appointment: %w", err)
	}
	return a, nil
}

// AppointmentFilter narrows a listing.
type AppointmentFilter struct {
	PatientID *uuid.UUID
	DoctorID  *uuid.UUID
	Status    *AppointmentStatus
	From      *time.Time
	To        *time.Time
	Limit     int
	Offset    int
}

// ListAppointments returns a page plus the total matching count.
func (r *Repository) ListAppointments(ctx context.Context, q Querier, f AppointmentFilter) ([]Appointment, int64, error) {
	// Built by appending placeholders, never by concatenating values.
	where := "WHERE deleted_at IS NULL"
	args := []any{}
	add := func(clause string, v any) {
		args = append(args, v)
		where += fmt.Sprintf(" AND %s$%d", clause, len(args))
	}
	if f.PatientID != nil {
		add("patient_id = ", *f.PatientID)
	}
	if f.DoctorID != nil {
		add("doctor_id = ", *f.DoctorID)
	}
	if f.Status != nil {
		add("status = ", string(*f.Status))
	}
	if f.From != nil {
		add("slot_start_at >= ", *f.From)
	}
	if f.To != nil {
		add("slot_start_at < ", *f.To)
	}

	var total int64
	if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM appointments `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("scheduling: count appointments: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	args = append(args, limit, f.Offset)
	query := fmt.Sprintf(`SELECT %s FROM appointments %s ORDER BY slot_start_at DESC, id LIMIT $%d OFFSET $%d`,
		appointmentColumns, where, len(args)-1, len(args))

	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("scheduling: list appointments: %w", err)
	}
	defer rows.Close()

	var out []Appointment
	for rows.Next() {
		a, err := scanAppointment(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scheduling: scan appointment: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("scheduling: iterate appointments: %w", err)
	}
	return out, total, nil
}

// PatientHasOverlappingAppointment guards against one patient holding two
// consultations at once -- with two different doctors, which the per-slot unique
// index cannot see.
func (r *Repository) PatientHasOverlappingAppointment(ctx context.Context, q Querier, patientID uuid.UUID, startAt, endAt time.Time) (bool, error) {
	const query = `
		SELECT EXISTS (
			SELECT 1 FROM appointments
			WHERE patient_id = $1
			  AND status IN ('pending_payment', 'confirmed')
			  AND deleted_at IS NULL
			  AND slot_start_at < $3
			  AND slot_end_at > $2
		)`
	var exists bool
	if err := q.QueryRow(ctx, query, patientID, startAt, endAt).Scan(&exists); err != nil {
		return false, fmt.Errorf("scheduling: check overlapping appointment: %w", err)
	}
	return exists, nil
}

// CancelAppointment records the cancellation and the refund policy it earned.
func (r *Repository) CancelAppointment(ctx context.Context, tx pgx.Tx, id uuid.UUID, expectedVersion int,
	by *uuid.UUID, byRole, reason string, policy RefundPolicy, at time.Time) (int64, error) {
	const q = `
		UPDATE appointments
		SET status = 'cancelled',
		    cancelled_at = $4,
		    cancelled_by = $5,
		    cancelled_by_role = $6,
		    cancellation_reason = $7,
		    refund_policy = $8,
		    version = version + 1,
		    updated_at = NOW()
		WHERE id = $1 AND version = $2 AND status = ANY($3)`
	live := []string{string(AppointmentPendingPayment), string(AppointmentConfirmed)}
	tag, err := tx.Exec(ctx, q, id, expectedVersion, live, at, by, byRole, reason, string(policy))
	if err != nil {
		return 0, fmt.Errorf("scheduling: cancel appointment: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ConfirmAppointment moves a paid booking to confirmed.
func (r *Repository) ConfirmAppointment(ctx context.Context, tx pgx.Tx, id uuid.UUID, paymentID *uuid.UUID, at time.Time) (int64, error) {
	const q = `
		UPDATE appointments
		SET status = 'confirmed',
		    payment_id = COALESCE($2, payment_id),
		    confirmed_at = $3,
		    version = version + 1,
		    updated_at = NOW()
		WHERE id = $1 AND status = 'pending_payment'`
	tag, err := tx.Exec(ctx, q, id, paymentID, at)
	if err != nil {
		return 0, fmt.Errorf("scheduling: confirm appointment: %w", err)
	}
	return tag.RowsAffected(), nil
}

// MarkAppointmentTerminal records completion or a no-show.
func (r *Repository) MarkAppointmentTerminal(ctx context.Context, tx pgx.Tx, id uuid.UUID, status AppointmentStatus, at time.Time) (int64, error) {
	column := "completed_at"
	if status == AppointmentNoShow {
		column = "no_show_at"
	}
	q := fmt.Sprintf(`
		UPDATE appointments
		SET status = $2, %s = $3, version = version + 1, updated_at = NOW()
		WHERE id = $1 AND status = 'confirmed'`, column)
	tag, err := tx.Exec(ctx, q, id, string(status), at)
	if err != nil {
		return 0, fmt.Errorf("scheduling: mark appointment %s: %w", status, err)
	}
	return tag.RowsAffected(), nil
}

// ListStaleAppointments finds bookings in a status they should have left by now:
// pending_payment past the payment window, or confirmed well past their end
// time with nobody having reported the outcome.
func (r *Repository) ListStaleAppointments(ctx context.Context, q Querier, status AppointmentStatus, column string, cutoff time.Time, limit int) ([]Appointment, error) {
	// column is chosen from a fixed set by the caller in this package; it is
	// never derived from user input.
	switch column {
	case "created_at", "slot_end_at":
	default:
		return nil, fmt.Errorf("scheduling: unsupported stale column %q", column)
	}
	query := fmt.Sprintf(`
		SELECT %s FROM appointments
		WHERE status = $1 AND %s < $2 AND deleted_at IS NULL
		ORDER BY %s
		LIMIT $3`, appointmentColumns, column, column)

	rows, err := q.Query(ctx, query, string(status), cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("scheduling: list stale appointments: %w", err)
	}
	defer rows.Close()

	var out []Appointment
	for rows.Next() {
		a, err := scanAppointment(rows)
		if err != nil {
			return nil, fmt.Errorf("scheduling: scan stale appointment: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scheduling: iterate stale appointments: %w", err)
	}
	return out, nil
}
