package doctor

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"

	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
)

// DurableName is the JetStream durable consumer name for doctor-service's
// appointment.completed subscription.
// DurableName must not contain "." -- JetStream rejects a durable whose name
// contains a subject-token separator, and the consumer then fails to start.
// That failure was silent in effect: the projection never ran, so
// ?available=now returned nothing against 1,577 real slots.
const DurableName = "doctor-service_appointment-completed"

// appointment.completed decodes into events.AppointmentTerminal, the canonical
// payload scheduling-service publishes. It used to be a private struct here
// that guessed the shape, and the guess was wrong: it declared `completed_at`
// while scheduling sends `occurred_at`, so the timestamp was always the zero
// time. Nothing failed, because encoding/json does not error on an absent
// field -- which is precisely why the payload is now shared rather than
// re-declared.

// EventConsumer wires the appointment.completed subscription to the
// service's review-eligibility and consultation-count side effects.
type EventConsumer struct {
	repo *Repository
	pool database.Pool
	log  zerolog.Logger
}

// NewEventConsumer builds an EventConsumer.
func NewEventConsumer(repo *Repository, pool database.Pool, log zerolog.Logger) *EventConsumer {
	return &EventConsumer{repo: repo, pool: pool, log: log}
}

// Subjects this consumer wants delivered.
func (c *EventConsumer) Subjects() []events.Subject {
	return []events.Subject{events.SubjectAppointmentCompleted}
}

// Run subscribes and blocks until ctx is cancelled.
func (c *EventConsumer) Run(ctx context.Context, sub events.Subscriber) error {
	return sub.Subscribe(ctx, DurableName, c.Subjects(), c.handle)
}

// handle unlocks review eligibility for the completed appointment and, the
// first time this appointment_id is seen, bumps the doctor's
// consultation_count. Idempotency comes from RecordEligibility's
// INSERT ... ON CONFLICT (appointment_id) DO NOTHING, not from the event's
// own id -- a natural business key is a stronger and simpler guarantee than
// deduplicating on envelope.ID, and it is what the "created" return value
// gates the consultation_count increment on.
func (c *EventConsumer) handle(ctx context.Context, env events.Envelope) error {
	var payload events.AppointmentTerminal
	if err := env.Decode(&payload); err != nil {
		c.log.Error().Err(err).Str("event_id", env.ID.String()).
			Msg("appointment.completed consumer: unparseable payload, dropping")
		return nil
	}
	if payload.AppointmentID == uuidZero || payload.DoctorID == uuidZero || payload.PatientID == uuidZero {
		c.log.Error().Str("event_id", env.ID.String()).
			Msg("appointment.completed consumer: payload missing required ids, dropping")
		return nil
	}

	err := database.InTx(ctx, c.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		created, err := c.repo.RecordEligibility(ctx, tx, payload.DoctorID, payload.PatientID, payload.AppointmentID)
		if err != nil {
			return err
		}
		if !created {
			// Redelivery of an event we already applied; consultation_count
			// was already incremented the first time.
			return nil
		}
		return c.repo.IncrementConsultationCount(ctx, tx, payload.DoctorID)
	})
	if err != nil {
		return fmt.Errorf("appointment.completed consumer: appointment %s: %w", payload.AppointmentID, err)
	}
	return nil
}

var uuidZero uuid.UUID
