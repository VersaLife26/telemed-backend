package appointments

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
)

var ErrNotFound = errors.New("appointments: not found")

type Repository struct {
	pool database.Pool
}

func NewRepository(pool database.Pool) *Repository { return &Repository{pool: pool} }

func (r *Repository) Get(ctx context.Context, id uuid.UUID) (Appointment, error) {
	const q = `
		SELECT appointment_id, doctor_id, patient_id, specialty_code, district, status, scheduled_at, occurred_at
		FROM appointments_projection WHERE appointment_id = $1`
	a, err := scan(r.pool.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Appointment{}, ErrNotFound
	}
	return a, err
}

func (r *Repository) List(ctx context.Context, f ListFilter) ([]Appointment, int64, error) {
	where := "WHERE 1=1"
	args := []any{}
	if f.Status != "" {
		args = append(args, f.Status)
		where += fmt.Sprintf(" AND status = $%d", len(args))
	}
	if f.DoctorID != uuid.Nil {
		args = append(args, f.DoctorID)
		where += fmt.Sprintf(" AND doctor_id = $%d", len(args))
	}
	if f.PatientID != uuid.Nil {
		args = append(args, f.PatientID)
		where += fmt.Sprintf(" AND patient_id = $%d", len(args))
	}
	if !f.From.IsZero() {
		args = append(args, f.From)
		where += fmt.Sprintf(" AND scheduled_at >= $%d", len(args))
	}
	if !f.To.IsZero() {
		args = append(args, f.To)
		where += fmt.Sprintf(" AND scheduled_at <= $%d", len(args))
	}

	var total int64
	if err := r.pool.QueryRow(ctx, "SELECT COUNT(*) FROM appointments_projection "+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("appointments: count: %w", err)
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
		SELECT appointment_id, doctor_id, patient_id, specialty_code, district, status, scheduled_at, occurred_at
		FROM appointments_projection %s ORDER BY scheduled_at DESC NULLS LAST LIMIT $%d OFFSET $%d`,
		where, len(args)-1, len(args))

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("appointments: list: %w", err)
	}
	defer rows.Close()

	var out []Appointment
	for rows.Next() {
		a, err := scan(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, a)
	}
	return out, total, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scan(row rowScanner) (Appointment, error) {
	var a Appointment
	err := row.Scan(&a.AppointmentID, &a.DoctorID, &a.PatientID, &a.SpecialtyCode, &a.District, &a.Status, &a.ScheduledAt, &a.OccurredAt)
	if err != nil {
		return Appointment{}, fmt.Errorf("appointments: scan: %w", err)
	}
	return a, nil
}
