package payment

import (
	"context"
	"errors"
	"fmt"

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
}

// NewConsumer builds the consumer.
func NewConsumer(svc *Service, sub events.Subscriber, log zerolog.Logger) *Consumer {
	return &Consumer{svc: svc, sub: sub, log: log}
}

// DurableName is the JetStream durable consumer name. It is stable across
// restarts and deploys, which is what lets the service pick up where it left
// off rather than replaying the whole stream.
const DurableName = "payment-service"

// Start subscribes and blocks until ctx is cancelled.
func (c *Consumer) Start(ctx context.Context) error {
	subjects := []events.Subject{
		events.SubjectAppointmentCreated,
		events.SubjectAppointmentCancelled,
	}
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

	default:
		// A subject we did not ask for. Acknowledge so it does not redeliver.
		log.Debug().Msg("ignoring unsubscribed subject")
		return nil
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
