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
	// DefaultBookedSlot is used when scheduled_end_at is missing. It matches
	// CreateConsultation's fallback so join and sweep agree on the window.
	DefaultBookedSlot = 15 * time.Minute

	// LateJoinGrace is retained for older tests and comments. First join is
	// allowed for the whole booked slot, not this grace window.
	LateJoinGrace = 10 * time.Minute

	// LateJoinCutoff is no longer a join stop; the booked slot end is.
	LateJoinCutoff = LateJoinGrace
)

func bookedSlotEnd(c *Consultation) time.Time {
	if c.ScheduledEndAt.After(c.ScheduledAt) {
		return c.ScheduledEndAt
	}
	return c.ScheduledAt.Add(DefaultBookedSlot)
}

// SweepPatientNoShow closes scheduled consults whose booked slot has ended
// with nobody arriving. It does not touch waiting or active visits, and it
// does not rewrite later slots. The appointment is closed as completed (the
// slot was allocated), not as a no-show.
func (s *Service) SweepPatientNoShow(ctx context.Context, now time.Time, batch int) (int, error) {
	if batch <= 0 {
		batch = DefaultRunningLateBatch
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	stale, err := s.store.ListScheduledPastJoinCutoff(ctx, s.pool, now, batch)
	if err != nil {
		return 0, err
	}

	marked := 0
	for _, c := range stale {
		ok, err := s.markPatientNoShow(ctx, c, now)
		if err != nil {
			s.log.Error().Err(err).
				Str("consultation_id", c.ID.String()).
				Msg("unused-slot sweep failed")
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
	reason := "slot_ended"
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
		s.log.Warn().Err(err).Str("room", c.RoomName).Msg("end room after unused slot closed failed")
	}
	return true, nil
}
