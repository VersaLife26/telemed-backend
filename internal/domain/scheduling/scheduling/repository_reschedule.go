package scheduling

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
)

const rescheduleColumns = `
	id, appointment_id, patient_id, doctor_id,
	original_slot_id, original_start_at, original_end_at,
	proposed_slot_id, proposed_start_at, proposed_end_at, proposed_slot_created,
	reason, status, decided_by, decided_by_role, decided_at,
	version, created_at, updated_at`

func scanRescheduleRequest(row pgx.Row) (RescheduleRequest, error) {
	var r RescheduleRequest
	var decidedByRole *string
	err := row.Scan(
		&r.ID, &r.AppointmentID, &r.PatientID, &r.DoctorID,
		&r.OriginalSlotID, &r.OriginalStartAt, &r.OriginalEndAt,
		&r.ProposedSlotID, &r.ProposedStartAt, &r.ProposedEndAt, &r.ProposedSlotCreated,
		&r.Reason, &r.Status, &r.DecidedBy, &decidedByRole, &r.DecidedAt,
		&r.Version, &r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		return RescheduleRequest{}, err
	}
	if decidedByRole != nil {
		r.DecidedByRole = *decidedByRole
	}
	r.OriginalStartAt, r.OriginalEndAt = utc(r.OriginalStartAt), utc(r.OriginalEndAt)
	r.ProposedStartAt, r.ProposedEndAt = utc(r.ProposedStartAt), utc(r.ProposedEndAt)
	r.CreatedAt, r.UpdatedAt = utc(r.CreatedAt), utc(r.UpdatedAt)
	r.DecidedAt = utcPtr(r.DecidedAt)
	return r, nil
}

// LockSlotByDoctorStart locks the unique (doctor_id, start_at) row, if any.
func (r *Repository) LockSlotByDoctorStart(ctx context.Context, tx pgx.Tx, doctorID uuid.UUID, startAt time.Time) (Slot, error) {
	q := `SELECT ` + slotColumns + ` FROM slots WHERE doctor_id = $1 AND start_at = $2 FOR UPDATE`
	s, err := scanSlot(tx.QueryRow(ctx, q, doctorID, startAt.UTC()))
	if errors.Is(err, pgx.ErrNoRows) {
		return Slot{}, ErrSlotNotFound
	}
	if err != nil {
		return Slot{}, fmt.Errorf("scheduling: lock slot by doctor start: %w", err)
	}
	return s, nil
}

// HoldSlotForReschedule parks an AVAILABLE generated slot as BLOCKED for one
// patient with no reserved_until, so the waitlist sweeper cannot steal it.
func (r *Repository) HoldSlotForReschedule(ctx context.Context, tx pgx.Tx, slotID uuid.UUID, startAt time.Time, patientID uuid.UUID, expectedVersion int) (int64, error) {
	const q = `
		UPDATE slots
		SET status = 'BLOCKED',
		    reserved_for = $4,
		    reserved_until = NULL,
		    version = version + 1,
		    updated_at = NOW()
		WHERE id = $1 AND start_at = $2 AND version = $3 AND status = 'AVAILABLE'`
	tag, err := tx.Exec(ctx, q, slotID, startAt, expectedVersion, patientID)
	if err != nil {
		return 0, fmt.Errorf("scheduling: hold slot for reschedule: %w", err)
	}
	return tag.RowsAffected(), nil
}

// InsertAdHocBlockedSlot materialises a slot that the generator never produced,
// already held for the patient this request belongs to.
func (r *Repository) InsertAdHocBlockedSlot(ctx context.Context, tx pgx.Tx, s Slot) (Slot, error) {
	const q = `
		INSERT INTO slots (id, doctor_id, start_at, end_at, status, reserved_for, reserved_until, version)
		VALUES ($1, $2, $3, $4, 'BLOCKED', $5, NULL, 0)
		RETURNING ` + slotColumns
	out, err := scanSlot(tx.QueryRow(ctx, q, s.ID, s.DoctorID, s.StartAt.UTC(), s.EndAt.UTC(), s.ReservedFor))
	if err != nil {
		return Slot{}, fmt.Errorf("scheduling: insert ad-hoc blocked slot: %w", err)
	}
	return out, nil
}

// PatientHasOverlappingAppointmentExcluding is the patient overlap check with
// the appointment being moved taken out of the predicate, otherwise a propose
// that overlaps the original window would always collide with itself.
func (r *Repository) PatientHasOverlappingAppointmentExcluding(ctx context.Context, q Querier, patientID, excludeAppointmentID uuid.UUID, startAt, endAt time.Time) (bool, error) {
	const query = `
		SELECT EXISTS (
			SELECT 1 FROM appointments
			WHERE patient_id = $1
			  AND id <> $2
			  AND status IN ('pending_payment', 'confirmed')
			  AND deleted_at IS NULL
			  AND slot_start_at < $4
			  AND slot_end_at > $3
		)`
	var exists bool
	if err := q.QueryRow(ctx, query, patientID, excludeAppointmentID, startAt, endAt).Scan(&exists); err != nil {
		return false, fmt.Errorf("scheduling: check patient overlapping appointment: %w", err)
	}
	return exists, nil
}

// DoctorHasOverlappingAppointmentExcluding is the same window check for one
// doctor's live bookings.
func (r *Repository) DoctorHasOverlappingAppointmentExcluding(ctx context.Context, q Querier, doctorID, excludeAppointmentID uuid.UUID, startAt, endAt time.Time) (bool, error) {
	const query = `
		SELECT EXISTS (
			SELECT 1 FROM appointments
			WHERE doctor_id = $1
			  AND id <> $2
			  AND status IN ('pending_payment', 'confirmed')
			  AND deleted_at IS NULL
			  AND slot_start_at < $4
			  AND slot_end_at > $3
		)`
	var exists bool
	if err := q.QueryRow(ctx, query, doctorID, excludeAppointmentID, startAt, endAt).Scan(&exists); err != nil {
		return false, fmt.Errorf("scheduling: check doctor overlapping appointment: %w", err)
	}
	return exists, nil
}

// InsertRescheduleRequest writes a pending request. A 23505 on the pending
// unique index is a second propose racing the first.
func (r *Repository) InsertRescheduleRequest(ctx context.Context, tx pgx.Tx, req *RescheduleRequest) error {
	const q = `
		INSERT INTO reschedule_requests (
			id, appointment_id, patient_id, doctor_id,
			original_slot_id, original_start_at, original_end_at,
			proposed_slot_id, proposed_start_at, proposed_end_at, proposed_slot_created,
			reason, status, version, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7,
			$8, $9, $10, $11,
			$12, 'pending', 0, NOW(), NOW()
		)
		RETURNING created_at, updated_at`
	if req.ID == uuid.Nil {
		req.ID = uuid.New()
	}
	err := tx.QueryRow(ctx, q,
		req.ID, req.AppointmentID, req.PatientID, req.DoctorID,
		req.OriginalSlotID, req.OriginalStartAt, req.OriginalEndAt,
		req.ProposedSlotID, req.ProposedStartAt, req.ProposedEndAt, req.ProposedSlotCreated,
		req.Reason,
	).Scan(&req.CreatedAt, &req.UpdatedAt)
	if err != nil {
		return fmt.Errorf("scheduling: insert reschedule request: %w", err)
	}
	req.Status = ReschedulePending
	req.CreatedAt, req.UpdatedAt = utc(req.CreatedAt), utc(req.UpdatedAt)
	return nil
}

// GetRescheduleRequest reads one request without locking it.
func (r *Repository) GetRescheduleRequest(ctx context.Context, q Querier, id uuid.UUID) (RescheduleRequest, error) {
	req, err := scanRescheduleRequest(q.QueryRow(ctx,
		`SELECT `+rescheduleColumns+` FROM reschedule_requests WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return RescheduleRequest{}, ErrRescheduleNotFound
	}
	if err != nil {
		return RescheduleRequest{}, fmt.Errorf("scheduling: get reschedule request: %w", err)
	}
	return req, nil
}

// LockRescheduleRequest takes FOR UPDATE on one request.
func (r *Repository) LockRescheduleRequest(ctx context.Context, tx pgx.Tx, id uuid.UUID) (RescheduleRequest, error) {
	req, err := scanRescheduleRequest(tx.QueryRow(ctx,
		`SELECT `+rescheduleColumns+` FROM reschedule_requests WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return RescheduleRequest{}, ErrRescheduleNotFound
	}
	if err != nil {
		return RescheduleRequest{}, fmt.Errorf("scheduling: lock reschedule request: %w", err)
	}
	return req, nil
}

// GetPendingRescheduleByAppointment returns the open request for a booking, if any.
func (r *Repository) GetPendingRescheduleByAppointment(ctx context.Context, q Querier, appointmentID uuid.UUID) (RescheduleRequest, error) {
	req, err := scanRescheduleRequest(q.QueryRow(ctx,
		`SELECT `+rescheduleColumns+` FROM reschedule_requests
		 WHERE appointment_id = $1 AND status = 'pending'`, appointmentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return RescheduleRequest{}, ErrRescheduleNotFound
	}
	if err != nil {
		return RescheduleRequest{}, fmt.Errorf("scheduling: get pending reschedule: %w", err)
	}
	return req, nil
}

// ListRescheduleRequestsForAppointment returns every request for one booking,
// newest first.
func (r *Repository) ListRescheduleRequestsForAppointment(ctx context.Context, q Querier, appointmentID uuid.UUID) ([]RescheduleRequest, error) {
	rows, err := q.Query(ctx,
		`SELECT `+rescheduleColumns+` FROM reschedule_requests
		 WHERE appointment_id = $1
		 ORDER BY created_at DESC`, appointmentID)
	if err != nil {
		return nil, fmt.Errorf("scheduling: list reschedule requests: %w", err)
	}
	defer rows.Close()
	return scanRescheduleRows(rows)
}

// ListPendingRescheduleRequests is the admin queue.
func (r *Repository) ListPendingRescheduleRequests(ctx context.Context, q Querier, limit, offset int) ([]RescheduleRequest, int64, error) {
	var total int64
	if err := q.QueryRow(ctx,
		`SELECT COUNT(*) FROM reschedule_requests WHERE status = 'pending'`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("scheduling: count pending reschedule requests: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	if limit <= 0 {
		limit = 20
	}
	rows, err := q.Query(ctx,
		`SELECT `+rescheduleColumns+` FROM reschedule_requests
		 WHERE status = 'pending'
		 ORDER BY created_at DESC
		 LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("scheduling: list pending reschedule requests: %w", err)
	}
	defer rows.Close()
	out, err := scanRescheduleRows(rows)
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// ListExpiredPendingRescheduleRequests finds pending rows whose original start
// has passed — the doctor never showed, so the expiry job runs the decline path.
func (r *Repository) ListExpiredPendingRescheduleRequests(ctx context.Context, q Querier, now time.Time, limit int) ([]RescheduleRequest, error) {
	rows, err := q.Query(ctx,
		`SELECT `+rescheduleColumns+` FROM reschedule_requests
		 WHERE status = 'pending' AND original_start_at <= $1
		 ORDER BY original_start_at
		 LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("scheduling: list expired reschedule requests: %w", err)
	}
	defer rows.Close()
	return scanRescheduleRows(rows)
}

func scanRescheduleRows(rows pgx.Rows) ([]RescheduleRequest, error) {
	var out []RescheduleRequest
	for rows.Next() {
		req, err := scanRescheduleRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("scheduling: scan reschedule request: %w", err)
		}
		out = append(out, req)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scheduling: iterate reschedule requests: %w", err)
	}
	return out, nil
}

// DecideRescheduleRequest is the version-guarded terminal write.
func (r *Repository) DecideRescheduleRequest(ctx context.Context, tx pgx.Tx, id uuid.UUID, expectedVersion int,
	status RescheduleStatus, decidedBy *uuid.UUID, role string, at time.Time) (int64, error) {
	const q = `
		UPDATE reschedule_requests
		SET status = $3,
		    decided_by = $4,
		    decided_by_role = $5,
		    decided_at = $6,
		    version = version + 1,
		    updated_at = NOW()
		WHERE id = $1 AND version = $2 AND status = 'pending'`
	tag, err := tx.Exec(ctx, q, id, expectedVersion, string(status), decidedBy, role, at)
	if err != nil {
		return 0, fmt.Errorf("scheduling: decide reschedule request: %w", err)
	}
	return tag.RowsAffected(), nil
}

// MoveAppointment points a confirmed booking at a different slot. Same id,
// same payment, new instants.
func (r *Repository) MoveAppointment(ctx context.Context, tx pgx.Tx, id uuid.UUID, expectedVersion int,
	slotID uuid.UUID, startAt, endAt time.Time) (int64, error) {
	const q = `
		UPDATE appointments
		SET slot_id = $3,
		    slot_start_at = $4,
		    slot_end_at = $5,
		    version = version + 1,
		    updated_at = NOW()
		WHERE id = $1 AND version = $2 AND status = 'confirmed'`
	tag, err := tx.Exec(ctx, q, id, expectedVersion, slotID, startAt, endAt)
	if err != nil {
		return 0, fmt.Errorf("scheduling: move appointment: %w", err)
	}
	return tag.RowsAffected(), nil
}

// UniqueConstraintPendingReschedule is the Postgres name of the partial unique
// index that enforces one live request per appointment.
const UniqueConstraintPendingReschedule = "uq_reschedule_requests_pending"

// IsPendingRescheduleConflict reports whether err is the second-propose 23505.
func IsPendingRescheduleConflict(err error) bool {
	return database.UniqueConstraint(err) == UniqueConstraintPendingReschedule
}
