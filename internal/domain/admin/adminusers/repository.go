package adminusers

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
)

// ErrVersionConflict is returned when an UPDATE's optimistic-lock predicate
// (version = $expected) matches zero rows: either the account does not
// exist, or someone else changed it since the caller last read it.
var ErrVersionConflict = errors.New("adminusers: version conflict")

// ErrNotFound mirrors sql.ErrNoRows without leaking the driver type.
var ErrNotFound = errors.New("adminusers: not found")

type Repository struct {
	pool database.Pool
}

func NewRepository(pool database.Pool) *Repository { return &Repository{pool: pool} }

// EnsureByKeycloakSubject upserts the admin_users row for a verified
// identity: insert on first sight, refresh email/display name/last_login_at
// on every subsequent call. role is only ever set on INSERT -- an existing
// admin's role is changed explicitly via Update, never silently overwritten
// by whatever role happened to be primary on this particular token.
func (r *Repository) EnsureByKeycloakSubject(ctx context.Context, subject, email, displayName, role string) (AdminUser, error) {
	const q = `
		INSERT INTO admin_users (keycloak_subject, email, display_name, role, last_login_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (keycloak_subject) WHERE deleted_at IS NULL DO UPDATE SET
			email = EXCLUDED.email,
			display_name = CASE WHEN EXCLUDED.display_name <> '' THEN EXCLUDED.display_name ELSE admin_users.display_name END,
			last_login_at = NOW(),
			updated_at = NOW()
		RETURNING id, keycloak_subject, email, display_name, role, ip_allowlist, active, last_login_at, created_at, updated_at, version`

	row := r.pool.QueryRow(ctx, q, subject, email, displayName, role)
	return scanOne(row)
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (AdminUser, error) {
	const q = `
		SELECT id, keycloak_subject, email, display_name, role, ip_allowlist, active, last_login_at, created_at, updated_at, version
		FROM admin_users WHERE id = $1 AND deleted_at IS NULL`
	row := r.pool.QueryRow(ctx, q, id)
	u, err := scanOne(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminUser{}, ErrNotFound
	}
	return u, err
}

// List returns every active-or-not admin account, newest first. The admin
// console is expected to run at a headcount of dozens, not thousands, so
// this is unpaginated by design -- see AGENT-BRIEF's own numbers ("10
// admins").
func (r *Repository) List(ctx context.Context) ([]AdminUser, error) {
	const q = `
		SELECT id, keycloak_subject, email, display_name, role, ip_allowlist, active, last_login_at, created_at, updated_at, version
		FROM admin_users WHERE deleted_at IS NULL ORDER BY created_at DESC`
	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("adminusers: list: %w", err)
	}
	defer rows.Close()

	var out []AdminUser
	for rows.Next() {
		u, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Update applies an optimistic-locked partial update and returns the new
// row. ErrVersionConflict signals the caller should re-fetch and retry (or,
// for an HTTP handler, return 409).
func (r *Repository) Update(ctx context.Context, id uuid.UUID, p UpdateParams) (AdminUser, error) {
	const q = `
		UPDATE admin_users SET
			active       = COALESCE($3, active),
			role         = COALESCE($4, role),
			ip_allowlist = COALESCE($5, ip_allowlist),
			updated_at   = NOW(),
			version      = version + 1
		WHERE id = $1 AND version = $2 AND deleted_at IS NULL
		RETURNING id, keycloak_subject, email, display_name, role, ip_allowlist, active, last_login_at, created_at, updated_at, version`

	row := r.pool.QueryRow(ctx, q, id, p.Version, p.Active, p.Role, p.IPAllowlist)
	u, err := scanOne(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminUser{}, ErrVersionConflict
	}
	return u, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanOne(row rowScanner) (AdminUser, error) {
	var u AdminUser
	err := row.Scan(&u.ID, &u.KeycloakSubject, &u.Email, &u.DisplayName, &u.Role,
		&u.IPAllowlist, &u.Active, &u.LastLoginAt, &u.CreatedAt, &u.UpdatedAt, &u.Version)
	if err != nil {
		return AdminUser{}, fmt.Errorf("adminusers: scan: %w", err)
	}
	return u, nil
}

func scanRow(rows pgx.Rows) (AdminUser, error) { return scanOne(rows) }

// Create inserts a new admin account already linked to its Keycloak subject.
// The caller provisions the login first: a row without a subject is an admin
// nobody can sign in as, and the unique index on keycloak_subject means the
// database, not the application, is what guarantees one row per identity.
func (r *Repository) Create(ctx context.Context, p CreateParams) (AdminUser, error) {
	const q = `
		INSERT INTO admin_users (keycloak_subject, email, display_name, role, ip_allowlist)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, keycloak_subject, email, display_name, role, ip_allowlist, active, last_login_at, created_at, updated_at, version`

	allowlist := p.IPAllowlist
	if allowlist == nil {
		allowlist = []string{}
	}
	row := r.pool.QueryRow(ctx, q, p.KeycloakSubject, p.Email, p.DisplayName, p.Role, allowlist)
	return scanOne(row)
}

// DeleteHard removes a row outright. It exists only to unwind a failed
// Create -- everything a user does is soft-deleted.
func (r *Repository) DeleteHard(ctx context.Context, id uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM admin_users WHERE id = $1`, id)
	return err
}
