package doctor

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// GetScheduleSettings returns a doctor's slot-shape preferences, or the
// platform defaults if they have never saved the availability editor.
//
// Returning defaults rather than ErrNotFound is deliberate: every doctor has an
// effective schedule shape, and a caller that has to distinguish "no row" from
// "row with defaults" would end up duplicating the default in three places.
// BufferMinutes stays nil in the default, which is how "no preference" reaches
// the consumer as an absent field rather than a zero.
func (r *Repository) GetScheduleSettings(ctx context.Context, doctorID uuid.UUID) (ScheduleSettings, error) {
	return r.getScheduleSettings(ctx, r.pool, doctorID)
}

// GetScheduleSettingsTx is GetScheduleSettings inside a caller-supplied
// transaction, so an event can be built from the settings the same transaction
// just wrote.
func (r *Repository) GetScheduleSettingsTx(ctx context.Context, tx pgx.Tx, doctorID uuid.UUID) (ScheduleSettings, error) {
	return r.getScheduleSettings(ctx, tx, doctorID)
}

type rowQueryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (r *Repository) getScheduleSettings(ctx context.Context, q rowQueryer, doctorID uuid.UUID) (ScheduleSettings, error) {
	const sql = `
		SELECT slot_duration_minutes, buffer_minutes, max_per_day, timezone
		FROM doctor_schedule_settings
		WHERE doctor_id = $1`

	s := ScheduleSettings{DoctorID: doctorID}
	err := q.QueryRow(ctx, sql, doctorID).
		Scan(&s.SlotDurationMinutes, &s.BufferMinutes, &s.MaxPerDay, &s.Timezone)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultScheduleSettings(doctorID), nil
	}
	if err != nil {
		return ScheduleSettings{}, fmt.Errorf("doctor: get schedule settings: %w", err)
	}
	return s, nil
}

// UpsertScheduleSettings writes a doctor's slot-shape preferences. It must run
// in the same transaction as ReplaceWorkingHours and the doctor.updated outbox
// write: a schedule split across two commits is a schedule scheduling-service
// can read half of.
//
// buffer_minutes is written straight through, NULL included. Coalescing a nil
// to 0 here would erase the distinction the whole column exists to preserve.
func (r *Repository) UpsertScheduleSettings(ctx context.Context, tx pgx.Tx, s ScheduleSettings) error {
	const q = `
		INSERT INTO doctor_schedule_settings
			(doctor_id, slot_duration_minutes, buffer_minutes, max_per_day, timezone, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
		ON CONFLICT (doctor_id) DO UPDATE SET
			slot_duration_minutes = EXCLUDED.slot_duration_minutes,
			buffer_minutes        = EXCLUDED.buffer_minutes,
			max_per_day           = EXCLUDED.max_per_day,
			timezone              = EXCLUDED.timezone,
			updated_at            = NOW()`
	if _, err := tx.Exec(ctx, q, s.DoctorID, s.SlotDurationMinutes, s.BufferMinutes, s.MaxPerDay, s.Timezone); err != nil {
		return fmt.Errorf("doctor: upsert schedule settings: %w", err)
	}
	return nil
}
