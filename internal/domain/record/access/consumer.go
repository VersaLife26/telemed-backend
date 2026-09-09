package access

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"

	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
)

// Consumer builds the treating_relationships read-model from consumed
// consultation events. AGENT-BRIEF asks specifically for
// "Consume consultation.ended"; this also consumes consultation.started so
// that "during an appointment" access (not just "after") is actually
// enforceable -- without it, a doctor mid-consultation would be denied
// access to the very record they are being asked to look at until the call
// ends. Both subjects already exist in the platform's AllSubjects list, so
// no new subject is introduced.
type Consumer struct {
	repo *Repository
	pool database.Pool
	log  zerolog.Logger
}

// NewConsumer builds the event consumer.
func NewConsumer(repo *Repository, pool database.Pool, log zerolog.Logger) *Consumer {
	return &Consumer{repo: repo, pool: pool, log: log.With().Str("component", "access_consumer").Logger()}
}

// Subjects the consumer wants delivered.
func (c *Consumer) Subjects() []events.Subject {
	return []events.Subject{events.SubjectConsultationStarted, events.SubjectConsultationEnded}
}

// Handle processes one event. It is idempotent on envelope.ID: a redelivery
// of the same event (guaranteed possible under at-least-once delivery) is a
// no-op the second time, verified by consumed_events inside the same
// transaction as the side effect it guards.
func (c *Consumer) Handle(ctx context.Context, env events.Envelope) error {
	return database.InTx(ctx, c.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		already, err := c.repo.IsEventConsumed(ctx, tx, env.ID)
		if err != nil {
			return err
		}
		if already {
			c.log.Debug().Str("event_id", env.ID.String()).Str("subject", string(env.Subject)).Msg("duplicate delivery, skipping")
			return nil
		}

		// events.ConsultationStarted and events.ConsultationEnded are the
		// canonical types, imported from the platform and shared with
		// consultation-service, which publishes them. They are separate
		// types with separate timestamp fields, so each subject is decoded
		// into the one that actually matches it.
		//
		// This replaces a private ConsultationEventPayload that read
		// `occurred_at` -- a field the producer never sent. encoding/json
		// left it as the zero time and the code silently fell back to the
		// envelope's publish timestamp, so every treating-relationship
		// window was stamped with "when the relay drained the outbox"
		// rather than when the call actually started or ended. Under a
		// backlog those differ by however long the backlog was.
		var (
			appointmentID, doctorID, patientID uuid.UUID
			startedAt, endedAt                 *time.Time
		)
		switch env.Subject {
		case events.SubjectConsultationStarted:
			var payload events.ConsultationStarted
			if err := env.Decode(&payload); err != nil {
				return fmt.Errorf("access: decode %s payload: %w", env.Subject, err)
			}
			appointmentID, doctorID, patientID = payload.AppointmentID, payload.DoctorID, payload.PatientID
			at := payload.StartedAt
			if at.IsZero() {
				at = env.OccurredAt
			}
			startedAt = &at
		case events.SubjectConsultationEnded:
			var payload events.ConsultationEnded
			if err := env.Decode(&payload); err != nil {
				return fmt.Errorf("access: decode %s payload: %w", env.Subject, err)
			}
			appointmentID, doctorID, patientID = payload.AppointmentID, payload.DoctorID, payload.PatientID
			at := payload.EndedAt
			if at.IsZero() {
				at = env.OccurredAt
			}
			endedAt = &at
		default:
			return fmt.Errorf("access: consumer not subscribed to %s", env.Subject)
		}

		if appointmentID == uuid.Nil || doctorID == uuid.Nil || patientID == uuid.Nil {
			return fmt.Errorf("access: %s payload missing required ids", env.Subject)
		}

		if err := c.repo.UpsertTreatingRelationship(ctx, tx, appointmentID, doctorID, patientID, startedAt, endedAt); err != nil {
			return err
		}
		return c.repo.MarkEventConsumed(ctx, tx, env.ID, string(env.Subject))
	})
}
