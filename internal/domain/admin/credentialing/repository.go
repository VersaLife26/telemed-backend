package credentialing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
)

var ErrNotFound = errors.New("credentialing: not found")

type Repository struct {
	pool database.Pool
}

func NewRepository(pool database.Pool) *Repository { return &Repository{pool: pool} }

// UpsertDoctorFromEvent applies a doctor.registered event to the local
// projection. It is idempotent: redelivery of the same event_id, or an
// out-of-order delivery of an older event for a doctor already projected
// from a newer one, is a no-op (per the platform contract that every
// consumer be idempotent on envelope.ID -- AGENT-BRIEF section 3).
func (r *Repository) UpsertDoctorFromEvent(ctx context.Context, eventID uuid.UUID, d DoctorSummary) error {
	// keepNonBlank is why the document keys are not plain EXCLUDED
	// assignments. doctor.registered is published at registration, before any
	// credential has been uploaded, so its key set is normally empty. A
	// redelivery of that event after doctor.documents_updated has filled the
	// row must not blank the reviewer's documents -- and at-least-once
	// delivery makes that redelivery a certainty, not a hypothetical.
	const q = `
		INSERT INTO doctor_projection
			(doctor_id, event_id, full_name, email, phone, slmc_number, years_experience,
			 specialty_code, fee_cents, slmc_certificate_key, nic_document_key, degree_certificate_key,
			 photo_key, registered_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (doctor_id) DO UPDATE SET
			event_id = EXCLUDED.event_id,
			full_name = EXCLUDED.full_name,
			email = EXCLUDED.email,
			phone = EXCLUDED.phone,
			slmc_number = EXCLUDED.slmc_number,
			years_experience = EXCLUDED.years_experience,
			specialty_code = EXCLUDED.specialty_code,
			fee_cents = COALESCE(EXCLUDED.fee_cents, doctor_projection.fee_cents),
			slmc_certificate_key = COALESCE(NULLIF(EXCLUDED.slmc_certificate_key, ''), doctor_projection.slmc_certificate_key),
			nic_document_key = COALESCE(NULLIF(EXCLUDED.nic_document_key, ''), doctor_projection.nic_document_key),
			degree_certificate_key = COALESCE(NULLIF(EXCLUDED.degree_certificate_key, ''), doctor_projection.degree_certificate_key),
			photo_key = COALESCE(NULLIF(EXCLUDED.photo_key, ''), doctor_projection.photo_key),
			updated_at = NOW()
		WHERE doctor_projection.event_id <> $2`
	_, err := r.pool.Exec(ctx, q, d.DoctorID, eventID, d.FullName, d.Email, d.Phone, d.SLMCNumber,
		d.YearsExperience, d.SpecialtyCode, nullInt64(d.FeeCents), d.SLMCCertificateKey, d.NICDocumentKey,
		d.DegreeCertificateKey, d.PhotoKey, d.RegisteredAt)
	if err != nil && database.IsUniqueViolation(err) && d.SLMCNumber != "" {
		// A rejected projection still held the SLMC under the old global unique
		// index (and may until migrate 000012). Free it so a re-apply can land.
		_, freeErr := r.pool.Exec(ctx, `
			UPDATE doctor_projection
			SET slmc_number = slmc_number || '#rejected:' || doctor_id::text,
			    updated_at = NOW()
			WHERE slmc_number = $1
			  AND verification_status = 'rejected'
			  AND doctor_id <> $2`, d.SLMCNumber, d.DoctorID)
		if freeErr == nil {
			_, err = r.pool.Exec(ctx, q, d.DoctorID, eventID, d.FullName, d.Email, d.Phone, d.SLMCNumber,
				d.YearsExperience, d.SpecialtyCode, nullInt64(d.FeeCents), d.SLMCCertificateKey, d.NICDocumentKey,
				d.DegreeCertificateKey, d.PhotoKey, d.RegisteredAt)
		}
	}
	if err != nil {
		return fmt.Errorf("credentialing: upsert doctor projection: %w", err)
	}
	return nil
}

// ApplyDocumentKeys applies a doctor.documents_updated event to the
// projection's four credential document keys.
//
// updatedAt is the event's own timestamp and acts as the ordering guard: the
// write lands only when it is strictly newer than whatever documents event was
// applied last. That makes a duplicate delivery a no-op (JetStream guarantees
// at-least-once, not exactly-once) and stops a redelivery arriving out of
// order from rolling the reviewer's document set backwards to an older,
// smaller one.
//
// A doctor with no projection row yet is not an error: doctor.registered and
// doctor.documents_updated are independent subjects with independent
// consumers, so the upload event can genuinely win the race. The next
// registered delivery fills the row, and its COALESCE above preserves the keys
// this write would have set. Reporting "0 rows" here as a failure would spin
// the consumer on a redelivery loop that cannot succeed.
func (r *Repository) ApplyDocumentKeys(ctx context.Context, doctorID uuid.UUID, d DoctorSummary, updatedAt time.Time) error {
	const q = `
		UPDATE doctor_projection SET
			slmc_certificate_key = $2,
			nic_document_key = $3,
			degree_certificate_key = $4,
			photo_key = $5,
			documents_updated_at = $6,
			updated_at = NOW()
		WHERE doctor_id = $1
		  AND (documents_updated_at IS NULL OR documents_updated_at < $6)`
	_, err := r.pool.Exec(ctx, q, doctorID,
		d.SLMCCertificateKey, d.NICDocumentKey, d.DegreeCertificateKey, d.PhotoKey, updatedAt)
	if err != nil {
		return fmt.Errorf("credentialing: apply document keys for %s: %w", doctorID, err)
	}
	return nil
}

func (r *Repository) ListPending(ctx context.Context, page, perPage int) ([]DoctorSummary, int64, error) {
	offset := (page - 1) * perPage

	var total int64
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM doctor_projection WHERE verification_status = 'pending'`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("credentialing: count pending: %w", err)
	}

	const q = `
		SELECT doctor_id, full_name, email, phone, slmc_number, years_experience, specialty_code,
		       verification_status, slmc_certificate_key, nic_document_key, degree_certificate_key,
		       photo_key, registered_at
		FROM doctor_projection
		WHERE verification_status = 'pending'
		ORDER BY registered_at ASC
		LIMIT $1 OFFSET $2`
	rows, err := r.pool.Query(ctx, q, perPage, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("credentialing: list pending: %w", err)
	}
	defer rows.Close()

	out, err := scanDoctors(rows)
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func (r *Repository) GetDoctor(ctx context.Context, id uuid.UUID) (DoctorSummary, error) {
	const q = `
		SELECT doctor_id, full_name, email, phone, slmc_number, years_experience, specialty_code,
		       verification_status, slmc_certificate_key, nic_document_key, degree_certificate_key,
		       photo_key, registered_at
		FROM doctor_projection WHERE doctor_id = $1`
	d, err := scanDoctor(r.pool.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return DoctorSummary{}, ErrNotFound
	}
	return d, err
}

// GetOrCreatePendingChecklist returns the current pending checklist for a
// doctor, creating an empty one if none exists (first time an admin opens
// this doctor in the queue). The partial unique index on
// (doctor_id) WHERE overall_status = 'pending' makes the INSERT safe under
// concurrent first-opens: the loser's ON CONFLICT falls through to the
// SELECT.
func (r *Repository) GetOrCreatePendingChecklist(ctx context.Context, doctorID uuid.UUID) (Checklist, error) {
	const insertQ = `
		INSERT INTO verification_checklists (doctor_id)
		VALUES ($1)
		ON CONFLICT (doctor_id) WHERE overall_status = 'pending' DO NOTHING`
	if _, err := r.pool.Exec(ctx, insertQ, doctorID); err != nil {
		return Checklist{}, fmt.Errorf("credentialing: ensure checklist: %w", err)
	}

	const selectQ = `
		SELECT id, doctor_id, slmc_format_valid, slmc_format_checked_by, slmc_format_checked_at,
		       slmc_registry_checked, slmc_registry_checked_by, slmc_registry_checked_at,
		       experience_verified, experience_checked_by, experience_checked_at,
		       nic_matches, nic_checked_by, nic_checked_at,
		       photo_clear, photo_checked_by, photo_checked_at,
		       overall_status, decision_reason, decided_by, decided_at,
		       created_at, updated_at, version
		FROM verification_checklists
		WHERE doctor_id = $1 AND overall_status = 'pending'
		ORDER BY created_at DESC LIMIT 1`
	return scanChecklist(r.pool.QueryRow(ctx, selectQ, doctorID))
}

// GetLatestChecklist is a read-only lookup for GET /doctors/{id}: the most
// recent checklist for a doctor, whatever its status. Unlike
// GetOrCreatePendingChecklist it never inserts a row, so a GET request never
// has a write side effect.
func (r *Repository) GetLatestChecklist(ctx context.Context, doctorID uuid.UUID) (Checklist, error) {
	const q = `
		SELECT id, doctor_id, slmc_format_valid, slmc_format_checked_by, slmc_format_checked_at,
		       slmc_registry_checked, slmc_registry_checked_by, slmc_registry_checked_at,
		       experience_verified, experience_checked_by, experience_checked_at,
		       nic_matches, nic_checked_by, nic_checked_at,
		       photo_clear, photo_checked_by, photo_checked_at,
		       overall_status, decision_reason, decided_by, decided_at,
		       created_at, updated_at, version
		FROM verification_checklists
		WHERE doctor_id = $1
		ORDER BY created_at DESC LIMIT 1`
	c, err := scanChecklist(r.pool.QueryRow(ctx, q, doctorID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Checklist{}, ErrNotFound
	}
	return c, err
}

// UpdateChecklistItems applies a partial update to the checklist items,
// each stamped with the checker and the time it was checked.
func (r *Repository) UpdateChecklistItems(ctx context.Context, checklistID uuid.UUID, u ChecklistUpdate) (Checklist, error) {
	const q = `
		UPDATE verification_checklists SET
			slmc_format_valid = COALESCE($2, slmc_format_valid),
			slmc_format_checked_by = CASE WHEN $2 IS NOT NULL THEN $6 ELSE slmc_format_checked_by END,
			slmc_format_checked_at = CASE WHEN $2 IS NOT NULL THEN NOW() ELSE slmc_format_checked_at END,

			slmc_registry_checked = COALESCE($3, slmc_registry_checked),
			slmc_registry_checked_by = CASE WHEN $3 IS NOT NULL THEN $6 ELSE slmc_registry_checked_by END,
			slmc_registry_checked_at = CASE WHEN $3 IS NOT NULL THEN NOW() ELSE slmc_registry_checked_at END,

			experience_verified = COALESCE($4, experience_verified),
			experience_checked_by = CASE WHEN $4 IS NOT NULL THEN $6 ELSE experience_checked_by END,
			experience_checked_at = CASE WHEN $4 IS NOT NULL THEN NOW() ELSE experience_checked_at END,

			nic_matches = COALESCE($5, nic_matches),
			nic_checked_by = CASE WHEN $5 IS NOT NULL THEN $6 ELSE nic_checked_by END,
			nic_checked_at = CASE WHEN $5 IS NOT NULL THEN NOW() ELSE nic_checked_at END,

			photo_clear = COALESCE($7, photo_clear),
			photo_checked_by = CASE WHEN $7 IS NOT NULL THEN $6 ELSE photo_checked_by END,
			photo_checked_at = CASE WHEN $7 IS NOT NULL THEN NOW() ELSE photo_checked_at END,

			updated_at = NOW(),
			version = version + 1
		WHERE id = $1 AND overall_status = 'pending'
		RETURNING id, doctor_id, slmc_format_valid, slmc_format_checked_by, slmc_format_checked_at,
		       slmc_registry_checked, slmc_registry_checked_by, slmc_registry_checked_at,
		       experience_verified, experience_checked_by, experience_checked_at,
		       nic_matches, nic_checked_by, nic_checked_at,
		       photo_clear, photo_checked_by, photo_checked_at,
		       overall_status, decision_reason, decided_by, decided_at,
		       created_at, updated_at, version`

	row := r.pool.QueryRow(ctx, q, checklistID, u.SLMCFormatValid, u.SLMCRegistryChecked,
		u.ExperienceVerified, u.NICMatches, u.CheckerID, u.PhotoClear)
	c, err := scanChecklist(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Checklist{}, ErrNotFound
	}
	return c, err
}

// Decide records the approve/reject decision on the pending checklist and
// flips the local projection's verification_status in the same
// transaction the caller (service.Verify) also uses to enqueue the outbox
// event -- so the checklist, the projection, and the published event either
// all happen or none do.
func (r *Repository) Decide(ctx context.Context, tx pgx.Tx, doctorID uuid.UUID, d VerifyDecision) (Checklist, error) {
	status := "rejected"
	if d.Approve {
		status = "approved"
	}

	const updateChecklist = `
		UPDATE verification_checklists SET
			overall_status = $2,
			decision_reason = $3,
			decided_by = $4,
			decided_at = NOW(),
			updated_at = NOW(),
			version = version + 1
		WHERE id = (
			SELECT id FROM verification_checklists
			WHERE doctor_id = $1 AND overall_status = 'pending'
			ORDER BY created_at DESC LIMIT 1
		)
		RETURNING id, doctor_id, slmc_format_valid, slmc_format_checked_by, slmc_format_checked_at,
		       slmc_registry_checked, slmc_registry_checked_by, slmc_registry_checked_at,
		       experience_verified, experience_checked_by, experience_checked_at,
		       nic_matches, nic_checked_by, nic_checked_at,
		       photo_clear, photo_checked_by, photo_checked_at,
		       overall_status, decision_reason, decided_by, decided_at,
		       created_at, updated_at, version`
	row := tx.QueryRow(ctx, updateChecklist, doctorID, status, d.Reason, d.DeciderID)
	c, err := scanChecklist(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Checklist{}, ErrNotFound
	}
	if err != nil {
		return Checklist{}, err
	}

	const updateProjection = `UPDATE doctor_projection SET verification_status = $2, updated_at = NOW() WHERE doctor_id = $1`
	if _, err := tx.Exec(ctx, updateProjection, doctorID, status); err != nil {
		return Checklist{}, fmt.Errorf("credentialing: update doctor projection status: %w", err)
	}

	return c, nil
}

// SetVerificationStatus updates the projected status outside the checklist Decide path
// (used when doctor.application_approved/rejected arrives from doctor-service).
func (r *Repository) SetVerificationStatus(ctx context.Context, doctorID uuid.UUID, status string) error {
	const q = `UPDATE doctor_projection SET verification_status = $2, updated_at = NOW() WHERE doctor_id = $1`
	_, err := r.pool.Exec(ctx, q, doctorID, status)
	if err != nil {
		return fmt.Errorf("credentialing: set verification status: %w", err)
	}
	return nil
}

func scanDoctors(rows pgx.Rows) ([]DoctorSummary, error) {
	var out []DoctorSummary
	for rows.Next() {
		d, err := scanDoctor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scanDoctor(row rowScanner) (DoctorSummary, error) {
	var d DoctorSummary
	err := row.Scan(&d.DoctorID, &d.FullName, &d.Email, &d.Phone, &d.SLMCNumber, &d.YearsExperience,
		&d.SpecialtyCode, &d.VerificationStatus, &d.SLMCCertificateKey, &d.NICDocumentKey,
		&d.DegreeCertificateKey, &d.PhotoKey, &d.RegisteredAt)
	if err != nil {
		return DoctorSummary{}, fmt.Errorf("credentialing: scan doctor: %w", err)
	}
	return d, nil
}

func scanChecklist(row rowScanner) (Checklist, error) {
	var c Checklist
	var reason *string
	err := row.Scan(&c.ID, &c.DoctorID, &c.SLMCFormatValid, &c.SLMCFormatCheckedBy, &c.SLMCFormatCheckedAt,
		&c.SLMCRegistryChecked, &c.SLMCRegistryCheckedBy, &c.SLMCRegistryCheckedAt,
		&c.ExperienceVerified, &c.ExperienceCheckedBy, &c.ExperienceCheckedAt,
		&c.NICMatches, &c.NICCheckedBy, &c.NICCheckedAt,
		&c.PhotoClear, &c.PhotoCheckedBy, &c.PhotoCheckedAt,
		&c.OverallStatus, &reason, &c.DecidedBy, &c.DecidedAt,
		&c.CreatedAt, &c.UpdatedAt, &c.Version)
	if err != nil {
		return Checklist{}, fmt.Errorf("credentialing: scan checklist: %w", err)
	}
	if reason != nil {
		c.DecisionReason = *reason
	}
	return c, nil
}

func nullInt64(n int64) *int64 {
	if n == 0 {
		return nil
	}
	return &n
}
