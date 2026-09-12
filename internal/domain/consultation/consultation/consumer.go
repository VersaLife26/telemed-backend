package consultation

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/platform/events"
)

// appointmentTeardownSubjects close an unstarted consultation so a late join
// cannot reopen a visit that scheduling has already recorded as cancelled or
// a no-show.
var appointmentTeardownSubjects = []events.Subject{
	events.SubjectAppointmentCancelled,
	events.SubjectAppointmentNoShow,
}

// RegisterConsumers starts the durable consumers this service owns:
//
//   - appointment.confirmed pre-creates the consultation row so the room name
//     is known before either party ever calls Join.
//   - appointment.cancelled and appointment.no_show tear down a consultation
//     that has not started, so a late join cannot reopen a closed visit.
//   - appointment.rescheduled moves scheduled_at on a consultation that has
//     not started, keeping the same room name and join URLs.
//
// Handlers are idempotent on redelivery via database constraints (see
// Service.CreateFromAppointment, Service.TeardownForCancellation, and
// Service.RescheduleFromAppointment), which is what the platform's
// at-least-once delivery guarantee requires without needing a separate
// processed-events table for these subjects.
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
			appointmentTeardownSubjects,
			func(ctx context.Context, env events.Envelope) error {
				var id struct {
					AppointmentID uuid.UUID `json:"appointment_id"`
				}
				if err := env.Decode(&id); err != nil {
					return fmt.Errorf("consultation: decode %s: %w", env.Subject, err)
				}
				if err := svc.TeardownForCancellation(ctx, id.AppointmentID); err != nil {
					return fmt.Errorf("consultation: handle %s: %w", env.Subject, err)
				}
				return nil
			})
		if err != nil {
			log.Error().Err(err).Msg("appointment.cancelled/no_show consumer stopped")
		}
	}()

	go func() {
		err := sub.Subscribe(ctx, "consultation-service-appointment-rescheduled",
			[]events.Subject{events.SubjectAppointmentRescheduled},
			func(ctx context.Context, env events.Envelope) error {
				var payload events.AppointmentRescheduled
				if err := env.Decode(&payload); err != nil {
					return fmt.Errorf("consultation: decode appointment.rescheduled: %w", err)
				}
				if err := svc.RescheduleFromAppointment(ctx, payload); err != nil {
					return fmt.Errorf("consultation: handle appointment.rescheduled: %w", err)
				}
				return nil
			})
		if err != nil {
			log.Error().Err(err).Msg("appointment.rescheduled consumer stopped")
		}
	}()

	log.Info().Msg("consultation event consumers registered")
}
