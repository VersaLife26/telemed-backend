package user

import (
	"context"
	"errors"
	"time"

	"github.com/rs/zerolog"
)

// Reaper periodically anonymises accounts whose PDPA erasure grace period
// has passed. It follows the same shape as events.Relay: every replica runs
// it, and the underlying UPDATE ... FOR UPDATE SKIP LOCKED query makes that
// safe without a leader election.
type Reaper struct {
	svc       *Service
	log       zerolog.Logger
	interval  time.Duration
	batchSize int
}

// NewReaper builds the erasure worker.
func NewReaper(svc *Service, log zerolog.Logger, interval time.Duration, batchSize int) *Reaper {
	if interval <= 0 {
		interval = time.Hour
	}
	if batchSize <= 0 {
		batchSize = 100
	}
	return &Reaper{svc: svc, log: log, interval: interval, batchSize: batchSize}
}

// Run polls until ctx is cancelled. Call it in its own goroutine at boot,
// same as the outbox relay.
func (r *Reaper) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	r.log.Info().Dur("interval", r.interval).Msg("pdpa erasure reaper started")

	for {
		select {
		case <-ctx.Done():
			r.log.Info().Msg("pdpa erasure reaper stopped")
			return
		case <-ticker.C:
			n, err := r.svc.AnonymizeDue(ctx, r.batchSize)
			if err != nil && !errors.Is(err, context.Canceled) {
				r.log.Error().Err(err).Msg("pdpa erasure sweep failed")
				continue
			}
			if n > 0 {
				r.log.Info().Int64("count", n).Msg("pdpa erasure: accounts anonymised")
			}
		}
	}
}
