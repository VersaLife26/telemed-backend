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
	id, phone, email, display_name, first_name, last_name, slmc_number, specialty, languages,
	language_other, experience_years, fee_cents, required_fee_cents, bio,
	pgim_board_certified, medical_school, qualifications_text, availability_notes,
	is_general_practitioner, practicing_locations, terms_accepted_at,
	bank_encrypted, bank_name, bank_branch,
	status, rejection_reason,
	decided_at, decided_by, activated_user_id, activated_at, created_at, updated_at`

func scanApplication(row pgx.Row) (Application, error) {
	var a Application
	var languages, locations []string
	var bio, rejection, languageOther, medicalSchool, quals, availability, bankEnc, bankName, bankBranch *string
	var firstName, lastName *string
	var decidedAt, activatedAt, termsAt *time.Time
	var decidedBy, activatedUserID *uuid.UUID
	var status string
	var pgim, gp bool
	var requiredFee int64

	err := row.Scan(
		&a.ID, &a.Phone, &a.Email, &a.DisplayName, &firstName, &lastName, &a.SLMCNumber, &a.Specialty, &languages,
		&languageOther, &a.ExperienceYears, &a.FeeCents, &requiredFee, &bio,
		&pgim, &medicalSchool, &quals, &availability,
		&gp, &locations, &termsAt,
		&bankEnc, &bankName, &bankBranch,
		&status, &rejection,
		&decidedAt, &decidedBy, &activatedUserID, &activatedAt, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		return Application{}, err
	}
	a.Languages = stringsToLanguages(languages)
	a.FirstName = deref(firstName)
	a.LastName = deref(lastName)
	a.LanguageOther = deref(languageOther)
	a.RequiredFeeCents = requiredFee
	a.Bio = deref(bio)
	a.PGIMBoardCertified = pgim
	a.MedicalSchool = deref(medicalSchool)
	a.QualificationsText = deref(quals)
	a.AvailabilityNotes = deref(availability)
	a.IsGeneralPractitioner = gp
	a.PracticingLocations = locations
	a.TermsAcceptedAt = termsAt
	a.BankEncrypted = deref(bankEnc)
	a.BankName = deref(bankName)
	a.BankBranch = deref(bankBranch)
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
			id, phone, email, display_name, first_name, last_name, slmc_number, specialty, languages,
			language_other, experience_years, fee_cents, required_fee_cents, bio,
			pgim_board_certified, medical_school, qualifications_text, availability_notes,
			is_general_practitioner, practicing_locations, terms_accepted_at,
			bank_encrypted, bank_name, bank_branch, status, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9,
			$10, $11, $12, $13, $14,
			$15, $16, $17, $18,
			$19, $20, $21,
			$22, $23, $24, $25, NOW(), NOW()
		)`
	locs := a.PracticingLocations
	if locs == nil {
		locs = []string{}
	}
	_, err := tx.Exec(ctx, q,
		a.ID, a.Phone, a.Email, a.DisplayName, a.FirstName, a.LastName, a.SLMCNumber, a.Specialty, languagesToStrings(a.Languages),
		a.LanguageOther, a.ExperienceYears, a.FeeCents, a.RequiredFeeCents, a.Bio,
		a.PGIMBoardCertified, a.MedicalSchool, a.QualificationsText, a.AvailabilityNotes,
		a.IsGeneralPractitioner, locs, a.TermsAcceptedAt,
		nullString(a.BankEncrypted), a.BankName, a.BankBranch, string(a.Status),
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

type ApplicationDocument struct {
	ID            uuid.UUID
	ApplicationID uuid.UUID
	DocumentType  DocumentType
	Filename      string
	ContentType   string
	Bytes         []byte
	UploadedAt    time.Time
}

func (r *Repository) UpsertApplicationDocument(ctx context.Context, tx pgx.Tx, d ApplicationDocument) error {
	const q = `
		INSERT INTO doctor_application_documents (
			id, application_id, document_type, filename, content_type, bytes, uploaded_at
		) VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (application_id, document_type) DO UPDATE SET
			filename = EXCLUDED.filename,
			content_type = EXCLUDED.content_type,
			bytes = EXCLUDED.bytes,
			uploaded_at = NOW()`
	id := d.ID
	if id == uuid.Nil {
		id = uuid.New()
	}
	_, err := tx.Exec(ctx, q, id, d.ApplicationID, string(d.DocumentType), d.Filename, d.ContentType, d.Bytes)
	if err != nil {
		return fmt.Errorf("doctor: upsert application document: %w", err)
	}
	return nil
}

func (r *Repository) ListApplicationDocuments(ctx context.Context, applicationID uuid.UUID, includeBytes bool) ([]ApplicationDocument, error) {
	cols := `id, application_id, document_type, filename, content_type, uploaded_at`
	if includeBytes {
		cols = `id, application_id, document_type, filename, content_type, bytes, uploaded_at`
	}
	q := `SELECT ` + cols + ` FROM doctor_application_documents WHERE application_id = $1 ORDER BY uploaded_at ASC`
	rows, err := r.pool.Query(ctx, q, applicationID)
	if err != nil {
		return nil, fmt.Errorf("doctor: list application documents: %w", err)
	}
	defer rows.Close()
	var out []ApplicationDocument
	for rows.Next() {
		var d ApplicationDocument
		var dtype string
		if includeBytes {
			if err := rows.Scan(&d.ID, &d.ApplicationID, &dtype, &d.Filename, &d.ContentType, &d.Bytes, &d.UploadedAt); err != nil {
				return nil, err
			}
		} else {
			if err := rows.Scan(&d.ID, &d.ApplicationID, &dtype, &d.Filename, &d.ContentType, &d.UploadedAt); err != nil {
				return nil, err
			}
		}
		d.DocumentType = DocumentType(dtype)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r *Repository) GetApplicationDocument(ctx context.Context, applicationID uuid.UUID, docType DocumentType) (ApplicationDocument, error) {
	const q = `
		SELECT id, application_id, document_type, filename, content_type, bytes, uploaded_at
		FROM doctor_application_documents
		WHERE application_id = $1 AND document_type = $2`
	var d ApplicationDocument
	var dtype string
	err := r.pool.QueryRow(ctx, q, applicationID, string(docType)).Scan(
		&d.ID, &d.ApplicationID, &dtype, &d.Filename, &d.ContentType, &d.Bytes, &d.UploadedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ApplicationDocument{}, ErrNotFound
		}
		return ApplicationDocument{}, fmt.Errorf("doctor: get application document: %w", err)
	}
	d.DocumentType = DocumentType(dtype)
	return d, nil
}
