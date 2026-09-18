package finance

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"telemed/internal/platform/events"
)

// Projector folds payout.sent into payout_batches_projection so the console
// can list settlement windows without joining telemed_payment.
type Projector struct {
	repo *Repository
	log  zerolog.Logger
}

func NewProjector(repo *Repository, log zerolog.Logger) *Projector {
	return &Projector{repo: repo, log: log.With().Str("component", "finance_projector").Logger()}
}

func (p *Projector) Subscribe(ctx context.Context, sub events.Subscriber) error {
	return sub.Subscribe(ctx, "admin-finance-payouts",
		[]events.Subject{events.SubjectPayoutSent}, p.handlePayout)
}

func (p *Projector) handlePayout(ctx context.Context, env events.Envelope) error {
	var payload events.PayoutSent
	if err := env.Decode(&payload); err != nil {
		return fmt.Errorf("finance: decode payout.sent: %w", err)
	}
	sentAt := payload.SentAt
	if sentAt.IsZero() {
		sentAt = env.OccurredAt
	}
	if err := p.repo.ApplyPayoutSent(ctx, payload.PeriodStart, payload.PeriodEnd,
		payload.AmountCents, payload.Currency, sentAt); err != nil {
		return err
	}
	return nil
}
