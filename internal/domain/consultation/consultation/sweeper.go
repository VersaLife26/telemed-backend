package consultation

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// --- why this exists --------------------------------------------------------
//
// Three paths reach endInternal: the HTTP End handler, appointment.cancelled
// teardown, and LiveKit's room_finished webhook. The first two need somebody
// to press something. The third is the only thing that closes the case both
// clients died on -- a patient whose phone battery went flat while the doctor's
// laptop lid closed -- and it is a webhook: it is lost whenever LiveKit cannot
// reach us, whenever we are mid-deploy, whenever the signature does not check
// out, and entirely when VIDEO_PROVIDER is not livekit.
//
// A consultation stuck in 'active' is not a cosmetic problem:
//
//   - consultation.ended is never published, so scheduling never sees
//     appointment.completed or no_show, and the appointment never settles.
//   - record-service writes treating_relationships.ended_at from that event.
//     Without it the treating window is anchored on started_at, so the
//     30-day access clock runs from a time that is not the end of care.
//   - The LiveKit room stays open, the recording egress never stops, and the
//     recording is never finalised.
//   - duration_seconds is never written, so the doctor's rolling average --
//     which is what tells the next patient how long they will wait -- silently
//     stops learning from that consultation.
//
// --- why it is careful ------------------------------------------------------
//
// Auto-ending a call has clinical consequences: it destroys the LiveKit room,
// so a premature sweep drops a live consultation. Two rules follow.
//
// 1. The database cannot tell a live call from an abandoned one on its own.
//    Once both parties have joined, a healthy 90-minute consultation writes no
//    participant rows at all; its only heartbeat is the periodic quality
//    sample. So "quiet in the database" is a candidate filter, never a verdict.
//
// 2. The verdict comes from the video provider, which is the only component
//    that knows who is actually connected right now. A provider error is NOT
//    evidence of an empty room -- it is most likely the very outage that lost
//    the webhook -- so an error means skip and try again next tick. Failing to
//    sweep costs one more interval. Ending a live consultation costs a
//    consultation.
//
// --- the threshold ----------------------------------------------------------
//
// DefaultIdleTimeout is 2 hours, chosen against four numbers:
//
//   - LiveKit's own empty-room teardown is LIVEKIT_EMPTY_ROOM_TIMEOUT, 300s by
//     default. On a healthy platform the room_finished webhook already ends an
//     emptied consultation about five minutes after the last participant
//     leaves. The sweeper must never race that, or it becomes the mechanism
//     rather than the backstop, and its far cruder evidence starts making the
//     decisions.
//   - A mobile reconnect. A patient handing over from wifi to a Sri Lankan 4G
//     cell is routinely gone for 30-120 seconds, and the room is briefly empty
//     if both parties reconnect at once.
//   - A doctor finishing their notes with the room idle, which the brief
//     rightly flags. Worth being precise about what that costs: clinical notes
//     live in record-service and are authorised by the treating relationship,
//     not by the consultation being open (clinicalnotes.authoriseWrite has no
//     time bound and explicitly does not require consultation.ended), so
//     ending the consultation does not stop the doctor writing. What it does
//     take away is their live video session. Twenty minutes is a generous
//     bound on that; two hours is four times it.
//   - The longest plausible consultation. The platform's own default estimate
//     is 15 minutes. A two-hour telemedicine consultation is not a thing, but
//     because rule 1 above makes the provider the decider, being wrong in this
//     direction costs nothing but a wasted API call.
//
// Two hours is also short enough that a stuck row clears inside one
// operational shift, so scheduling and payment see the outcome the same day
// rather than never.
//
// The ended_at we write is the last EVIDENCE of activity, not the sweep time.
// Dating a swept consultation at "now" would report a 9-hour consultation to
// analytics and to the doctor's rolling average because a webhook was lost
// overnight.

const (
	// DefaultIdleTimeout is how quiet a consultation must be before the
	// provider is asked about it. See the reasoning above.
	DefaultIdleTimeout = 2 * time.Hour

	// DefaultSweepInterval is how often the sweeper looks. Frequent enough
	// that a stuck row is caught promptly, rare enough that it is nothing on
	// a partial index over a handful of active rows.
	DefaultSweepInterval = 5 * time.Minute

	// DefaultSweepBatch bounds the work one tick may do, so a backlog after an
	// outage drains over several ticks instead of one long transaction storm.
	DefaultSweepBatch = 50

	// EndReasonStale is the end_reason written by the sweeper. It is
	// deliberately distinct from every human and webhook reason so these are
	// greppable, alertable, and never mistaken for a clean end.
	EndReasonStale = "stale_swept"

	// sweepLockKey serialises the sweep across replicas. The cron-in-every-pod
	// shape is what produced the payout batch race (SECURITY-REVIEW F19); this
	// one takes a lease rather than repeating it.
	sweepLockKey = "consultation:sweep:stale-active"
)

// StaleCandidate is one 'active' consultation whose local timeline has gone
// quiet. It is a candidate, not a verdict.
type StaleCandidate struct {
	ID           uuid.UUID
	RoomName     string
	StartedAt    time.Time
	LastActivity time.Time
}

// SweepStaleActive ends consultations that are still 'active' but that the
// video provider says nobody is in. It returns how many it ended.
//
// Safe to call concurrently from every replica: a Redis lease means only one
// does the work, and endInternal is a no-op against an already-terminal row.
func (s *Service) SweepStaleActive(ctx context.Context, idleFor time.Duration, batch int) (int, error) {
	if idleFor <= 0 {
		idleFor = DefaultIdleTimeout
	}
	if batch <= 0 {
		batch = DefaultSweepBatch
	}

	token, ok, err := s.cache.Lock(ctx, sweepLockKey, 2*time.Minute)
	if err != nil {
		return 0, err
	}
	if !ok {
		// Another replica holds the lease. Not an error.
		return 0, nil
	}
	defer func() {
		if err := s.cache.Unlock(ctx, sweepLockKey, token); err != nil {
			s.log.Warn().Err(err).Msg("releasing the stale-consultation sweep lease failed")
		}
	}()

	cutoff := time.Now().UTC().Add(-idleFor)
	candidates, err := s.store.ListStaleActive(ctx, s.pool, cutoff, batch)
	if err != nil {
		return 0, err
	}

	swept := 0
	for _, cand := range candidates {
		ended, err := s.sweepOne(ctx, cand)
		if err != nil {
			// One bad row must not stop the batch: the next one may be the
			// consultation whose recording is waiting to be finalised.
			s.log.Error().Err(err).
				Str("consultation_id", cand.ID.String()).
				Msg("sweeping a stale consultation failed")
			continue
		}
		if ended {
			swept++
		}
	}
	return swept, nil
}

// sweepOne applies rule 2: the provider decides.
func (s *Service) sweepOne(ctx context.Context, cand StaleCandidate) (bool, error) {
	participants, err := s.video.ListParticipants(ctx, cand.RoomName)
	switch {
	case errors.Is(err, ErrRoomNotFound):
		// The room is definitively gone and we never heard about it. This is
		// the case the sweeper exists for: the webhook was lost.
	case err != nil:
		// Could be the outage that lost the webhook in the first place.
		// Silence is not evidence of an empty room.
		s.log.Warn().Err(err).
			Str("consultation_id", cand.ID.String()).
			Msg("video provider unavailable, leaving the consultation active for now")
		return false, nil
	case len(participants) > 0:
		// Somebody is genuinely in there. A long consultation, not a stale
		// one. Leave it alone.
		return false, nil
	}

	c, err := s.store.GetConsultation(ctx, s.pool, cand.ID)
	if err != nil {
		return false, err
	}
	if c.Status != StatusActive {
		// Ended between the query and here, by a webhook or a human.
		return false, nil
	}

	// Date it at the last evidence of activity, not at the sweep. A webhook
	// lost overnight must not report a nine-hour consultation.
	endedAt := cand.LastActivity
	if endedAt.IsZero() || endedAt.Before(cand.StartedAt) {
		endedAt = cand.StartedAt
	}

	if _, err := s.endInternal(ctx, c, EndReasonStale, endedAt); err != nil {
		return false, err
	}
	s.log.Warn().
		Str("consultation_id", c.ID.String()).
		Time("started_at", cand.StartedAt).
		Time("last_activity", cand.LastActivity).
		Msg("ended a consultation that was still active with an empty room; the provider webhook that should have done this never arrived")
	return true, nil
}

// StaleSweeper runs SweepStaleActive on a ticker.
type StaleSweeper struct {
	svc      *Service
	interval time.Duration
	idleFor  time.Duration
	batch    int
	log      zerolog.Logger
}

// NewStaleSweeper builds the worker, applying defaults for zero values.
func NewStaleSweeper(svc *Service, interval, idleFor time.Duration, batch int, log zerolog.Logger) *StaleSweeper {
	if interval <= 0 {
		interval = DefaultSweepInterval
	}
	if idleFor <= 0 {
		idleFor = DefaultIdleTimeout
	}
	if batch <= 0 {
		batch = DefaultSweepBatch
	}
	return &StaleSweeper{svc: svc, interval: interval, idleFor: idleFor, batch: batch, log: log}
}

// Run sweeps until ctx is cancelled. It runs in every replica; the Redis lease
// inside SweepStaleActive is what makes that safe.
func (w *StaleSweeper) Run(ctx context.Context) {
	w.log.Info().
		Dur("interval", w.interval).
		Dur("idle_timeout", w.idleFor).
		Msg("stale consultation sweeper started")

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			swept, err := w.svc.SweepStaleActive(ctx, w.idleFor, w.batch)
			if err != nil {
				w.log.Error().Err(err).Msg("stale consultation sweep failed")
				continue
			}
			if swept > 0 {
				w.log.Warn().Int("swept", swept).
					Msg("stale consultations ended by the sweeper; the video provider's room_finished webhook is not arriving")
			}
		}
	}
}
