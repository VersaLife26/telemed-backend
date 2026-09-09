package users

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
)

var ErrNotFound = errors.New("users: not found")

type Repository struct {
	pool database.Pool
}

func NewRepository(pool database.Pool) *Repository { return &Repository{pool: pool} }

func (r *Repository) UpsertFromRegistration(ctx context.Context, eventID uuid.UUID, p Registration) error {
	const q = `
		INSERT INTO user_projection (user_id, event_id, full_name, email, phone, role, status, registered_at)
		VALUES ($1,$2,$3,$4,$5,$6,'active',$7)
		ON CONFLICT (user_id) DO UPDATE SET
			event_id = EXCLUDED.event_id, full_name = EXCLUDED.full_name, email = EXCLUDED.email,
			phone = EXCLUDED.phone, role = EXCLUDED.role, updated_at = NOW()
		WHERE user_projection.event_id <> EXCLUDED.event_id`
	_, err := r.pool.Exec(ctx, q, p.UserID, eventID, p.FullName, p.Email, p.Phone, p.Role, p.RegisteredAt)
	if err != nil {
		return fmt.Errorf("users: upsert registration: %w", err)
	}
	return nil
}

func (r *Repository) SetStatus(ctx context.Context, eventID, userID uuid.UUID, status string) error {
	const q = `
		INSERT INTO user_projection (user_id, event_id, role, status)
		VALUES ($1, $2, 'patient', $3)
		ON CONFLICT (user_id) DO UPDATE SET
			event_id = EXCLUDED.event_id, status = EXCLUDED.status, updated_at = NOW()
		WHERE user_projection.event_id <> EXCLUDED.event_id`
	// role defaults to 'patient' only for the pathological case where a
	// status event arrives before the registration event ever did; the
	// subsequent registration upsert corrects it as soon as it lands.
	_, err := r.pool.Exec(ctx, q, userID, eventID, status)
	if err != nil {
		return fmt.Errorf("users: set status: %w", err)
	}
	return nil
}

func (r *Repository) Get(ctx context.Context, id uuid.UUID) (User, error) {
	const q = `SELECT user_id, full_name, email, phone, role, status, registered_at
	           FROM user_projection WHERE user_id = $1`
	u, err := scan(r.pool.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

func (r *Repository) List(ctx context.Context, f ListFilter) ([]User, int64, error) {
	where := "WHERE 1=1"
	args := []any{}
	if f.Query != "" {
		args = append(args, "%"+f.Query+"%")
		n := len(args)
		where += fmt.Sprintf(" AND (full_name ILIKE $%d OR email ILIKE $%d OR phone ILIKE $%d)", n, n, n)
	}
	if f.Role != "" {
		args = append(args, f.Role)
		where += fmt.Sprintf(" AND role = $%d", len(args))
	}
	if f.Status != "" {
		args = append(args, f.Status)
		where += fmt.Sprintf(" AND status = $%d", len(args))
	}

	var total int64
	if err := r.pool.QueryRow(ctx, "SELECT COUNT(*) FROM user_projection "+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("users: count: %w", err)
	}

	perPage := f.PerPage
	if perPage <= 0 {
		perPage = 20
	}
	page := f.Page
	if page <= 0 {
		page = 1
	}
	args = append(args, perPage, (page-1)*perPage)
	q := fmt.Sprintf(`SELECT user_id, full_name, email, phone, role, status, registered_at
	                   FROM user_projection %s ORDER BY registered_at DESC NULLS LAST LIMIT $%d OFFSET $%d`,
		where, len(args)-1, len(args))

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("users: list: %w", err)
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		u, err := scan(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, u)
	}
	return out, total, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scan(row rowScanner) (User, error) {
	var u User
	var registeredAt *time.Time
	err := row.Scan(&u.UserID, &u.FullName, &u.Email, &u.Phone, &u.Role, &u.Status, &registeredAt)
	if err != nil {
		return User{}, fmt.Errorf("users: scan: %w", err)
	}
	// registered_at is NULL only in the edge case where a suspend/reinstate
	// event for a not-yet-projected user arrives before its registration
	// event; the zero time renders as "unknown" rather than crashing the
	// scan.
	if registeredAt != nil {
		u.RegisteredAt = *registeredAt
	}
	return u, nil
}
