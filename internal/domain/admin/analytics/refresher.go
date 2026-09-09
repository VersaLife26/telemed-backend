package analytics

import (
	"context"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"
)

// Refresher runs REFRESH MATERIALIZED VIEW CONCURRENTLY on the analytics
// views hourly (SDD/V2 docs section 8.8: "refreshed hourly via cron"). It is
// deliberately a single dependency-free loop rather than robfig/cron's full
// scheduler for the common case (see NewHourly), but exposes RunOnce so
// tests and an operator's manual "refresh now" tool can trigger it directly
// without waiting for the next tick.
type Refresher struct {
	repo *Repository
	log  zerolog.Logger
}

func NewRefresher(repo *Repository, log zerolog.Logger) *Refresher {
	return &Refresher{repo: repo, log: log}
}

// RunOnce refreshes every materialized view immediately.
func (r *Refresher) RunOnce(ctx context.Context) error {
	start := time.Now()
	if err := r.repo.RefreshAll(ctx); err != nil {
		r.log.Error().Err(err).Msg("analytics: materialized view refresh failed")
		return err
	}
	r.log.Info().Dur("took", time.Since(start)).Msg("analytics: materialized views refreshed")
	return nil
}

// RunHourly starts a cron schedule ("@hourly") that calls RunOnce, using
// robfig/cron -- the platform's pinned scheduler (AGENT-BRIEF section 1) --
// rather than a hand-rolled ticker, so this loop reads the same way every
// other cron job in the platform does. It blocks until ctx is cancelled;
// run it in its own goroutine at boot, exactly like the outbox relay.
func (r *Refresher) RunHourly(ctx context.Context) error {
	c := cron.New()
	if _, err := c.AddFunc("@hourly", func() {
		refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		_ = r.RunOnce(refreshCtx)
	}); err != nil {
		return err
	}
	c.Start()
	<-ctx.Done()
	stopCtx := c.Stop()
	select {
	case <-stopCtx.Done():
	case <-time.After(10 * time.Second):
	}
	return nil
}
