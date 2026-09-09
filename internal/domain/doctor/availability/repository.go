package availability

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Repository applies slot events to doctor_slot_state and keeps
// doctor_availability_summary consistent with it, inside the caller's
// transaction.
type Repository struct{}

// NewRepository returns a stateless repository; every method takes the
// transaction it runs on, same convention as the rest of the platform.
func NewRepository() *Repository { return &Repository{} }

// ApplySlotEvent upserts one slot's state and, if the upsert actually
// changed anything, recomputes the (doctor_id, date) summary row it belongs
// to. It is a no-op -- by design, not by accident -- when:
//
//   - eventID exactly matches the slot's last-applied event (exact
//     redelivery of a message we already processed), or
//   - occurredAt is older than the slot's last-applied event time (a
//     message that raced ahead of one we already applied, arriving late).
//
// Both cases are expected under NATS JetStream's at-least-once, unordered
// delivery guarantee. Using event time rather than arrival order for the
// second check is what makes the projection converge to the same final
// state no matter what order the three events for a given slot arrive in.
func (r *Repository) ApplySlotEvent(
	ctx context.Context, tx pgx.Tx,
	eventID uuid.UUID, occurredAt time.Time,
	status SlotStatus, payload SlotEventPayload,
	loc *time.Location,
) error {
	local := payload.StartAt.In(loc)
	slotDate := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)

	const upsert = `
		INSERT INTO doctor_slot_state
			(slot_id, doctor_id, start_at, slot_date, status, last_event_id, last_event_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW())
		ON CONFLICT (slot_id) DO UPDATE SET
			doctor_id     = EXCLUDED.doctor_id,
			start_at      = EXCLUDED.start_at,
			slot_date     = EXCLUDED.slot_date,
			status        = EXCLUDED.status,
			last_event_id = EXCLUDED.last_event_id,
			last_event_at = EXCLUDED.last_event_at,
			updated_at    = NOW()
		WHERE doctor_slot_state.last_event_id IS DISTINCT FROM EXCLUDED.last_event_id
		  AND EXCLUDED.last_event_at >= doctor_slot_state.last_event_at
		RETURNING doctor_id, slot_date`

	var doctorID uuid.UUID
	var appliedDate time.Time
	err := tx.QueryRow(ctx, upsert,
		payload.SlotID, payload.DoctorID, payload.StartAt, slotDate, string(status), eventID, occurredAt,
	).Scan(&doctorID, &appliedDate)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Duplicate or stale-out-of-order event: correctly ignored.
			return nil
		}
		return fmt.Errorf("availability: apply slot event: %w", err)
	}

	return r.recomputeSummary(ctx, tx, doctorID, appliedDate)
}

// recomputeSummary rebuilds the (doctor_id, date) row of
// doctor_availability_summary from doctor_slot_state. It always produces
// exactly one row for the pair because the caller just wrote a row into
// doctor_slot_state for that same (doctor_id, date) -- COUNT/MIN over an
// empty AVAILABLE set still yields a valid 0-count row rather than no row.
func (r *Repository) recomputeSummary(ctx context.Context, tx pgx.Tx, doctorID uuid.UUID, date time.Time) error {
	const recompute = `
		INSERT INTO doctor_availability_summary (doctor_id, date, available_slot_count, next_available_at, updated_at)
		SELECT
			doctor_id,
			slot_date,
			COUNT(*) FILTER (WHERE status = 'AVAILABLE'),
			MIN(start_at) FILTER (WHERE status = 'AVAILABLE'),
			NOW()
		FROM doctor_slot_state
		WHERE doctor_id = $1 AND slot_date = $2
		GROUP BY doctor_id, slot_date
		ON CONFLICT (doctor_id, date) DO UPDATE SET
			available_slot_count = EXCLUDED.available_slot_count,
			next_available_at    = EXCLUDED.next_available_at,
			updated_at            = NOW()`

	if _, err := tx.Exec(ctx, recompute, doctorID, date); err != nil {
		return fmt.Errorf("availability: recompute summary: %w", err)
	}
	return nil
}

// SlotStateFor returns the current projected state of one slot. Exported for
// tests; production code never needs to read a single slot's state, only the
// aggregated summary that search queries.
func (r *Repository) SlotStateFor(ctx context.Context, tx pgx.Tx, slotID uuid.UUID) (SlotState, bool, error) {
	const q = `
		SELECT slot_id, doctor_id, start_at, slot_date, status, last_event_id, last_event_at
		FROM doctor_slot_state WHERE slot_id = $1`
	var s SlotState
	var status string
	err := tx.QueryRow(ctx, q, slotID).Scan(
		&s.SlotID, &s.DoctorID, &s.StartAt, &s.SlotDate, &status, &s.LastEventID, &s.LastEventAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SlotState{}, false, nil
		}
		return SlotState{}, false, fmt.Errorf("availability: slot state: %w", err)
	}
	s.Status = SlotStatus(status)
	return s, true, nil
}

// SummaryFor returns the summary row for (doctorID, date), if any. Exported
// for tests.
func (r *Repository) SummaryFor(ctx context.Context, tx pgx.Tx, doctorID uuid.UUID, date time.Time) (DailySummary, bool, error) {
	const q = `
		SELECT doctor_id, date, available_slot_count, next_available_at
		FROM doctor_availability_summary WHERE doctor_id = $1 AND date = $2`
	var s DailySummary
	err := tx.QueryRow(ctx, q, doctorID, date).Scan(&s.DoctorID, &s.Date, &s.AvailableSlotCount, &s.NextAvailableAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DailySummary{}, false, nil
		}
		return DailySummary{}, false, fmt.Errorf("availability: summary: %w", err)
	}
	return s, true, nil
}
