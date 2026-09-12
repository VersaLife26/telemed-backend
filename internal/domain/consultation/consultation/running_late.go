package consultation

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"

	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
)

const (
	// DefaultRunningLateInterval is how often we look for live consults that
	// have run past their booked end. One minute is frequent enough that the
	// next patient hears within a short wait, rare enough to stay cheap.
	DefaultRunningLateInterval = time.Minute

	// DefaultRunningLateBatch bounds work per tick.
	DefaultRunningLateBatch = 50

	runningLateLockKey = "consultation:sweep:running-late"
)

// SweepRunningLate finds active consults past scheduled_end_at and, once each,
// asks notification to tell the doctor's next waiting patient that the doctor
// is finishing with the previous visit. It does not move any slots.
func (s *Service) SweepRunningLate(ctx context.Context, now time.Time, batch int) (int, error) {
	if batch <= 0 {
		batch = DefaultRunningLateBatch
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	token, ok, err := s.cache.Lock(ctx, runningLateLockKey, 2*time.Minute)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil
	}
	defer func() {
		if err := s.cache.Unlock(ctx, runningLateLockKey, token); err != nil {
			s.log.Warn().Err(err).Msg("releasing the running-late sweep lease failed")
		}
	}()

	overrun, err := s.store.ListOverrunActive(ctx, s.pool, now, batch)
	if err != nil {
		return 0, err
	}

	notified := 0
	for _, active := range overrun {
		did, err := s.notifyNextPatientRunningLate(ctx, active, now)
		if err != nil {
			s.log.Error().Err(err).
				Str("consultation_id", active.ID.String()).
				Msg("running-late notify failed")
			continue
		}
		if did {
			notified++
		}
	}
	return notified, nil
}

func (s *Service) notifyNextPatientRunningLate(ctx context.Context, active *Consultation, now time.Time) (bool, error) {
	next, err := s.store.FindNextUpcomingForDoctor(ctx, s.pool, active.DoctorID, active.ScheduledAt)
	if errors.Is(err, ErrNotFound) {
		// Claim anyway so we do not re-query this active consult every minute
		// when nobody is waiting after it.
		return false, database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			_, claimErr := s.store.ClaimRunningLateNotified(ctx, tx, active.ID, now)
			return claimErr
		})
	}
	if err != nil {
		return false, err
	}

	minutesLate := int(now.Sub(active.ScheduledEndAt).Minutes())
	if minutesLate < 1 {
		minutesLate = 1
	}

	var claimed bool
	err = database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		ok, claimErr := s.store.ClaimRunningLateNotified(ctx, tx, active.ID, now)
		if claimErr != nil {
			return claimErr
		}
		if !ok {
			return nil
		}
		claimed = true
		return s.outbox.Enqueue(ctx, tx, events.SubjectConsultationDoctorRunningLate, active.ID.String(),
			events.ConsultationDoctorRunningLate{
				ActiveConsultationID: active.ID,
				ActiveAppointmentID:  active.AppointmentID,
				NextAppointmentID:    next.AppointmentID,
				NextPatientID:        next.PatientID,
				DoctorID:             active.DoctorID,
				MinutesLate:          minutesLate,
				DetectedAt:           now,
			})
	})
	if err != nil {
		return false, err
	}
	return claimed, nil
}

// RunningLateSweeper runs SweepRunningLate on a ticker.
type RunningLateSweeper struct {
	svc      *Service
	interval time.Duration
	batch    int
	log      zerolog.Logger
}

// NewRunningLateSweeper builds the worker, applying defaults for zero values.
func NewRunningLateSweeper(svc *Service, interval time.Duration, batch int, log zerolog.Logger) *RunningLateSweeper {
	if interval <= 0 {
		interval = DefaultRunningLateInterval
	}
	if batch <= 0 {
		batch = DefaultRunningLateBatch
	}
	return &RunningLateSweeper{svc: svc, interval: interval, batch: batch, log: log}
}

// Run sweeps until ctx is cancelled. Every replica runs; the Redis lease inside
// SweepRunningLate serialises the work.
func (w *RunningLateSweeper) Run(ctx context.Context) {
	w.log.Info().
		Dur("interval", w.interval).
		Msg("running-late consultation sweeper started")

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := w.svc.SweepRunningLate(ctx, time.Time{}, w.batch)
			if err != nil {
				w.log.Error().Err(err).Msg("running-late sweep failed")
			} else if n > 0 {
				w.log.Info().Int("notified", n).Msg("next patients notified that the doctor is running late")
			}
			absent, err := w.svc.SweepPatientNoShow(ctx, time.Time{}, w.batch)
			if err != nil {
				w.log.Error().Err(err).Msg("patient no-show sweep failed")
			} else if absent > 0 {
				w.log.Info().Int("marked", absent).Msg("late patients who never joined marked no-show")
			}
		}
	}
}
