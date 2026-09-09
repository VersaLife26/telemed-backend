package doctor

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
)

// IsEligibleForReview reports whether patientID completed appointmentID with
// doctorID, i.e. whether the appointment.completed consumer recorded an
// eligibility row for it. This is the enforcement point for "only if the
// caller completed an appointment with them".
func (r *Repository) IsEligibleForReview(ctx context.Context, doctorID, patientID, appointmentID uuid.UUID) (bool, error) {
	const q = `
		SELECT 1 FROM review_eligibility
		WHERE appointment_id = $1 AND doctor_id = $2 AND patient_id = $3`
	var one int
	err := r.pool.QueryRow(ctx, q, appointmentID, doctorID, patientID).Scan(&one)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("doctor: check review eligibility: %w", err)
	}
	return true, nil
}

// RecordEligibility is called from the appointment.completed consumer. The
// INSERT ... ON CONFLICT DO NOTHING on the appointment_id primary key is
// what makes the consumer idempotent: a redelivered event is a harmless
// no-op, signalled to the caller via the second return value so it knows
// whether to also bump consultation_count.
func (r *Repository) RecordEligibility(ctx context.Context, tx pgx.Tx, doctorID, patientID, appointmentID uuid.UUID) (created bool, err error) {
	const q = `
		INSERT INTO review_eligibility (appointment_id, doctor_id, patient_id, created_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (appointment_id) DO NOTHING
		RETURNING appointment_id`
	var id uuid.UUID
	err = tx.QueryRow(ctx, q, appointmentID, doctorID, patientID).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("doctor: record eligibility: %w", err)
	}
	return true, nil
}

// InsertReview creates a review. UNIQUE(appointment_id) is the backstop
// against a double-submit race the eligibility check alone cannot close.
func (r *Repository) InsertReview(ctx context.Context, tx pgx.Tx, rv *Review) error {
	const q = `
		INSERT INTO reviews (id, doctor_id, patient_id, appointment_id, rating, comment, is_published, created_at, updated_at, version)
		VALUES ($1, $2, $3, $4, $5, $6, TRUE, NOW(), NOW(), 0)`
	_, err := tx.Exec(ctx, q, rv.ID, rv.DoctorID, rv.PatientID, rv.AppointmentID, rv.Rating, nullString(rv.Comment))
	if err != nil {
		if database.IsUniqueViolation(err) {
			return ErrReviewExists
		}
		return fmt.Errorf("doctor: insert review: %w", err)
	}
	return nil
}

// ListPublishedReviews returns published reviews for a doctor, newest first.
func (r *Repository) ListPublishedReviews(ctx context.Context, doctorID uuid.UUID, limit, offset int) ([]Review, int64, error) {
	const q = `
		SELECT id, doctor_id, patient_id, appointment_id, rating, COALESCE(comment, ''), is_published, created_at, updated_at, version
		FROM reviews
		WHERE doctor_id = $1 AND is_published = TRUE AND deleted_at IS NULL
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3`

	rows, err := r.pool.Query(ctx, q, doctorID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("doctor: list reviews: %w", err)
	}
	defer rows.Close()

	var out []Review
	for rows.Next() {
		var rv Review
		if err := rows.Scan(&rv.ID, &rv.DoctorID, &rv.PatientID, &rv.AppointmentID, &rv.Rating,
			&rv.Comment, &rv.IsPublished, &rv.CreatedAt, &rv.UpdatedAt, &rv.Version); err != nil {
			return nil, 0, fmt.Errorf("doctor: scan review: %w", err)
		}
		out = append(out, rv)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("doctor: reviews rows: %w", err)
	}

	var total int64
	const countQ = `SELECT COUNT(*) FROM reviews WHERE doctor_id = $1 AND is_published = TRUE AND deleted_at IS NULL`
	if err := r.pool.QueryRow(ctx, countQ, doctorID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("doctor: count reviews: %w", err)
	}
	return out, total, nil
}
