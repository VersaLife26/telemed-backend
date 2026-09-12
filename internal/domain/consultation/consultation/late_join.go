package consultation

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
)

const (
	// LateJoinGrace is how long after scheduled_at the doctor waits and the
	// patient may still join for the first time. After this, auto no-show
	// fires if they never arrived. Later slots are not moved.
	LateJoinGrace = 10 * time.Minute

	// LateJoinCutoff is the hard stop on a first patient join. Same length as
	// the grace window: once it elapses, join is refused unless they already
	// made it into the waiting room.
	LateJoinCutoff = LateJoinGrace
)

// SweepPatientNoShow closes scheduled consults whose late-join window has
// passed with nobody arriving. It does not touch waiting or active visits,
// and it does not rewrite later slots.
func (s *Service) SweepPatientNoShow(ctx context.Context, now time.Time, batch int) (int, error) {
	if batch <= 0 {
		batch = DefaultRunningLateBatch
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	cutoff := now.Add(-LateJoinGrace)
	stale, err := s.store.ListScheduledPastJoinCutoff(ctx, s.pool, cutoff, batch)
	if err != nil {
		return 0, err
	}

	marked := 0
	for _, c := range stale {
		ok, err := s.markPatientNoShow(ctx, c, now)
		if err != nil {
			s.log.Error().Err(err).
				Str("consultation_id", c.ID.String()).
				Msg("patient no-show sweep failed")
			continue
		}
		if ok {
			marked++
		}
	}
	return marked, nil
}

func (s *Service) markPatientNoShow(ctx context.Context, c *Consultation, now time.Time) (bool, error) {
	if c.Status != StatusScheduled || c.DeletedAt != nil {
		return false, nil
	}

	c.Status = StatusAbandoned
	c.DeletedAt = &now
	reason := "no_show"
	c.EndReason = &reason

	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := s.store.UpdateConsultation(ctx, tx, c); err != nil {
			return err
		}
		if err := s.store.RecordEvent(ctx, tx, Event{
			ConsultationID: c.ID, Type: EventAbandoned, ActorIdentity: "system",
			Metadata: map[string]any{"reason": reason}, OccurredAt: now,
		}); err != nil {
			return err
		}
		return s.outbox.Enqueue(ctx, tx, events.SubjectConsultationPatientNoShow, c.ID.String(),
			events.ConsultationPatientNoShow{
				ConsultationID: c.ID,
				AppointmentID:  c.AppointmentID,
				PatientID:      c.PatientID,
				DoctorID:       c.DoctorID,
				ScheduledAt:    c.ScheduledAt,
				DetectedAt:     now,
			})
	})
	if err != nil {
		if errors.Is(err, ErrOptimisticLock) {
			return false, nil
		}
		return false, err
	}

	if err := s.video.EndRoom(ctx, c.RoomName); err != nil && !errors.Is(err, ErrRoomNotFound) {
		s.log.Warn().Err(err).Str("room", c.RoomName).Msg("end room after patient no-show failed")
	}
	return true, nil
}
