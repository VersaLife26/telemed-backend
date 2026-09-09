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
	SubjectOTPRequested     Subject = "user.otp_requested"
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

	SubjectWaitlistJoined    Subject = "waitlist.joined"
	SubjectWaitlistSlotOffer Subject = "waitlist.slot_offered"

	SubjectPaymentSucceeded Subject = "payment.succeeded"
	SubjectPaymentFailed    Subject = "payment.failed"
	SubjectPaymentRefunded  Subject = "payment.refunded"
	SubjectPayoutSent       Subject = "payout.sent"

	SubjectConsultationStarted Subject = "consultation.started"
	SubjectConsultationEnded   Subject = "consultation.ended"

	SubjectPrescriptionIssued    Subject = "prescription.issued"
	SubjectRecordUploaded        Subject = "record.uploaded"
	SubjectClinicalNoteFinalised Subject = "clinical_note.finalised"

	SubjectNotificationRequested Subject = "notification.requested"

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

	SubjectContentSpecialtyUpdated Subject = "content.specialty_updated"
	SubjectContentSymptomUpdated   Subject = "content.symptom_updated"
	SubjectContentDrugUpdated      Subject = "content.drug_updated"
)

// AllSubjects is used by the infra bootstrap to declare the JetStream stream
// and by tests to assert no service publishes an undeclared subject.
var AllSubjects = []Subject{
	SubjectUserRegistered, SubjectUserSuspended, SubjectUserReinstated, SubjectOTPRequested,
	SubjectDoctorRegistered, SubjectDoctorApproved, SubjectDoctorRejected, SubjectDoctorUpdated,
	SubjectDoctorDocumentsUpdated,
	SubjectDoctorApplicationSubmitted, SubjectDoctorApplicationApproved, SubjectDoctorApplicationRejected,
	SubjectSlotsGenerated, SubjectSlotBooked, SubjectSlotReleased, SubjectSlotWithdrawn,
	SubjectAppointmentCreated, SubjectAppointmentConfirmed, SubjectAppointmentCancelled,
	SubjectAppointmentCompleted, SubjectAppointmentNoShow, SubjectAppointmentReminder,
	SubjectWaitlistJoined, SubjectWaitlistSlotOffer,
	SubjectPaymentSucceeded, SubjectPaymentFailed, SubjectPaymentRefunded, SubjectPayoutSent,
	SubjectConsultationStarted, SubjectConsultationEnded,
	SubjectPrescriptionIssued, SubjectRecordUploaded, SubjectClinicalNoteFinalised,
	SubjectNotificationRequested,
	SubjectAdminUserSuspendRequested, SubjectAdminUserReinstateRequested,
	SubjectAdminAppointmentForceCancel, SubjectAdminDoubleBookingResolveRequested,
	SubjectAdminRefundApproved, SubjectAdminPayoutBatchRequested,
	SubjectContentSpecialtyUpdated, SubjectContentSymptomUpdated, SubjectContentDrugUpdated,
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
