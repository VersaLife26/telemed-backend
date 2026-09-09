package notifications

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
)

// Notification is one admin-console inbox row.
type Notification struct {
	ID         uuid.UUID
	Kind       string
	Title      string
	Body       string
	Href       string
	ResourceID *uuid.UUID
	ReadAt     *time.Time
	CreatedAt  time.Time
}

// Repository stores admin in-app notifications.
type Repository struct {
	pool database.Pool
}

func NewRepository(pool database.Pool) *Repository { return &Repository{pool: pool} }

// Insert creates a notification. Idempotent when resource_id+kind already exists unread.
func (r *Repository) Insert(ctx context.Context, n Notification) error {
	const q = `
		INSERT INTO admin_notifications (id, kind, title, body, href, resource_id, created_at)
		SELECT $1, $2, $3, $4, $5, $6, NOW()
		WHERE NOT EXISTS (
			SELECT 1 FROM admin_notifications
			WHERE kind = $2 AND resource_id IS NOT DISTINCT FROM $6 AND read_at IS NULL
		)`
	id := n.ID
	if id == uuid.Nil {
		id = uuid.New()
	}
	_, err := r.pool.Exec(ctx, q, id, n.Kind, n.Title, n.Body, n.Href, n.ResourceID)
	if err != nil {
		return fmt.Errorf("notifications: insert: %w", err)
	}
	return nil
}

// ListUnread returns recent unread notifications, newest first.
func (r *Repository) ListUnread(ctx context.Context, limit int) ([]Notification, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	const q = `
		SELECT id, kind, title, body, href, resource_id, read_at, created_at
		FROM admin_notifications
		WHERE read_at IS NULL
		ORDER BY created_at DESC
		LIMIT $1`
	rows, err := r.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("notifications: list: %w", err)
	}
	defer rows.Close()
	return scanNotifications(rows)
}

// CountUnread returns the unread badge count.
func (r *Repository) CountUnread(ctx context.Context) (int64, error) {
	const q = `SELECT COUNT(*) FROM admin_notifications WHERE read_at IS NULL`
	var n int64
	if err := r.pool.QueryRow(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("notifications: count: %w", err)
	}
	return n, nil
}

// MarkRead marks one notification read.
func (r *Repository) MarkRead(ctx context.Context, id uuid.UUID) error {
	const q = `UPDATE admin_notifications SET read_at = NOW() WHERE id = $1 AND read_at IS NULL`
	_, err := r.pool.Exec(ctx, q, id)
	return err
}

// MarkAllRead marks every unread notification read.
func (r *Repository) MarkAllRead(ctx context.Context) error {
	const q = `UPDATE admin_notifications SET read_at = NOW() WHERE read_at IS NULL`
	_, err := r.pool.Exec(ctx, q)
	return err
}

func scanNotifications(rows pgx.Rows) ([]Notification, error) {
	var out []Notification
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.Kind, &n.Title, &n.Body, &n.Href, &n.ResourceID, &n.ReadAt, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
