// Package events implements the transactional outbox pattern over NATS
// JetStream.
//
// The rule the whole platform depends on: a business change and the event
// announcing it are written in ONE Postgres transaction. Nothing is published
// to the broker inline. A relay worker then moves rows from outbox_events to
// JetStream and marks them published.
//
// This is what makes "slot booked but nobody was notified" impossible. The
// worst case is a duplicate delivery, which every consumer handles because
// every consumer is idempotent on event_id.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Subject is a NATS subject. The platform uses <domain>.<past-tense-verb>.
type Subject string

const (
	SubjectUserRegistered   Subject = "user.registered"
	SubjectUserSuspended    Subject = "user.suspended"
	SubjectUserReinstated   Subject = "user.reinstated"
	SubjectDoctorRegistered Subject = "doctor.registered"
	SubjectDoctorApproved   Subject = "doctor.approved"
	SubjectDoctorRejected   Subject = "doctor.rejected"
	SubjectDoctorUpdated    Subject = "doctor.updated"
	// SubjectDoctorApplicationSubmitted is the public apply path: profile
	// details collected before any OTP or user account exists.
	SubjectDoctorApplicationSubmitted Subject = "doctor.application_submitted"
	SubjectDoctorApplicationApproved  Subject = "doctor.application_approved"
	SubjectDoctorApplicationRejected  Subject = "doctor.application_rejected"
	// SubjectDoctorDocumentsUpdated announces a change to a doctor's
	// credential document set. It is separate from doctor.updated because a
	// doctor registers first and uploads credentials afterwards: the
	// registration event necessarily predates every document on it, so
	// without this the admin verification queue shows an application with no
	// attachments and a reviewer has nothing to review.
	SubjectDoctorDocumentsUpdated Subject = "doctor.documents_updated"

	SubjectSlotsGenerated Subject = "slot.generated"
	SubjectSlotBooked     Subject = "slot.booked"
	SubjectSlotReleased   Subject = "slot.released"
	// SubjectSlotWithdrawn is NOT slot.released, and folding the two together
	// is a real bug: released means "bookable again", withdrawn means "never
	// again". A consumer that treats a withdrawal as a release puts a doctor's
	// slots back on the market on the exact day they are on leave.
	SubjectSlotWithdrawn Subject = "slot.withdrawn"

	SubjectAppointmentCreated   Subject = "appointment.created"
	SubjectAppointmentConfirmed Subject = "appointment.confirmed"
	SubjectAppointmentCancelled Subject = "appointment.cancelled"
	SubjectAppointmentCompleted Subject = "appointment.completed"
	SubjectAppointmentNoShow    Subject = "appointment.no_show"
	SubjectAppointmentReminder  Subject = "appointment.reminder_due"
	// SubjectAppointmentRescheduleRequested is a doctor asking to move a
	// confirmed booking. It is not a cancellation: the original slot stays
	// held until the patient or an administrator accepts or declines.
	SubjectAppointmentRescheduleRequested Subject = "appointment.reschedule_requested"
	// SubjectAppointmentRescheduled is the fact that the same paid appointment
	// now occupies a different slot. Consumers must not treat this as a cancel.
	SubjectAppointmentRescheduled Subject = "appointment.rescheduled"

	SubjectWaitlistSlotOffer Subject = "waitlist.slot_offered"

	SubjectPaymentSucceeded Subject = "payment.succeeded"
	SubjectPaymentFailed    Subject = "payment.failed"
	SubjectPaymentRefunded  Subject = "payment.refunded"
	SubjectPayoutSent       Subject = "payout.sent"

	SubjectConsultationStarted Subject = "consultation.started"
	SubjectConsultationEnded   Subject = "consultation.ended"
	// SubjectConsultationDoctorRunningLate is a courtesy to the next patient
	// while the previous consult is still active past its booked end. It is
	// not a reschedule and carries no clinical detail.
	SubjectConsultationDoctorRunningLate Subject = "consultation.doctor_running_late"
	// SubjectConsultationEarlyJoinOffered asks the next patient whether they
	// can join a few minutes early. It is not a reschedule: the booked time
	// stands unless they accept and join through the existing waiting room.
	SubjectConsultationEarlyJoinOffered Subject = "consultation.early_join_offered"

	SubjectPrescriptionIssued Subject = "prescription.issued"

	// admin.* are COMMANDS, not facts. The admin service owns no clinical or
	// financial data, so an admin action becomes a request to the service that
	// does own it, applied and audited there. Naming them in the imperative
	// keeps that distinction visible: everything else on this list already
	// happened.
	SubjectAdminUserSuspendRequested          Subject = "admin.user_suspend_requested"
	SubjectAdminUserReinstateRequested        Subject = "admin.user_reinstate_requested"
	SubjectAdminAppointmentForceCancel        Subject = "admin.appointment_force_cancel_requested"
	SubjectAdminDoubleBookingResolveRequested Subject = "admin.double_booking_resolve_requested"
	SubjectAdminRefundApproved                Subject = "admin.refund_approved"
	SubjectAdminPayoutBatchRequested          Subject = "admin.payout_batch_requested"
)

// AllSubjects is used by the infra bootstrap to declare the JetStream stream
// and by tests to assert no service publishes an undeclared subject.
var AllSubjects = []Subject{
	SubjectUserRegistered, SubjectUserSuspended, SubjectUserReinstated,
	SubjectDoctorRegistered, SubjectDoctorApproved, SubjectDoctorRejected, SubjectDoctorUpdated,
	SubjectDoctorDocumentsUpdated,
	SubjectDoctorApplicationSubmitted, SubjectDoctorApplicationApproved, SubjectDoctorApplicationRejected,
	SubjectSlotsGenerated, SubjectSlotBooked, SubjectSlotReleased, SubjectSlotWithdrawn,
	SubjectAppointmentCreated, SubjectAppointmentConfirmed, SubjectAppointmentCancelled,
	SubjectAppointmentCompleted, SubjectAppointmentNoShow, SubjectAppointmentReminder,
	SubjectAppointmentRescheduleRequested, SubjectAppointmentRescheduled,
	SubjectWaitlistSlotOffer,
	SubjectPaymentSucceeded, SubjectPaymentFailed, SubjectPaymentRefunded, SubjectPayoutSent,
	SubjectConsultationStarted, SubjectConsultationEnded, SubjectConsultationDoctorRunningLate,
	SubjectConsultationEarlyJoinOffered,
	SubjectPrescriptionIssued,
	SubjectAdminUserSuspendRequested, SubjectAdminUserReinstateRequested,
	SubjectAdminAppointmentForceCancel, SubjectAdminDoubleBookingResolveRequested,
	SubjectAdminRefundApproved, SubjectAdminPayoutBatchRequested,
}

// Envelope is the wire format for every event. It is versioned from day one:
// in 2040 a consumer must be able to tell a v1 payload from a v3 one without
// guessing from field presence.
type Envelope struct {
	ID          uuid.UUID       `json:"id"`
	Subject     Subject         `json:"subject"`
	Version     int             `json:"version"`
	OccurredAt  time.Time       `json:"occurred_at"`
	Producer    string          `json:"producer"`
	TraceID     string          `json:"trace_id,omitempty"`
	AggregateID string          `json:"aggregate_id,omitempty"`
	Payload     json.RawMessage `json:"payload"`
}

// NewEnvelope marshals payload into a versioned envelope.
func NewEnvelope(subject Subject, producer, aggregateID string, payload any) (Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("events: marshal payload for %s: %w", subject, err)
	}
	return Envelope{
		ID:          uuid.New(),
		Subject:     subject,
		Version:     1,
		OccurredAt:  time.Now().UTC(),
		Producer:    producer,
		AggregateID: aggregateID,
		Payload:     raw,
	}, nil
}

// Decode unmarshals the envelope payload into dst.
func (e Envelope) Decode(dst any) error {
	if err := json.Unmarshal(e.Payload, dst); err != nil {
		return fmt.Errorf("events: decode %s payload: %w", e.Subject, err)
	}
	return nil
}

// Publisher sends an already-committed event to the broker. Business code does
// not call this directly -- it writes to the outbox and the relay calls it.
type Publisher interface {
	Publish(ctx context.Context, env Envelope) error
	Close() error
}

// Handler processes one delivered event. Returning nil acknowledges it;
// returning an error triggers a redelivery with backoff.
type Handler func(ctx context.Context, env Envelope) error

// Subscriber consumes events with a durable, at-least-once subscription.
type Subscriber interface {
	Subscribe(ctx context.Context, durable string, subjects []Subject, h Handler) error
	Close() error
}
