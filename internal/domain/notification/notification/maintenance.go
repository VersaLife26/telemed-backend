package notification

import (
	"context"
	"time"

	"github.com/rs/zerolog"
)

// MaintenanceJob runs the housekeeping this service owns: pruning
// PHI-adjacent notification bodies past the retention window (see the
// retention note in migrations/000002_notification_schema.up.sql) and
// hard-deleting device tokens that have been invalidated for a long time.
type MaintenanceJob struct {
	repo           *Repository
	log            zerolog.Logger
	bodyRetention  time.Duration
	tokenRetention time.Duration
}

// NewMaintenanceJob builds the job. bodyRetention defaults to 90 days
// (matching AGENT-BRIEF's retention requirement for this service);
// tokenRetention defaults to 30 days.
func NewMaintenanceJob(repo *Repository, log zerolog.Logger, bodyRetention, tokenRetention time.Duration) *MaintenanceJob {
	if bodyRetention <= 0 {
		bodyRetention = 90 * 24 * time.Hour
	}
	if tokenRetention <= 0 {
		tokenRetention = 30 * 24 * time.Hour
	}
	return &MaintenanceJob{repo: repo, log: log, bodyRetention: bodyRetention, tokenRetention: tokenRetention}
}

// Run sweeps once immediately (so a short-lived container still gets one
// pass) and then every interval until ctx is cancelled. Both sweeps are
// cheap, idempotent UPDATE/DELETE statements, so running them more often
// than strictly necessary costs nothing.
func (m *MaintenanceJob) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	m.tick(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.tick(ctx)
		}
	}
}

func (m *MaintenanceJob) tick(ctx context.Context) {
	if n, err := m.repo.PruneOldNotificationBodies(ctx, m.bodyRetention); err != nil {
		m.log.Error().Err(err).Msg("prune notification bodies failed")
	} else if n > 0 {
		m.log.Info().Int64("rows", n).Msg("pruned notification bodies past retention window")
	}

	if n, err := m.repo.HardPruneInvalidatedTokens(ctx, m.tokenRetention); err != nil {
		m.log.Error().Err(err).Msg("prune invalidated device tokens failed")
	} else if n > 0 {
		m.log.Info().Int64("rows", n).Msg("hard-pruned long-invalidated device tokens")
	}
}
