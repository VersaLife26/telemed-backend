package disputes

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
)

var ErrNotFound = errors.New("disputes: not found")
var ErrVersionConflict = errors.New("disputes: version conflict")

// ErrNotAssignee is returned when an admin tries to act on a dispute that is
// assigned to someone else. The row has modelled an assignee since
// migrations/000003 and nothing enforced it (security review F23): the
// resolution of a dispute decides whether a patient gets money back, so
// "whoever clicks first" is not an access control.
var ErrNotAssignee = errors.New("disputes: dispute is assigned to another admin")

type Repository struct {
	pool database.Pool
}

func NewRepository(pool database.Pool) *Repository { return &Repository{pool: pool} }

func (r *Repository) Create(ctx context.Context, p CreateParams) (Dispute, error) {
	const q = `
		INSERT INTO disputes (appointment_id, patient_id, doctor_id, category, description, refund_requested)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id, appointment_id, patient_id, doctor_id, category, description, status,
		          assigned_to, resolution, refund_requested, refund_amount_cents, currency,
		          created_at, updated_at, version`
	row := r.pool.QueryRow(ctx, q, p.AppointmentID, p.PatientID, p.DoctorID, p.Category, p.Description, p.RefundRequested)
	return scan(row)
}

func (r *Repository) Get(ctx context.Context, id uuid.UUID) (Dispute, error) {
	const q = `
		SELECT id, appointment_id, patient_id, doctor_id, category, description, status,
		       assigned_to, resolution, refund_requested, refund_amount_cents, currency,
		       created_at, updated_at, version
		FROM disputes WHERE id = $1 AND deleted_at IS NULL`
	d, err := scan(r.pool.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Dispute{}, ErrNotFound
	}
	return d, err
}

func (r *Repository) List(ctx context.Context, f ListFilter) ([]Dispute, int64, error) {
	where := "WHERE deleted_at IS NULL"
	args := []any{}
	if f.Status != "" {
		args = append(args, f.Status)
		where += fmt.Sprintf(" AND status = $%d", len(args))
	}
	if f.AssignedTo != uuid.Nil {
		args = append(args, f.AssignedTo)
		where += fmt.Sprintf(" AND assigned_to = $%d", len(args))
	}

	var total int64
	if err := r.pool.QueryRow(ctx, "SELECT COUNT(*) FROM disputes "+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("disputes: count: %w", err)
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

	q := fmt.Sprintf(`
		SELECT id, appointment_id, patient_id, doctor_id, category, description, status,
		       assigned_to, resolution, refund_requested, refund_amount_cents, currency,
		       created_at, updated_at, version
		FROM disputes %s ORDER BY created_at DESC LIMIT $%d OFFSET $%d`, where, len(args)-1, len(args))

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("disputes: list: %w", err)
	}
	defer rows.Close()

	var out []Dispute
	for rows.Next() {
		d, err := scan(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, d)
	}
	return out, total, rows.Err()
}

// Assign moves a dispute to assignee on behalf of actor.
//
// The `assigned_to IS NULL OR assigned_to = $4` predicate is the race-safe half
// of the assignee rule; the service checks the same condition first so it can
// return a meaningful 403 rather than a bare conflict. Doing it only in the
// service would leave a window in which two admins both read an unassigned
// dispute and both claim it. force lifts the predicate for a super_admin --
// see Service.Assign for why that escape hatch has to exist.
func (r *Repository) Assign(ctx context.Context, id, assignee, actor uuid.UUID, version int, force bool) (Dispute, error) {
	const q = `
		UPDATE disputes SET assigned_to = $3, status = CASE WHEN status = 'open' THEN 'investigating' ELSE status END,
		       updated_at = NOW(), version = version + 1
		WHERE id = $1 AND version = $2 AND deleted_at IS NULL
		  AND ($5 OR assigned_to IS NULL OR assigned_to = $4)
		RETURNING id, appointment_id, patient_id, doctor_id, category, description, status,
		          assigned_to, resolution, refund_requested, refund_amount_cents, currency,
		          created_at, updated_at, version`
	row := r.pool.QueryRow(ctx, q, id, version, assignee, actor, force)
	d, err := scan(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Dispute{}, ErrVersionConflict
	}
	return d, err
}

// Resolve closes a dispute on behalf of actor, optionally awarding a refund.
//
// Same predicate as Assign, and for the same reason: the row carries an
// assignee, and until this predicate existed nothing anywhere enforced it --
// any of the five admin roles could resolve any dispute, with an arbitrary
// refund_amount_cents (security review F23).
func (r *Repository) Resolve(ctx context.Context, id uuid.UUID, resolution string, refundAmountCents *int64, actor uuid.UUID, version int, force bool) (Dispute, error) {
	const q = `
		UPDATE disputes SET status = 'resolved', resolution = $3, refund_amount_cents = COALESCE($4, refund_amount_cents),
		       updated_at = NOW(), version = version + 1
		WHERE id = $1 AND version = $2 AND deleted_at IS NULL
		  AND ($6 OR assigned_to IS NULL OR assigned_to = $5)
		RETURNING id, appointment_id, patient_id, doctor_id, category, description, status,
		          assigned_to, resolution, refund_requested, refund_amount_cents, currency,
		          created_at, updated_at, version`
	row := r.pool.QueryRow(ctx, q, id, version, resolution, refundAmountCents, actor, force)
	d, err := scan(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Dispute{}, ErrVersionConflict
	}
	return d, err
}

func (r *Repository) AddComment(ctx context.Context, disputeID, authorID uuid.UUID, body string) (Comment, error) {
	const q = `
		INSERT INTO dispute_comments (dispute_id, author_admin_id, body)
		VALUES ($1, $2, $3)
		RETURNING id, dispute_id, author_admin_id, body, created_at`
	var c Comment
	err := r.pool.QueryRow(ctx, q, disputeID, authorID, body).
		Scan(&c.ID, &c.DisputeID, &c.AuthorAdminID, &c.Body, &c.CreatedAt)
	if err != nil {
		return Comment{}, fmt.Errorf("disputes: add comment: %w", err)
	}
	return c, nil
}

func (r *Repository) ListComments(ctx context.Context, disputeID uuid.UUID) ([]Comment, error) {
	const q = `
		SELECT id, dispute_id, author_admin_id, body, created_at
		FROM dispute_comments WHERE dispute_id = $1 AND deleted_at IS NULL ORDER BY created_at ASC`
	rows, err := r.pool.Query(ctx, q, disputeID)
	if err != nil {
		return nil, fmt.Errorf("disputes: list comments: %w", err)
	}
	defer rows.Close()

	var out []Comment
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ID, &c.DisputeID, &c.AuthorAdminID, &c.Body, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("disputes: scan comment: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scan(row rowScanner) (Dispute, error) {
	var d Dispute
	var resolution *string
	err := row.Scan(&d.ID, &d.AppointmentID, &d.PatientID, &d.DoctorID, &d.Category, &d.Description,
		&d.Status, &d.AssignedTo, &resolution, &d.RefundRequested, &d.RefundAmountCents, &d.Currency,
		&d.CreatedAt, &d.UpdatedAt, &d.Version)
	if err != nil {
		return Dispute{}, fmt.Errorf("disputes: scan: %w", err)
	}
	if resolution != nil {
		d.Resolution = *resolution
	}
	return d, nil
}
