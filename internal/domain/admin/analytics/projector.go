package analytics

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"telemed/internal/platform/events"
)

// Projector consumes payment.* and appointment.* events into the local
// analytics projection tables. It never touches the materialized views
// directly -- those are refreshed on a schedule by Refresher, deliberately
// decoupled from the write path so a burst of bookings cannot turn every
// event into a dashboard-refresh storm.
//
// Every payload is a canonical type from
// internal/platform/events/payloads.go, shared with its producer. There used
// to be one private struct covering all three payment subjects and another
// covering all five appointment subjects, which quietly asserted that the
// members of each group have the same shape. They do not: payment.refunded
// carries no doctor_id and no provider, payment.failed carries no commission,
// and only appointment.created carries the specialty. encoding/json filled
// each absent field with a zero, so the revenue and utilisation dashboards
// were being fed zeros nobody could see the origin of.
type Projector struct {
	repo *Repository
	log  zerolog.Logger
}

func NewProjector(repo *Repository, log zerolog.Logger) *Projector {
	return &Projector{repo: repo, log: log.With().Str("component", "analytics_projector").Logger()}
}

// Subscribe registers durable consumers for every event analytics needs.
// Call once at boot; blocks until ctx is cancelled.
func (p *Projector) Subscribe(ctx context.Context, sub events.Subscriber) error {
	if err := sub.Subscribe(ctx, "admin-analytics-payments",
		[]events.Subject{events.SubjectPaymentSucceeded, events.SubjectPaymentRefunded, events.SubjectPaymentFailed},
		p.handlePayment); err != nil {
		return err
	}
	return sub.Subscribe(ctx, "admin-analytics-appointments",
		[]events.Subject{
			events.SubjectAppointmentCreated, events.SubjectAppointmentConfirmed,
			events.SubjectAppointmentCancelled, events.SubjectAppointmentCompleted,
			events.SubjectAppointmentNoShow, events.SubjectAppointmentRescheduled,
		}, p.handleAppointment)
}

// handlePayment decodes into the canonical type that actually matches the
// subject, then fills in the specialty from this service's own appointment
// projection -- payment events carry no specialty, and asking payment-service
// to denormalise one would put the same fact in two places.
func (p *Projector) handlePayment(ctx context.Context, env events.Envelope) error {
	var (
		fact   PaymentFact
		status string
	)

	switch env.Subject {
	case events.SubjectPaymentSucceeded:
		var payload events.PaymentSucceeded
		if err := env.Decode(&payload); err != nil {
			return fmt.Errorf("analytics: decode %s: %w", env.Subject, err)
		}
		status = "succeeded"
		fact = PaymentFact{
			PaymentID: payload.PaymentID, AppointmentID: payload.AppointmentID,
			DoctorID: payload.DoctorID, PatientID: payload.PatientID,
			AmountCents: payload.AmountCents, CommissionCents: payload.CommissionCents,
			Currency: payload.Currency, Provider: payload.Provider,
			OccurredAt: payload.SucceededAt,
		}

	case events.SubjectPaymentFailed:
		var payload events.PaymentFailed
		if err := env.Decode(&payload); err != nil {
			return fmt.Errorf("analytics: decode %s: %w", env.Subject, err)
		}
		status = "failed"
		// No DoctorID and no commission on this payload, and correctly so:
		// a failed payment earned nothing and settled to nobody. Leaving them
		// zero is the honest projection, not a missing field.
		fact = PaymentFact{
			PaymentID: payload.PaymentID, AppointmentID: payload.AppointmentID,
			PatientID: payload.PatientID, AmountCents: payload.AmountCents,
			Currency: payload.Currency, Provider: payload.Provider,
			OccurredAt: payload.FailedAt,
		}

	case events.SubjectPaymentRefunded:
		var payload events.PaymentRefunded
		if err := env.Decode(&payload); err != nil {
			return fmt.Errorf("analytics: decode %s: %w", env.Subject, err)
		}
		status = "refunded"
		fact = PaymentFact{
			PaymentID: payload.PaymentID, AppointmentID: payload.AppointmentID,
			PatientID: payload.PatientID, AmountCents: payload.AmountCents,
			Currency: payload.Currency, OccurredAt: payload.RefundedAt,
		}

	default:
		return fmt.Errorf("analytics: payment consumer not subscribed to %s", env.Subject)
	}

	// payment-service has not adopted the canonical payloads yet and still
	// sends one `occurred_at` rather than succeeded_at/failed_at/refunded_at.
	// Accept both, or every payment lands in year 1 and the revenue rollup
	// reads zero for every real day. See eventcompat.go.
	at, source := resolveOccurredAt(env, fact.OccurredAt)
	fact.OccurredAt = at
	if source != "" {
		p.log.Warn().
			Str("subject", string(env.Subject)).
			Str("timestamp_source", source).
			Msg("analytics: producer still uses a pre-canonical timestamp spelling")
	}

	specialty, district, err := p.repo.AppointmentContext(ctx, fact.AppointmentID)
	if err != nil {
		return err
	}
	fact.SpecialtyCode, fact.District = specialty, district

	return p.repo.UpsertPayment(ctx, env.ID, status, fact)
}

// handleAppointment projects the booking lifecycle.
//
// Only appointment.created carries the specialty, which is exactly why the
// repository's ON CONFLICT clause updates status and occurred_at but leaves
// specialty_code as the creation event set them. scheduled_at is also frozen
// after create -- except appointment.rescheduled, which is the one later
// event that carries a new visit time (see RescheduleAppointment).
func (p *Projector) handleAppointment(ctx context.Context, env events.Envelope) error {
	if env.Subject == events.SubjectAppointmentCreated {
		var payload events.AppointmentCreated
		if err := env.Decode(&payload); err != nil {
			return fmt.Errorf("analytics: decode %s: %w", env.Subject, err)
		}
		return p.repo.UpsertAppointment(ctx, env.ID, "created", AppointmentFact{
			AppointmentID: payload.AppointmentID, DoctorID: payload.DoctorID,
			PatientID: payload.PatientID, SpecialtyCode: payload.Specialty,
			ScheduledAt: payload.StartAt, OccurredAt: payload.CreatedAt,
		})
	}

	var (
		fact   AppointmentFact
		status string
	)
	switch env.Subject {
	case events.SubjectAppointmentConfirmed:
		var payload events.AppointmentConfirmed
		if err := env.Decode(&payload); err != nil {
			return fmt.Errorf("analytics: decode %s: %w", env.Subject, err)
		}
		status = "confirmed"
		fact = AppointmentFact{
			AppointmentID: payload.AppointmentID, DoctorID: payload.DoctorID,
			PatientID: payload.PatientID, ScheduledAt: payload.StartAt,
			OccurredAt: payload.ConfirmedAt,
		}

	case events.SubjectAppointmentCancelled:
		var payload events.AppointmentCancelled
		if err := env.Decode(&payload); err != nil {
			return fmt.Errorf("analytics: decode %s: %w", env.Subject, err)
		}
		status = "cancelled"
		// CancelledAt is when the cancellation was REQUESTED, not when this
		// event was consumed. Using it keeps a cancellation that sat in a
		// queue past midnight counted on the day it actually happened.
		fact = AppointmentFact{
			AppointmentID: payload.AppointmentID, DoctorID: payload.DoctorID,
			PatientID: payload.PatientID, ScheduledAt: payload.StartAt,
			OccurredAt: payload.CancelledAt,
		}

	case events.SubjectAppointmentRescheduled:
		var payload events.AppointmentRescheduled
		if err := env.Decode(&payload); err != nil {
			return fmt.Errorf("analytics: decode %s: %w", env.Subject, err)
		}
		specialty, district, err := p.repo.AppointmentContext(ctx, payload.AppointmentID)
		if err != nil {
			return err
		}
		return p.repo.RescheduleAppointment(ctx, env.ID, AppointmentFact{
			AppointmentID: payload.AppointmentID, DoctorID: payload.DoctorID,
			PatientID: payload.PatientID, SpecialtyCode: specialty, District: district,
			ScheduledAt: payload.ProposedStart, OccurredAt: payload.RescheduledAt,
		})

	case events.SubjectAppointmentCompleted, events.SubjectAppointmentNoShow:
		var payload events.AppointmentTerminal
		if err := env.Decode(&payload); err != nil {
			return fmt.Errorf("analytics: decode %s: %w", env.Subject, err)
		}
		status = "completed"
		if env.Subject == events.SubjectAppointmentNoShow {
			status = "no_show"
		}
		fact = AppointmentFact{
			AppointmentID: payload.AppointmentID, DoctorID: payload.DoctorID,
			PatientID: payload.PatientID, ScheduledAt: payload.StartAt,
			OccurredAt: payload.OccurredAt,
		}

	default:
		return fmt.Errorf("analytics: appointment consumer not subscribed to %s", env.Subject)
	}

	// Carry the specialty forward from the creation event so an out-of-order
	// arrival that inserts the row first does not file it under no specialty
	// forever.
	specialty, district, err := p.repo.AppointmentContext(ctx, fact.AppointmentID)
	if err != nil {
		return err
	}
	fact.SpecialtyCode, fact.District = specialty, district

	return p.repo.UpsertAppointment(ctx, env.ID, status, fact)
}
