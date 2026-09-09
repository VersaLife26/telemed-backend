package doctor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
)

const applicationColumns = `
	id, phone, email, display_name, slmc_number, specialty, languages,
	experience_years, fee_cents, bio, status, rejection_reason,
	decided_at, decided_by, activated_user_id, activated_at, created_at, updated_at`

func scanApplication(row pgx.Row) (Application, error) {
	var a Application
	var languages []string
	var bio, rejection *string
	var decidedAt, activatedAt *time.Time
	var decidedBy, activatedUserID *uuid.UUID
	var status string

	err := row.Scan(
		&a.ID, &a.Phone, &a.Email, &a.DisplayName, &a.SLMCNumber, &a.Specialty, &languages,
		&a.ExperienceYears, &a.FeeCents, &bio, &status, &rejection,
		&decidedAt, &decidedBy, &activatedUserID, &activatedAt, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		return Application{}, err
	}
	a.Languages = stringsToLanguages(languages)
	a.Bio = deref(bio)
	a.RejectionReason = deref(rejection)
	a.Status = ApplicationStatus(status)
	a.DecidedAt = decidedAt
	a.DecidedBy = decidedBy
	a.ActivatedUserID = activatedUserID
	a.ActivatedAt = activatedAt
	return a, nil
}

// CreateApplication inserts a pending application.
func (r *Repository) CreateApplication(ctx context.Context, tx pgx.Tx, a *Application) error {
	const q = `
		INSERT INTO doctor_applications (
			id, phone, email, display_name, slmc_number, specialty, languages,
			experience_years, fee_cents, bio, status, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7,
			$8, $9, $10, $11, NOW(), NOW()
		)`
	_, err := tx.Exec(ctx, q,
		a.ID, a.Phone, a.Email, a.DisplayName, a.SLMCNumber, a.Specialty, languagesToStrings(a.Languages),
		a.ExperienceYears, a.FeeCents, a.Bio, string(a.Status),
	)
	if err != nil {
		if database.IsUniqueViolation(err) {
			return ErrApplicationExists
		}
		return fmt.Errorf("doctor: create application: %w", err)
	}
	return nil
}

// GetApplication returns one application by id.
func (r *Repository) GetApplication(ctx context.Context, id uuid.UUID) (Application, error) {
	q := `SELECT ` + applicationColumns + ` FROM doctor_applications WHERE id = $1`
	a, err := scanApplication(r.pool.QueryRow(ctx, q, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Application{}, ErrApplicationNotFound
		}
		return Application{}, fmt.Errorf("doctor: get application: %w", err)
	}
	return a, nil
}

// FindOpenApplicationByPhone returns the pending or approved application for a phone.
func (r *Repository) FindOpenApplicationByPhone(ctx context.Context, phone string) (Application, error) {
	q := `SELECT ` + applicationColumns + `
		FROM doctor_applications
		WHERE phone = $1 AND status IN ('pending', 'approved')
		ORDER BY created_at DESC
		LIMIT 1`
	a, err := scanApplication(r.pool.QueryRow(ctx, q, phone))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Application{}, ErrApplicationNotFound
		}
		return Application{}, fmt.Errorf("doctor: find open application: %w", err)
	}
	return a, nil
}

// FindOpenOrRecentApplicationByPhone returns the most recent open application,
// or the latest rejected/activated row when nothing is open (for eligibility UX).
func (r *Repository) FindOpenOrRecentApplicationByPhone(ctx context.Context, phone string) (Application, error) {
	if a, err := r.FindOpenApplicationByPhone(ctx, phone); err == nil {
		return a, nil
	} else if !errors.Is(err, ErrApplicationNotFound) {
		return Application{}, err
	}
	q := `SELECT ` + applicationColumns + `
		FROM doctor_applications
		WHERE phone = $1
		ORDER BY created_at DESC
		LIMIT 1`
	a, err := scanApplication(r.pool.QueryRow(ctx, q, phone))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Application{}, ErrApplicationNotFound
		}
		return Application{}, fmt.Errorf("doctor: find recent application: %w", err)
	}
	return a, nil
}

// DecideApplication records approve/reject on a pending application.
func (r *Repository) DecideApplication(
	ctx context.Context, tx pgx.Tx, id uuid.UUID,
	status ApplicationStatus, reason string, actorID *uuid.UUID, at time.Time,
) error {
	const q = `
		UPDATE doctor_applications SET
			status = $2,
			rejection_reason = CASE WHEN $2 = 'rejected' THEN $3 ELSE rejection_reason END,
			decided_at = $4,
			decided_by = $5,
			updated_at = NOW()
		WHERE id = $1 AND status = 'pending'`
	tag, err := tx.Exec(ctx, q, id, string(status), nullString(reason), at, actorID)
	if err != nil {
		return fmt.Errorf("doctor: decide application: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrInvalidTransition
	}
	return nil
}

// MarkApplicationActivated ties an approved application to the OTP-verified user.
func (r *Repository) MarkApplicationActivated(ctx context.Context, tx pgx.Tx, id, userID uuid.UUID, at time.Time) error {
	const q = `
		UPDATE doctor_applications SET
			status = 'activated',
			activated_user_id = $2,
			activated_at = $3,
			updated_at = NOW()
		WHERE id = $1 AND status = 'approved'`
	tag, err := tx.Exec(ctx, q, id, userID, at)
	if err != nil {
		return fmt.Errorf("doctor: activate application: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrApplicationNotReady
	}
	return nil
}
