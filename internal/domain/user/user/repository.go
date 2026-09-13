package user

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"telemed/internal/platform/database"
)

// dbtx is satisfied by both *pgxpool.Pool (via database.Pool) and pgx.Tx, so
// every repository method works whether it is called standalone or inside a
// database.InTx block. A repository never returns an *httpx.APIError and
// never begins its own transaction -- the service layer owns that decision.
type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

var (
	_ dbtx = database.Pool(nil)
	_ dbtx = pgx.Tx(nil)
)

// ErrOptimisticLock is returned when an UPDATE's version predicate matched no
// row, meaning another writer changed the row first.
var ErrOptimisticLock = errors.New("user: optimistic lock conflict")

// ErrNotFound is returned by a lookup that found no row.
var ErrNotFound = errors.New("user: row not found")

// Repository is the SQL boundary for the user domain. It runs queries only;
// no business rule lives here.
type Repository struct {
	pool database.Pool
}

// NewRepository builds a Repository bound to the service's connection pool.
func NewRepository(pool database.Pool) *Repository { return &Repository{pool: pool} }

// Pool exposes the underlying pool so the service layer can open
// transactions with database.InTx without the repository needing to know
// about transaction lifecycle.
func (r *Repository) Pool() database.Pool { return r.pool }

// ------------------------------------------------------------------ users --

const userColumns = `id, phone, email, name, nic_hash, nic_hash_version, language, role, status,
	no_show_count, keycloak_id, google_sub, email_verified_at, erasure_due_at, anonymized_at,
	created_at, updated_at, deleted_at, version, address, date_of_birth, photo_updated_at`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	var phone *string
	err := row.Scan(&u.ID, &phone, &u.Email, &u.Name, &u.NICHash, &u.NICHashVersion, &u.Language, &u.Role, &u.Status,
		&u.NoShowCount, &u.KeycloakID, &u.GoogleSub, &u.EmailVerifiedAt, &u.ErasureDueAt, &u.AnonymizedAt,
		&u.CreatedAt, &u.UpdatedAt, &u.DeletedAt, &u.Version, &u.Address, &u.DateOfBirth, &u.PhotoUpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("user: scan user: %w", err)
	}
	if phone != nil {
		u.Phone = *phone
	}
	return &u, nil
}

func mapUniqueViolation(err error) error {
	if !database.IsUniqueViolation(err) {
		return nil
	}
	switch database.UniqueConstraint(err) {
	case "idx_users_email_active":
		return ErrEmailTaken
	case "idx_users_google_sub_active":
		return ErrGoogleTaken
	default:
		return ErrPhoneTaken
	}
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// FindUserByPhone looks up an active (non-deleted) user by E.164 phone.
func (r *Repository) FindUserByPhone(ctx context.Context, tx dbtx, phone string) (*User, error) {
	const q = `SELECT ` + userColumns + ` FROM users WHERE phone = $1 AND deleted_at IS NULL`
	return scanUser(tx.QueryRow(ctx, q, phone))
}

// FindUserByEmail looks up an active user by normalised email.
func (r *Repository) FindUserByEmail(ctx context.Context, tx dbtx, email string) (*User, error) {
	const q = `SELECT ` + userColumns + ` FROM users WHERE email = $1 AND deleted_at IS NULL`
	return scanUser(tx.QueryRow(ctx, q, email))
}

// FindUserByGoogleSub looks up an active user by Google's stable subject.
func (r *Repository) FindUserByGoogleSub(ctx context.Context, tx dbtx, sub string) (*User, error) {
	const q = `SELECT ` + userColumns + ` FROM users WHERE google_sub = $1 AND deleted_at IS NULL`
	return scanUser(tx.QueryRow(ctx, q, sub))
}

// GetPasswordHash returns the bcrypt hash for a user, or "" when they have
// none (OTP-only or Google-only accounts). Missing users are ErrUserNotFound.
func (r *Repository) GetPasswordHash(ctx context.Context, tx dbtx, id uuid.UUID) (string, error) {
	const q = `SELECT password_hash FROM users WHERE id = $1`
	var hash *string
	err := tx.QueryRow(ctx, q, id).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUserNotFound
	}
	if err != nil {
		return "", fmt.Errorf("user: get password hash: %w", err)
	}
	if hash == nil {
		return "", nil
	}
	return *hash, nil
}

// LinkGoogle records a Google subject on an existing account, typically when
// the user first uses Google after registering by email or phone.
func (r *Repository) LinkGoogle(ctx context.Context, tx dbtx, userID uuid.UUID, sub string, verifiedAt time.Time) error {
	const q = `
		UPDATE users SET google_sub = $1, email_verified_at = COALESCE(email_verified_at, $2),
		    updated_at = NOW(), version = version + 1
		WHERE id = $3 AND deleted_at IS NULL`
	tag, err := tx.Exec(ctx, q, sub, verifiedAt, userID)
	if mapped := mapUniqueViolation(err); mapped != nil {
		return mapped
	}
	if err != nil {
		return fmt.Errorf("user: link google: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// SetPasswordHash stores a bcrypt hash. Empty hash is refused by the service
// layer before this is called.
func (r *Repository) SetPasswordHash(ctx context.Context, tx dbtx, userID uuid.UUID, hash string) error {
	const q = `UPDATE users SET password_hash = $1, updated_at = NOW(), version = version + 1 WHERE id = $2 AND deleted_at IS NULL`
	tag, err := tx.Exec(ctx, q, hash, userID)
	if err != nil {
		return fmt.Errorf("user: set password: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// FindUserByID looks up a user regardless of deleted state, so an internal
// caller (e.g. the erasure reaper, or an admin lookup) can still find it.
func (r *Repository) FindUserByID(ctx context.Context, tx dbtx, id uuid.UUID) (*User, error) {
	const q = `SELECT ` + userColumns + ` FROM users WHERE id = $1`
	return scanUser(tx.QueryRow(ctx, q, id))
}

// FindUsersByIDs resolves many ids in one round trip, backing the gRPC
// GetUsersBatch call. Unknown ids are simply absent from the result.
func (r *Repository) FindUsersByIDs(ctx context.Context, tx dbtx, ids []uuid.UUID) ([]User, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	const q = `SELECT ` + userColumns + ` FROM users WHERE id = ANY($1) AND deleted_at IS NULL`
	rows, err := tx.Query(ctx, q, ids)
	if err != nil {
		return nil, fmt.Errorf("user: find users by ids: %w", err)
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("user: find users by ids rows: %w", err)
	}
	return out, nil
}

// CreateUser inserts a new identity and populates the generated fields.
func (r *Repository) CreateUser(ctx context.Context, tx dbtx, u *User) error {
	const q = `
		INSERT INTO users (phone, email, name, nic_hash, nic_hash_version, language, role, status, keycloak_id, password_hash, google_sub, email_verified_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF($10, ''), $11, $12)
		RETURNING id, no_show_count, created_at, updated_at, version`
	err := tx.QueryRow(ctx, q, nilIfEmpty(u.Phone), u.Email, u.Name, u.NICHash, u.NICHashVersion, u.Language, u.Role, u.Status, u.KeycloakID, u.PasswordHash, u.GoogleSub, u.EmailVerifiedAt).
		Scan(&u.ID, &u.NoShowCount, &u.CreatedAt, &u.UpdatedAt, &u.Version)
	if mapped := mapUniqueViolation(err); mapped != nil {
		return mapped
	}
	if err != nil {
		return fmt.Errorf("user: create user: %w", err)
	}
	return nil
}

// UpdateProfile updates the mutable self-service fields, guarded by the
// optimistic-lock version column.
func (r *Repository) UpdateProfile(ctx context.Context, tx dbtx, u *User) error {
	const q = `
		UPDATE users SET name = $1, email = $2, phone = $3, language = $4, address = $5,
		    date_of_birth = $6, updated_at = NOW(), version = version + 1
		WHERE id = $7 AND version = $8 AND deleted_at IS NULL
		RETURNING updated_at, version`
	err := tx.QueryRow(ctx, q, u.Name, u.Email, nilIfEmpty(u.Phone), u.Language, u.Address, u.DateOfBirth, u.ID, u.Version).
		Scan(&u.UpdatedAt, &u.Version)
	if mapped := mapUniqueViolation(err); mapped != nil {
		return mapped
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrOptimisticLock
	}
	if err != nil {
		return fmt.Errorf("user: update profile: %w", err)
	}
	return nil
}

// ProfilePhoto is the raw avatar payload. It is loaded only by the photo
// download path so ordinary profile / directory reads stay cheap.
type ProfilePhoto struct {
	Data        []byte
	ContentType string
	UpdatedAt   time.Time
}

// GetProfilePhoto returns the stored avatar, or ErrNotFound when none is set.
func (r *Repository) GetProfilePhoto(ctx context.Context, tx dbtx, userID uuid.UUID) (*ProfilePhoto, error) {
	const q = `
		SELECT photo_data, photo_content_type, photo_updated_at
		FROM users
		WHERE id = $1 AND deleted_at IS NULL AND photo_data IS NOT NULL`
	var photo ProfilePhoto
	err := tx.QueryRow(ctx, q, userID).Scan(&photo.Data, &photo.ContentType, &photo.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("user: get profile photo: %w", err)
	}
	return &photo, nil
}

// SetProfilePhoto replaces the caller's avatar bytes.
func (r *Repository) SetProfilePhoto(ctx context.Context, tx dbtx, userID uuid.UUID, data []byte, contentType string) (time.Time, error) {
	const q = `
		UPDATE users
		SET photo_data = $2, photo_content_type = $3, photo_updated_at = NOW(),
		    updated_at = NOW(), version = version + 1
		WHERE id = $1 AND deleted_at IS NULL
		RETURNING photo_updated_at`
	var updatedAt time.Time
	err := tx.QueryRow(ctx, q, userID, data, contentType).Scan(&updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("user: set profile photo: %w", err)
	}
	return updatedAt, nil
}

// ClearProfilePhoto removes the caller's avatar.
func (r *Repository) ClearProfilePhoto(ctx context.Context, tx dbtx, userID uuid.UUID) error {
	const q = `
		UPDATE users
		SET photo_data = NULL, photo_content_type = NULL, photo_updated_at = NULL,
		    updated_at = NOW(), version = version + 1
		WHERE id = $1 AND deleted_at IS NULL`
	tag, err := tx.Exec(ctx, q, userID)
	if err != nil {
		return fmt.Errorf("user: clear profile photo: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetKeycloakID records the mirrored Keycloak identity once the (best-effort,
// non-blocking) provisioning call succeeds.
func (r *Repository) SetKeycloakID(ctx context.Context, tx dbtx, userID uuid.UUID, keycloakID string) error {
	const q = `UPDATE users SET keycloak_id = $1, updated_at = NOW() WHERE id = $2`
	_, err := tx.Exec(ctx, q, keycloakID, userID)
	if err != nil {
		return fmt.Errorf("user: set keycloak id: %w", err)
	}
	return nil
}

// FindUserByIDForUpdate is FindUserByID with a row lock, so an administrative
// status change and the concurrent login that would read the old status
// serialise instead of racing.
func (r *Repository) FindUserByIDForUpdate(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*User, error) {
	const q = `SELECT ` + userColumns + ` FROM users WHERE id = $1 FOR UPDATE`
	return scanUser(tx.QueryRow(ctx, q, id))
}

// SetStatus applies an administrative account-status change.
//
// It is deliberately NOT version-guarded, unlike UpdateProfile. This is not a
// user editing their own row against a form they loaded earlier -- it is an
// administrator stopping an account, arriving as an at-least-once event that
// may be redelivered. Failing it on a version mismatch would leave the account
// running, which is the wrong way for a suspension to fail. Serialisation
// comes from FindUserByIDForUpdate's row lock instead, and the service refuses
// the transition before it gets here if the account is already in the target
// state.
//
// deleted_at is untouched: suspension and PDPA deletion are different
// lifecycles, and a suspended account is still an account.
func (r *Repository) SetStatus(ctx context.Context, tx dbtx, id uuid.UUID, status Status) error {
	const q = `
		UPDATE users SET status = $2, updated_at = NOW(), version = version + 1
		WHERE id = $1 AND deleted_at IS NULL`
	tag, err := tx.Exec(ctx, q, id, string(status))
	if err != nil {
		return fmt.Errorf("user: set status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetRole promotes or demotes an account role (e.g. patient -> doctor after
// an approved application completes OTP). Not version-guarded: it is driven
// by the OTP path after phone proof, not by a stale self-service form.
func (r *Repository) SetRole(ctx context.Context, tx dbtx, id uuid.UUID, role Role) error {
	const q = `
		UPDATE users SET role = $2, updated_at = NOW(), version = version + 1
		WHERE id = $1 AND deleted_at IS NULL`
	tag, err := tx.Exec(ctx, q, id, string(role))
	if err != nil {
		return fmt.Errorf("user: set role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SoftDeleteUser marks the account deleted and schedules PDPA erasure.
func (r *Repository) SoftDeleteUser(ctx context.Context, tx dbtx, id uuid.UUID, erasureDueAt time.Time) error {
	const q = `
		UPDATE users
		SET status = 'deleted', deleted_at = NOW(), erasure_due_at = $2, updated_at = NOW(), version = version + 1
		WHERE id = $1 AND deleted_at IS NULL`
	tag, err := tx.Exec(ctx, q, id, erasureDueAt)
	if err != nil {
		return fmt.Errorf("user: soft delete user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// AnonymizeDueUsers scrubs PII for accounts whose erasure grace period has
// passed. It is idempotent: a row already anonymised is excluded, so calling
// it repeatedly (e.g. on every reaper tick) is safe.
func (r *Repository) AnonymizeDueUsers(ctx context.Context, tx dbtx, now time.Time, limit int) (int64, error) {
	const q = `
		UPDATE users
		SET name = '[erased]', email = NULL, nic_hash = NULL, nic_hash_version = NULL,
		    phone = 'erased:' || id::text, password_hash = NULL, google_sub = NULL, email_verified_at = NULL,
		    address = '', date_of_birth = NULL,
		    photo_data = NULL, photo_content_type = NULL, photo_updated_at = NULL,
		    anonymized_at = NOW(), updated_at = NOW(), version = version + 1
		WHERE id IN (
			SELECT id FROM users
			WHERE status = 'deleted' AND erasure_due_at IS NOT NULL AND erasure_due_at <= $1 AND anonymized_at IS NULL
			ORDER BY erasure_due_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)`
	tag, err := tx.Exec(ctx, q, now, limit)
	if err != nil {
		return 0, fmt.Errorf("user: anonymize due users: %w", err)
	}
	return tag.RowsAffected(), nil
}

// -------------------------------------------------------------- family_members --

const familyColumns = `id, owner_user_id, name, dob, relation, nic_hash, nic_hash_version, created_at, updated_at, deleted_at, version`

func scanFamilyMember(row pgx.Row) (*FamilyMember, error) {
	var f FamilyMember
	err := row.Scan(&f.ID, &f.OwnerUserID, &f.Name, &f.DOB, &f.Relation, &f.NICHash, &f.NICHashVersion,
		&f.CreatedAt, &f.UpdatedAt, &f.DeletedAt, &f.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrFamilyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("user: scan family member: %w", err)
	}
	return &f, nil
}

// CreateFamilyMember inserts a dependant profile.
func (r *Repository) CreateFamilyMember(ctx context.Context, tx dbtx, f *FamilyMember) error {
	const q = `
		INSERT INTO family_members (owner_user_id, name, dob, relation, nic_hash, nic_hash_version)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at, updated_at, version`
	err := tx.QueryRow(ctx, q, f.OwnerUserID, f.Name, f.DOB, f.Relation, f.NICHash, f.NICHashVersion).
		Scan(&f.ID, &f.CreatedAt, &f.UpdatedAt, &f.Version)
	if err != nil {
		return fmt.Errorf("user: create family member: %w", err)
	}
	return nil
}

// ListFamilyMembers returns every active dependant owned by ownerID.
func (r *Repository) ListFamilyMembers(ctx context.Context, tx dbtx, ownerID uuid.UUID) ([]FamilyMember, error) {
	const q = `SELECT ` + familyColumns + ` FROM family_members
		WHERE owner_user_id = $1 AND deleted_at IS NULL ORDER BY created_at`
	rows, err := tx.Query(ctx, q, ownerID)
	if err != nil {
		return nil, fmt.Errorf("user: list family members: %w", err)
	}
	defer rows.Close()

	var out []FamilyMember
	for rows.Next() {
		f, err := scanFamilyMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("user: list family members rows: %w", err)
	}
	return out, nil
}

// GetFamilyMember fetches one dependant by id, regardless of owner -- the
// service layer is responsible for the ownership check, which is what the
// authorisation test (user A must not read user B's family) exercises.
func (r *Repository) GetFamilyMember(ctx context.Context, tx dbtx, id uuid.UUID) (*FamilyMember, error) {
	const q = `SELECT ` + familyColumns + ` FROM family_members WHERE id = $1 AND deleted_at IS NULL`
	return scanFamilyMember(tx.QueryRow(ctx, q, id))
}

// UpdateFamilyMember updates a dependant's editable fields under the
// optimistic-lock version column.
func (r *Repository) UpdateFamilyMember(ctx context.Context, tx dbtx, f *FamilyMember) error {
	const q = `
		UPDATE family_members SET name = $1, dob = $2, relation = $3, nic_hash = $4, nic_hash_version = $5,
		       updated_at = NOW(), version = version + 1
		WHERE id = $6 AND version = $7 AND deleted_at IS NULL
		RETURNING updated_at, version`
	err := tx.QueryRow(ctx, q, f.Name, f.DOB, f.Relation, f.NICHash, f.NICHashVersion, f.ID, f.Version).
		Scan(&f.UpdatedAt, &f.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrOptimisticLock
	}
	if err != nil {
		return fmt.Errorf("user: update family member: %w", err)
	}
	return nil
}

// SoftDeleteFamilyMember removes a dependant. ownerID is included in the
// WHERE clause as a second authorisation layer beneath the service-level
// check -- defence in depth against a future call site that forgets it.
func (r *Repository) SoftDeleteFamilyMember(ctx context.Context, tx dbtx, id, ownerID uuid.UUID) error {
	const q = `
		UPDATE family_members SET deleted_at = NOW(), updated_at = NOW(), version = version + 1
		WHERE id = $1 AND owner_user_id = $2 AND deleted_at IS NULL`
	tag, err := tx.Exec(ctx, q, id, ownerID)
	if err != nil {
		return fmt.Errorf("user: soft delete family member: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrFamilyNotFound
	}
	return nil
}

// ---------------------------------------------------------------- otp_attempts --

// InsertOTPAttempt records one line of the abuse-investigation audit trail.
func (r *Repository) InsertOTPAttempt(ctx context.Context, tx dbtx, a OTPAttempt) error {
	const q = `INSERT INTO otp_attempts (phone, purpose, action, ip, success) VALUES ($1, $2, $3, $4, $5)`
	if _, err := tx.Exec(ctx, q, a.Phone, a.Purpose, a.Action, a.IP, a.Success); err != nil {
		return fmt.Errorf("user: insert otp attempt: %w", err)
	}
	return nil
}

// ------------------------------------------------------------- refresh_tokens --

// CreateRefreshToken inserts a new session token row.
func (r *Repository) CreateRefreshToken(ctx context.Context, tx dbtx, rt *RefreshToken) error {
	const q = `
		INSERT INTO refresh_tokens (user_id, family_id, token_hash, device_id_hash, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, issued_at`
	err := tx.QueryRow(ctx, q, rt.UserID, rt.FamilyID, rt.TokenHash, rt.DeviceIDHash, rt.ExpiresAt).
		Scan(&rt.ID, &rt.IssuedAt)
	if err != nil {
		return fmt.Errorf("user: create refresh token: %w", err)
	}
	return nil
}

const refreshColumns = `id, user_id, family_id, token_hash, device_id_hash, issued_at, expires_at, revoked_at, replaced_by`

func scanRefreshToken(row pgx.Row) (*RefreshToken, error) {
	var rt RefreshToken
	err := row.Scan(&rt.ID, &rt.UserID, &rt.FamilyID, &rt.TokenHash, &rt.DeviceIDHash,
		&rt.IssuedAt, &rt.ExpiresAt, &rt.RevokedAt, &rt.ReplacedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("user: scan refresh token: %w", err)
	}
	return &rt, nil
}

// FindRefreshTokenByHashForUpdate locks the row for the duration of the
// caller's transaction, so a concurrent rotation attempt on the same token
// serialises instead of racing.
func (r *Repository) FindRefreshTokenByHashForUpdate(ctx context.Context, tx pgx.Tx, hash string) (*RefreshToken, error) {
	const q = `SELECT ` + refreshColumns + ` FROM refresh_tokens WHERE token_hash = $1 FOR UPDATE`
	return scanRefreshToken(tx.QueryRow(ctx, q, hash))
}

// FindRefreshTokenByHash reads without locking, for the logout path where no
// further write depends on the read being stable.
func (r *Repository) FindRefreshTokenByHash(ctx context.Context, tx dbtx, hash string) (*RefreshToken, error) {
	const q = `SELECT ` + refreshColumns + ` FROM refresh_tokens WHERE token_hash = $1`
	return scanRefreshToken(tx.QueryRow(ctx, q, hash))
}

// RevokeRefreshToken marks one token used/revoked, optionally recording the
// token that replaced it (rotation) -- replacedBy is nil on a plain logout.
func (r *Repository) RevokeRefreshToken(ctx context.Context, tx dbtx, id uuid.UUID, replacedBy *uuid.UUID) error {
	const q = `UPDATE refresh_tokens SET revoked_at = NOW(), replaced_by = $2, updated_at = NOW()
		WHERE id = $1 AND revoked_at IS NULL`
	_, err := tx.Exec(ctx, q, id, replacedBy)
	if err != nil {
		return fmt.Errorf("user: revoke refresh token: %w", err)
	}
	return nil
}

// RevokeFamily revokes every still-active token descended from the same
// login. Called when rotation detects reuse of an already-rotated token --
// the standard response to a stolen refresh token is to burn the entire
// session lineage, not just the one token that was replayed.
func (r *Repository) RevokeFamily(ctx context.Context, tx dbtx, familyID uuid.UUID) (int64, error) {
	const q = `UPDATE refresh_tokens SET revoked_at = NOW(), updated_at = NOW()
		WHERE family_id = $1 AND revoked_at IS NULL`
	tag, err := tx.Exec(ctx, q, familyID)
	if err != nil {
		return 0, fmt.Errorf("user: revoke family: %w", err)
	}
	return tag.RowsAffected(), nil
}

// RevokeAllForUser implements logout-all: every active session for the user
// is revoked in one statement.
func (r *Repository) RevokeAllForUser(ctx context.Context, tx dbtx, userID uuid.UUID) (int64, error) {
	const q = `UPDATE refresh_tokens SET revoked_at = NOW(), updated_at = NOW()
		WHERE user_id = $1 AND revoked_at IS NULL`
	tag, err := tx.Exec(ctx, q, userID)
	if err != nil {
		return 0, fmt.Errorf("user: revoke all for user: %w", err)
	}
	return tag.RowsAffected(), nil
}

// -------------------------------------------------------------------- consents --

// InsertConsent appends one entry to a user's consent ledger. Consents are
// never updated or deleted -- a withdrawal is a new row with granted=false,
// which is what lets the platform answer "what had this user agreed to, and
// when" for a PDPA/GDPR audit.
func (r *Repository) InsertConsent(ctx context.Context, tx dbtx, c *Consent) error {
	const q = `
		INSERT INTO consents (user_id, kind, version, granted)
		VALUES ($1, $2, $3, $4)
		RETURNING id, created_at`
	err := tx.QueryRow(ctx, q, c.UserID, c.Kind, c.Version, c.Granted).Scan(&c.ID, &c.CreatedAt)
	if err != nil {
		return fmt.Errorf("user: insert consent: %w", err)
	}
	return nil
}
