package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
)

// Repository is the doctor domain's Postgres access layer. SQL only, no
// business rules, no *httpx.APIError -- those live in service.go and
// handler.go respectively.
type Repository struct {
	pool database.Pool
}

// NewRepository builds a Repository backed by the shared connection pool.
func NewRepository(pool database.Pool) *Repository { return &Repository{pool: pool} }

// ---------------------------------------------------------------------------
// Create / read
// ---------------------------------------------------------------------------

// Create inserts a new doctor profile. Must run inside the same transaction
// as the doctor.registered outbox write.
func (r *Repository) Create(ctx context.Context, tx pgx.Tx, d *Doctor) error {
	quals, err := json.Marshal(nilToEmptyQuals(d.Qualifications))
	if err != nil {
		return fmt.Errorf("doctor: marshal qualifications: %w", err)
	}

	const q = `
		INSERT INTO doctors (
			id, user_id, slmc_number, specialty, sub_specialties, experience_years, fee_cents,
			currency, display_name, languages, bio, photo_url, qualifications,
			verification_status, verified_at, verified_by, bank_encrypted, accepts_new_patients,
			created_at, updated_at, version
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7,
			$8, $9, $10, $11, $12, $13,
			$14, $15, $16, $17, $18,
			NOW(), NOW(), 0
		)`

	_, err = tx.Exec(ctx, q,
		d.ID, d.UserID, d.SLMCNumber, d.Specialty, nilToEmpty(d.SubSpecialties), d.ExperienceYears, d.FeeCents,
		defaultCurrency(d.Currency), d.DisplayName, languagesToStrings(d.Languages), d.Bio, d.PhotoURL, quals,
		string(d.VerificationStatus), d.VerifiedAt, d.VerifiedBy, nullString(d.BankEncrypted), d.AcceptsNewPatients,
	)
	if err != nil {
		if database.IsUniqueViolation(err) {
			return ErrSLMCTaken
		}
		return fmt.Errorf("doctor: create: %w", err)
	}
	return nil
}

const doctorColumns = `
	id, user_id, slmc_number, specialty, sub_specialties, experience_years, fee_cents,
	currency, display_name, languages, bio, photo_url, qualifications,
	verification_status, rejection_reason, verified_at, verified_by, bank_encrypted,
	rating, review_count, consultation_count, no_show_rate, accepts_new_patients,
	created_at, updated_at, deleted_at, version`

func scanDoctor(row pgx.Row) (Doctor, error) {
	var d Doctor
	var subSpecialties, languages []string
	var quals []byte
	var bankEncrypted, bio, photoURL, rejectionReason *string
	var verifiedAt, deletedAt *time.Time
	var verifiedBy *uuid.UUID
	var status string

	err := row.Scan(
		&d.ID, &d.UserID, &d.SLMCNumber, &d.Specialty, &subSpecialties, &d.ExperienceYears, &d.FeeCents,
		&d.Currency, &d.DisplayName, &languages, &bio, &photoURL, &quals,
		&status, &rejectionReason, &verifiedAt, &verifiedBy, &bankEncrypted,
		&d.Rating, &d.ReviewCount, &d.ConsultationCount, &d.NoShowRate, &d.AcceptsNewPatients,
		&d.CreatedAt, &d.UpdatedAt, &deletedAt, &d.Version,
	)
	if err != nil {
		return Doctor{}, err
	}

	d.VerificationStatus = VerificationStatus(status)
	d.SubSpecialties = subSpecialties
	d.Languages = stringsToLanguages(languages)
	d.BankEncrypted = deref(bankEncrypted)
	d.Bio = deref(bio)
	d.PhotoURL = deref(photoURL)
	d.RejectionReason = deref(rejectionReason)
	d.VerifiedAt = verifiedAt
	d.VerifiedBy = verifiedBy
	if len(quals) > 0 {
		if err := json.Unmarshal(quals, &d.Qualifications); err != nil {
			return Doctor{}, fmt.Errorf("doctor: unmarshal qualifications: %w", err)
		}
	}
	return d, nil
}

// GetByID returns one non-deleted doctor.
func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (Doctor, error) {
	q := fmt.Sprintf(`SELECT %s FROM doctors WHERE id = $1 AND deleted_at IS NULL`, doctorColumns)
	d, err := scanDoctor(r.pool.QueryRow(ctx, q, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Doctor{}, ErrNotFound
		}
		return Doctor{}, fmt.Errorf("doctor: get by id: %w", err)
	}
	return d, nil
}

// ListByIDs resolves many doctors in ONE query.
//
// It replaces a loop of GetByID calls in the gRPC batch method. `id = ANY($1)`
// is a single round trip and a single index scan, where the loop was one round
// trip per id and held a pool connection for the whole traversal -- so a large
// batch was a database outage rather than a slow response. Unknown and
// soft-deleted ids are simply absent from the result, which is the contract the
// batch method documents.
//
// The caller caps the length of ids. This function does not, deliberately: a
// repository that silently truncated a query would be a worse surprise than a
// slow one, and the cap belongs where the error can name itself.
func (r *Repository) ListByIDs(ctx context.Context, ids []uuid.UUID) ([]Doctor, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	q := fmt.Sprintf(`SELECT %s FROM doctors WHERE id = ANY($1) AND deleted_at IS NULL`, doctorColumns)
	rows, err := r.pool.Query(ctx, q, ids)
	if err != nil {
		return nil, fmt.Errorf("doctor: list by ids: %w", err)
	}
	defer rows.Close()

	out := make([]Doctor, 0, len(ids))
	for rows.Next() {
		d, err := scanDoctor(rows)
		if err != nil {
			return nil, fmt.Errorf("doctor: scan list by ids: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("doctor: list by ids rows: %w", err)
	}
	return out, nil
}

// GetByIDForUpdate locks the row FOR UPDATE inside tx, for the
// version-guarded update path.
func (r *Repository) GetByIDForUpdate(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Doctor, error) {
	q := fmt.Sprintf(`SELECT %s FROM doctors WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, doctorColumns)
	d, err := scanDoctor(tx.QueryRow(ctx, q, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Doctor{}, ErrNotFound
		}
		return Doctor{}, fmt.Errorf("doctor: get by id for update: %w", err)
	}
	return d, nil
}

// GetByUserID returns the doctor profile owned by a platform account, if any.
func (r *Repository) GetByUserID(ctx context.Context, userID uuid.UUID) (Doctor, error) {
	q := fmt.Sprintf(`SELECT %s FROM doctors WHERE user_id = $1 AND deleted_at IS NULL`, doctorColumns)
	d, err := scanDoctor(r.pool.QueryRow(ctx, q, userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Doctor{}, ErrNotFound
		}
		return Doctor{}, fmt.Errorf("doctor: get by user id: %w", err)
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// Update
// ---------------------------------------------------------------------------

// ProfileUpdate carries the mutable subset of a doctor's own profile. Fields
// an admin controls (verification_status, verified_at/by, rating,
// review_count, consultation_count) are deliberately absent -- a doctor
// cannot self-approve by PUTting their own profile.
type ProfileUpdate struct {
	Specialty          string
	SubSpecialties     []string
	ExperienceYears    int
	FeeCents           int64
	Languages          []Language
	Bio                string
	PhotoURL           string
	Qualifications     []Qualification
	AcceptsNewPatients bool
	BankEncrypted      *string // nil = leave unchanged
}

// Update applies u to the doctor identified by id, enforcing optimistic
// locking against expectedVersion. Must run inside tx, having already
// locked the row with GetByIDForUpdate in the same transaction.
func (r *Repository) Update(ctx context.Context, tx pgx.Tx, id uuid.UUID, expectedVersion int, u ProfileUpdate) error {
	quals, err := json.Marshal(nilToEmptyQuals(u.Qualifications))
	if err != nil {
		return fmt.Errorf("doctor: marshal qualifications: %w", err)
	}

	const q = `
		UPDATE doctors SET
			specialty = $1, sub_specialties = $2, experience_years = $3, fee_cents = $4,
			languages = $5, bio = $6, photo_url = $7, qualifications = $8,
			accepts_new_patients = $9,
			bank_encrypted = COALESCE($10, bank_encrypted),
			updated_at = NOW(), version = version + 1
		WHERE id = $11 AND version = $12 AND deleted_at IS NULL`

	tag, err := tx.Exec(ctx, q,
		u.Specialty, nilToEmpty(u.SubSpecialties), u.ExperienceYears, u.FeeCents,
		languagesToStrings(u.Languages), u.Bio, u.PhotoURL, quals,
		u.AcceptsNewPatients, u.BankEncrypted,
		id, expectedVersion,
	)
	if err != nil {
		return fmt.Errorf("doctor: update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrVersionConflict
	}
	return nil
}

// UpdateVerificationStatus performs one verification workflow transition.
// The transition legality check happens in service.go before this is
// called; this method's job is just to make the write atomic and
// version-safe.
func (r *Repository) UpdateVerificationStatus(
	ctx context.Context, tx pgx.Tx, id uuid.UUID, expectedVersion int,
	next VerificationStatus, reason string, actorID *uuid.UUID,
) error {
	// The rejected/approved conditions are passed in as plain booleans,
	// computed in Go, rather than as `$1 = 'rejected'` comparisons reusing
	// the same placeholder as the SET target -- Postgres's extended query
	// protocol infers one type per placeholder across the whole statement,
	// and a placeholder that is both a varchar assignment target and a
	// text comparison operand does not always resolve consistently
	// (42P08 "inconsistent types deduced for parameter"). Booleans sidestep
	// the ambiguity entirely.
	isRejected := next == StatusRejected
	isApproved := next == StatusApproved

	const q = `
		UPDATE doctors SET
			verification_status = $1,
			rejection_reason = CASE WHEN $2 THEN $3 ELSE rejection_reason END,
			verified_at = CASE WHEN $4 THEN NOW() ELSE verified_at END,
			verified_by = CASE WHEN $4 THEN $5 ELSE verified_by END,
			updated_at = NOW(), version = version + 1
		WHERE id = $6 AND version = $7 AND deleted_at IS NULL`

	tag, err := tx.Exec(ctx, q, string(next), isRejected, nullString(reason), isApproved, actorID, id, expectedVersion)
	if err != nil {
		return fmt.Errorf("doctor: update verification status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrVersionConflict
	}
	return nil
}

// IncrementConsultationCount is called from the appointment.completed
// consumer, guarded by the caller's idempotency check on appointment_id
// (see events_consumer.go).
func (r *Repository) IncrementConsultationCount(ctx context.Context, tx pgx.Tx, doctorID uuid.UUID) error {
	const q = `UPDATE doctors SET consultation_count = consultation_count + 1, updated_at = NOW(), version = version + 1 WHERE id = $1`
	if _, err := tx.Exec(ctx, q, doctorID); err != nil {
		return fmt.Errorf("doctor: increment consultation count: %w", err)
	}
	return nil
}

// RefreshReviewAggregate recomputes rating/review_count from published
// reviews and writes them onto the doctor row. This is the denormalisation
// step search depends on -- see docs/DESIGN.md.
func (r *Repository) RefreshReviewAggregate(ctx context.Context, tx pgx.Tx, doctorID uuid.UUID) error {
	const q = `
		UPDATE doctors SET
			rating = COALESCE((
				SELECT ROUND(AVG(rating)::numeric, 2) FROM reviews
				WHERE doctor_id = $1 AND is_published = TRUE AND deleted_at IS NULL
			), 0),
			review_count = (
				SELECT COUNT(*) FROM reviews
				WHERE doctor_id = $1 AND is_published = TRUE AND deleted_at IS NULL
			),
			updated_at = NOW()
		WHERE id = $1`
	if _, err := tx.Exec(ctx, q, doctorID); err != nil {
		return fmt.Errorf("doctor: refresh review aggregate: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Verification queue (internal, admin-service facing)
// ---------------------------------------------------------------------------

// PendingDoctor is a row of the verification queue: the doctor plus their
// uploaded documents, which is what an admin needs on one screen.
type PendingDoctor struct {
	Doctor    Doctor
	Documents []Document
}

// ListPending returns doctors awaiting a decision (pending or under_review),
// oldest first, paginated.
func (r *Repository) ListPending(ctx context.Context, limit, offset int) ([]Doctor, int64, error) {
	const q = `
		SELECT ` + doctorColumns + `
		FROM doctors
		WHERE verification_status IN ('pending', 'under_review') AND deleted_at IS NULL
		ORDER BY created_at ASC
		LIMIT $1 OFFSET $2`

	rows, err := r.pool.Query(ctx, q, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("doctor: list pending: %w", err)
	}
	defer rows.Close()

	var out []Doctor
	for rows.Next() {
		d, err := scanDoctor(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("doctor: scan pending: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("doctor: list pending rows: %w", err)
	}

	var total int64
	const countQ = `SELECT COUNT(*) FROM doctors WHERE verification_status IN ('pending', 'under_review') AND deleted_at IS NULL`
	if err := r.pool.QueryRow(ctx, countQ).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("doctor: count pending: %w", err)
	}
	return out, total, nil
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

// DefaultCurrency is the platform's only currency today. It exists as a
// constant rather than a literal so that adding a second one is a search, not
// an archaeology exercise.
const DefaultCurrency = "LKR"

// defaultCurrency fills in the currency for rows written before the column
// existed, and for callers that legitimately do not set it.
func defaultCurrency(c string) string {
	if c == "" {
		return DefaultCurrency
	}
	return c
}

func nilToEmpty(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

func nilToEmptyQuals(qs []Qualification) []Qualification {
	if qs == nil {
		return []Qualification{}
	}
	return qs
}

func languagesToStrings(ls []Language) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = string(l)
	}
	return out
}

func stringsToLanguages(ss []string) []Language {
	out := make([]Language, len(ss))
	for i, s := range ss {
		out[i] = Language(s)
	}
	return out
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
