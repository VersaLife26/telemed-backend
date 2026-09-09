package consultation

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"telemed/internal/platform/events"
)

// RegisterConsumers starts the two durable consumers this service owns:
//
//   - appointment.confirmed pre-creates the consultation row so the room name
//     is known before either party ever calls Join.
//   - appointment.cancelled tears down a consultation that has not started.
//
// Both handlers are idempotent on redelivery via database constraints (see
// Service.CreateFromAppointment and Service.TeardownForCancellation), which is
// what the platform's at-least-once delivery guarantee requires without
// needing a separate processed-events table for these two subjects.
//
// events.Subscriber.Subscribe blocks until ctx is cancelled (it drives the
// pull consumer's Consume loop internally), so each subscription runs in its
// own goroutine -- the same fire-and-forget shape cmd/server/main.go already
// uses for the outbox relay. A failure to even establish the consumer is
// logged, not returned, for the same reason: a NATS hiccup at boot must not
// crash a service that otherwise has no other dependency on it being up yet.
func RegisterConsumers(ctx context.Context, sub events.Subscriber, svc *Service, log zerolog.Logger) {
	go func() {
		err := sub.Subscribe(ctx, "consultation-service-appointment-confirmed",
			[]events.Subject{events.SubjectAppointmentConfirmed},
			func(ctx context.Context, env events.Envelope) error {
				var payload events.AppointmentConfirmed
				if err := env.Decode(&payload); err != nil {
					return fmt.Errorf("consultation: decode appointment.confirmed: %w", err)
				}
				if err := svc.CreateFromAppointment(ctx, payload); err != nil {
					return fmt.Errorf("consultation: handle appointment.confirmed: %w", err)
				}
				return nil
			})
		if err != nil {
			log.Error().Err(err).Msg("appointment.confirmed consumer stopped")
		}
	}()

	go func() {
		err := sub.Subscribe(ctx, "consultation-service-appointment-cancelled",
			[]events.Subject{events.SubjectAppointmentCancelled},
			func(ctx context.Context, env events.Envelope) error {
				var payload events.AppointmentCancelled
				if err := env.Decode(&payload); err != nil {
					return fmt.Errorf("consultation: decode appointment.cancelled: %w", err)
				}
				if err := svc.TeardownForCancellation(ctx, payload.AppointmentID); err != nil {
					return fmt.Errorf("consultation: handle appointment.cancelled: %w", err)
				}
				return nil
			})
		if err != nil {
			log.Error().Err(err).Msg("appointment.cancelled consumer stopped")
		}
	}()

	log.Info().Msg("consultation event consumers registered")
}
