package payment

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/platform/events"
	"telemed/internal/platform/logger"
)

// Consumer subscribes to the scheduling events that drive payment state.
//
// Two subjects, two rules:
//
//	appointment.created    -> create the pending payment row
//	appointment.cancelled  -> refund according to the cancellation policy
//
// Delivery is at-least-once, so both handlers are idempotent. appointment.created
// is idempotent on the payments.appointment_id unique index; appointment.cancelled
// is idempotent on the refund's idempotency key, which is derived from the
// appointment id.
type Consumer struct {
	svc *Service
	sub events.Subscriber
	log zerolog.Logger
	// payouts carries out admin.payout_batch_requested. Nil disables that one
	// handler rather than the consumer: a deployment that runs payments
	// without the settlement runner should ignore the command loudly, not
	// fail to start.
	payouts *PayoutRunner
}

// NewConsumer builds the consumer.
func NewConsumer(svc *Service, sub events.Subscriber, payouts *PayoutRunner, log zerolog.Logger) *Consumer {
	return &Consumer{svc: svc, sub: sub, payouts: payouts, log: log}
}

// DurableName is the JetStream durable consumer name. It is stable across
// restarts and deploys, which is what lets the service pick up where it left
// off rather than replaying the whole stream.
const DurableName = "payment-service"

// subjects lists what this service consumes. It is a function rather than a
// literal inside Start so a test can assert the set without a broker: an
// administrator command that nothing subscribes to fails silently, and the
// console reports success either way.
func subjects() []events.Subject {
	return []events.Subject{
		events.SubjectAppointmentCreated,
		events.SubjectAppointmentCancelled,
		// Administrator commands. This service owns money, so it is the only
		// place that can carry them out.
		events.SubjectAdminRefundApproved,
		events.SubjectAdminPayoutBatchRequested,
	}
}

// Start subscribes and blocks until ctx is cancelled.
func (c *Consumer) Start(ctx context.Context) error {
	subjects := subjects()
	if err := c.sub.Subscribe(ctx, DurableName, subjects, c.handle); err != nil {
		return fmt.Errorf("payment: subscribe to appointment events: %w", err)
	}
	c.log.Info().Strs("subjects", subjectStrings(subjects)).Msg("payment event consumer started")
	return nil
}

// handle dispatches one delivered event.
//
// Returning nil acknowledges. Returning an error triggers redelivery, so the
// distinction below matters: a malformed payload is acknowledged and dropped
// because redelivering it will never help, while a database failure is
// returned so it is retried.
func (c *Consumer) handle(ctx context.Context, env events.Envelope) error {
	log := c.log.With().
		Str("subject", string(env.Subject)).
		Str("event_id", logger.MaskID(env.ID.String())).
		Logger()

	switch env.Subject {
	case events.SubjectAppointmentCreated:
		var payload events.AppointmentCreated
		if err := env.Decode(&payload); err != nil {
			// Poison. Acknowledge it rather than wedging the consumer on one
			// bad message forever.
			log.Error().Err(err).Msg("dropping undecodable appointment.created")
			return nil
		}
		if err := c.svc.OnAppointmentCreated(ctx, payload); err != nil {
			if isPermanent(err) {
				log.Error().Err(err).Msg("dropping unprocessable appointment.created")
				return nil
			}
			return err
		}
		log.Debug().Msg("pending payment created for appointment")
		return nil

	case events.SubjectAppointmentCancelled:
		var payload events.AppointmentCancelled
		if err := env.Decode(&payload); err != nil {
			log.Error().Err(err).Msg("dropping undecodable appointment.cancelled")
			return nil
		}
		if err := c.svc.OnAppointmentCancelled(ctx, payload); err != nil {
			if isPermanent(err) {
				log.Error().Err(err).Msg("dropping unprocessable appointment.cancelled")
				return nil
			}
			return err
		}
		return nil

	case events.SubjectAdminRefundApproved:
		var payload events.AdminRefundApproved
		if err := env.Decode(&payload); err != nil {
			log.Error().Err(err).Msg("dropping undecodable admin.refund_approved")
			return nil
		}
		return c.handleRefundApproved(ctx, payload, env, log)

	case events.SubjectAdminPayoutBatchRequested:
		var payload events.AdminPayoutBatchRequested
		if err := env.Decode(&payload); err != nil {
			log.Error().Err(err).Msg("dropping undecodable admin.payout_batch_requested")
			return nil
		}
		return c.handlePayoutBatchRequested(ctx, payload, log)

	default:
		// A subject we did not ask for. Acknowledge so it does not redeliver.
		log.Debug().Msg("ignoring unsubscribed subject")
		return nil
	}
}

// handleRefundApproved returns money an administrator decided to return.
//
// CallerIsOps is true and AmountCents is passed straight through: an approved
// refund is a human overriding the cancellation policy, which is the whole
// reason the approval exists. ReasonAdminOverride is the closed-set value that
// records exactly that in the finance report; the administrator's free text
// rides along in the audit log the admin service already wrote, not here,
// because RefundReason is grouped on and must stay a closed set.
//
// Idempotency is the refund's own key, derived from the envelope id: a
// JetStream redelivery presents the same key and the second call is a no-op
// rather than a second refund.
func (c *Consumer) handleRefundApproved(ctx context.Context, cmd events.AdminRefundApproved, env events.Envelope, log zerolog.Logger) error {
	if cmd.PaymentID == uuid.Nil {
		log.Error().Msg("admin.refund_approved without a payment_id")
		return nil
	}
	_, err := c.svc.Refund(ctx, RefundInput{
		PaymentID:      cmd.PaymentID,
		CallerID:       cmd.AdminID,
		CallerIsOps:    true,
		Actor:          ActorSystem,
		AmountCents:    cmd.AmountCents,
		Reason:         ReasonAdminOverride,
		IdempotencyKey: "admin-refund:" + env.ID.String(),
	})
	if err != nil {
		if isPermanent(err) {
			log.Error().Err(err).Str("payment_id", logger.MaskID(cmd.PaymentID.String())).
				Msg("dropping unprocessable admin.refund_approved")
			return nil
		}
		return err
	}
	log.Info().Str("payment_id", logger.MaskID(cmd.PaymentID.String())).
		Msg("administrator-approved refund executed")
	return nil
}

// handlePayoutBatchRequested runs one settlement pass.
//
// The command's From/To are not passed on: PayoutRunner settles everything
// that is due, and inventing a windowed variant here would give the platform
// two different definitions of "due". They are recorded in the admin service's
// audit entry, which is where the operator's intent belongs.
//
// ErrPayoutRunInProgress is success, not failure. The cron and this command
// compete for the same lease by design, and redelivering because the nightly
// run happened to hold it would just try again against a runner that has
// already settled everything.
func (c *Consumer) handlePayoutBatchRequested(ctx context.Context, cmd events.AdminPayoutBatchRequested, log zerolog.Logger) error {
	if c.payouts == nil {
		log.Error().Msg("admin.payout_batch_requested received but no payout runner is configured")
		return nil
	}
	summary, err := c.payouts.Run(ctx)
	switch {
	case err == nil:
		log.Info().
			Int("paid", summary.Paid).Int("failed", summary.Failed).
			Int64("amount_cents", summary.AmountCents).
			Str("admin_id", logger.MaskID(cmd.AdminID.String())).
			Msg("administrator-requested payout batch complete")
		return nil
	case errors.Is(err, ErrPayoutRunInProgress):
		log.Info().Msg("administrator-requested payout batch skipped: a run already holds the lease")
		return nil
	default:
		return err
	}
}

// isPermanent reports whether retrying could ever succeed. Getting this wrong
// in the pessimistic direction costs a redelivery; getting it wrong in the
// optimistic direction wedges the consumer on one message forever.
func isPermanent(err error) bool {
	switch {
	case errors.Is(err, ErrNothingRefundable),
		errors.Is(err, ErrUnsupported),
		errors.Is(err, ErrProviderRejected):
		return true
	default:
		return false
	}
}

func subjectStrings(subjects []events.Subject) []string {
	out := make([]string, len(subjects))
	for i, s := range subjects {
		out[i] = string(s)
	}
	return out
}
