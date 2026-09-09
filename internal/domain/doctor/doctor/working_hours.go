package doctor

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// GetWorkingHours returns a doctor's declared weekly availability, ordered
// for display (Sunday..Saturday, then by start time).
func (r *Repository) GetWorkingHours(ctx context.Context, doctorID uuid.UUID) ([]WorkingHour, error) {
	const q = `
		SELECT id, doctor_id, day_of_week, start_time::text, end_time::text, is_available
		FROM working_hours
		WHERE doctor_id = $1
		ORDER BY day_of_week ASC, start_time ASC`

	rows, err := r.pool.Query(ctx, q, doctorID)
	if err != nil {
		return nil, fmt.Errorf("doctor: get working hours: %w", err)
	}
	defer rows.Close()

	var out []WorkingHour
	for rows.Next() {
		var w WorkingHour
		if err := rows.Scan(&w.ID, &w.DoctorID, &w.DayOfWeek, &w.StartTime, &w.EndTime, &w.IsAvailable); err != nil {
			return nil, fmt.Errorf("doctor: scan working hour: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("doctor: working hours rows: %w", err)
	}
	return out, nil
}

// ReplaceWorkingHours atomically replaces a doctor's entire weekly schedule.
// A full replace (delete + bulk insert) rather than a diff is deliberate:
// working hours have no independent identity a client can reference across
// calls, so "replace the set" is the only operation that cannot drift.
func (r *Repository) ReplaceWorkingHours(ctx context.Context, tx pgx.Tx, doctorID uuid.UUID, hours []WorkingHour) error {
	if _, err := tx.Exec(ctx, `DELETE FROM working_hours WHERE doctor_id = $1`, doctorID); err != nil {
		return fmt.Errorf("doctor: clear working hours: %w", err)
	}
	if len(hours) == 0 {
		return nil
	}

	rowsSrc := make([][]any, len(hours))
	for i, h := range hours {
		start, err := normalizeTimeOfDay(h.StartTime)
		if err != nil {
			return fmt.Errorf("doctor: working hours day %d start_time: %w", h.DayOfWeek, err)
		}
		end, err := normalizeTimeOfDay(h.EndTime)
		if err != nil {
			return fmt.Errorf("doctor: working hours day %d end_time: %w", h.DayOfWeek, err)
		}
		rowsSrc[i] = []any{uuid.New(), doctorID, h.DayOfWeek, start, end, h.IsAvailable}
	}
	_, err := tx.CopyFrom(ctx,
		pgx.Identifier{"working_hours"},
		[]string{"id", "doctor_id", "day_of_week", "start_time", "end_time", "is_available"},
		pgx.CopyFromRows(rowsSrc),
	)
	if err != nil {
		return fmt.Errorf("doctor: insert working hours: %w", err)
	}
	return nil
}

// normalizeTimeOfDay converts a wall-clock string to the form pgx can encode
// into a TIME column over the binary protocol.
//
// CopyFrom uses the binary protocol, and pgx v5 has an encode plan for
// "08:00:00" but none for "08:00" -- it fails with "cannot find encode plan",
// which surfaced as a 500 on every single availability save. The DTO accepts
// both forms, this repo's own test fixture uses the short one, and so does the
// doctor app, so the failure was total: no working hours meant doctor.approved
// carried no schedule, which meant zero slots generated and every approved
// doctor permanently unbookable.
//
// Normalising here rather than at the DTO keeps the storage concern where the
// storage constraint is: the API can go on accepting whatever doctors type.
func normalizeTimeOfDay(v string) (string, error) {
	for _, layout := range []string{"15:04:05", "15:04"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.Format("15:04:05"), nil
		}
	}
	return "", fmt.Errorf("%q is not a valid time of day (want HH:MM or HH:MM:SS)", v)
}
