package notification

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Doctor is one doctor_directory row: the small set of doctor facts this
// service needs in order to write a sentence about one.
//
// It is a projection of telemed_doctor, fed by doctor.approved and
// doctor.updated. This service never queries doctor-service's database
// (ADR-004) and never calls it synchronously either -- both events already
// arrive here, so the data is a local read by the time it is needed.
type Doctor struct {
	DoctorID  uuid.UUID
	UserID    uuid.UUID
	FullName  string
	Email     string
	Specialty string
	FeeCents  int64
	Currency  string
	Status    string
}

// ErrDoctorNotProjected means no doctor.approved has been seen for this id.
//
// It is deliberately an error and not a zero Doctor. A notification that
// reads "Your appointment with Dr. is confirmed" is worse than no
// notification: it is unactionable, it looks broken to the patient, and
// nothing anywhere records that it happened. Returning an error lets the
// event be redelivered until the projection catches up, which is the correct
// outcome when two consumers race on a freshly approved doctor.
var ErrDoctorNotProjected = errors.New("notification: doctor not in local directory")

// UpsertDoctorFromApproval applies a doctor.approved event. It carries every
// field, so it is a full upsert.
func (r *Repository) UpsertDoctorFromApproval(ctx context.Context, db Querier, d Doctor) error {
	const q = `
		INSERT INTO doctor_directory (
			doctor_id, user_id, full_name, email, specialty, fee_cents, currency, status
		)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'approved')
		ON CONFLICT (doctor_id) DO UPDATE SET
			user_id    = EXCLUDED.user_id,
			full_name  = EXCLUDED.full_name,
			email      = EXCLUDED.email,
			specialty  = EXCLUDED.specialty,
			fee_cents  = EXCLUDED.fee_cents,
			currency   = EXCLUDED.currency,
			status     = 'approved',
			updated_at = NOW()`
	if _, err := db.Exec(ctx, q, d.DoctorID, d.UserID, d.FullName, d.Email, d.Specialty, d.FeeCents, d.Currency); err != nil {
		return fmt.Errorf("notification: upsert doctor directory: %w", err)
	}
	return nil
}

// UpdateDoctorProfile applies a doctor.updated event.
//
// doctor.updated carries the profile fields that change often (specialty,
// fee, status) but NOT the name or email, so those columns are left alone
// rather than being overwritten with the zero value. An UPDATE that touches
// only what the event actually said is the whole difference between a
// projection and a data-loss bug.
//
// An update for a doctor never approved here is not an error: the row simply
// does not exist yet, and the approval event that creates it may still be in
// flight. Silently doing nothing is correct -- doctor.approved carries every
// field and will set them all when it arrives.
func (r *Repository) UpdateDoctorProfile(ctx context.Context, db Querier, d Doctor) error {
	const q = `
		UPDATE doctor_directory
		   SET specialty  = $2,
		       fee_cents  = $3,
		       currency   = $4,
		       status     = $5,
		       updated_at = NOW()
		 WHERE doctor_id = $1`
	if _, err := db.Exec(ctx, q, d.DoctorID, d.Specialty, d.FeeCents, d.Currency, d.Status); err != nil {
		return fmt.Errorf("notification: update doctor directory: %w", err)
	}
	return nil
}

// DoctorByID reads one projected doctor.
func (r *Repository) DoctorByID(ctx context.Context, db Querier, doctorID uuid.UUID) (Doctor, error) {
	const q = `
		SELECT doctor_id, user_id, full_name, email, specialty, fee_cents, currency, status
		  FROM doctor_directory
		 WHERE doctor_id = $1`
	var d Doctor
	err := db.QueryRow(ctx, q, doctorID).Scan(
		&d.DoctorID, &d.UserID, &d.FullName, &d.Email, &d.Specialty, &d.FeeCents, &d.Currency, &d.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Doctor{}, fmt.Errorf("%w: %s", ErrDoctorNotProjected, doctorID)
	}
	if err != nil {
		return Doctor{}, fmt.Errorf("notification: read doctor directory: %w", err)
	}
	return d, nil
}

// DisplayName renders the doctor's name the way every template expects it:
// the templates already say "Dr. {{.DoctorName}}", so this returns the bare
// name and never prefixes it again.
func (d Doctor) DisplayName() string { return d.FullName }

// deadlineFor is a small helper for the consumer's lookups, keeping a slow
// dependency from pinning a JetStream delivery open indefinitely.
func deadlineFor(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}
