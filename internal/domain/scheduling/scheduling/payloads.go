package scheduling

import (
	"github.com/google/uuid"
)

// Event payloads.
//
// Almost everything that used to be declared here is gone, and its absence is
// the fix. This file previously held nine private structs -- one per event this
// service publishes or consumes -- each of which was also declared, separately
// and unexported, inside every consumer. Two declarations of the same contract
// with no compiler between them is not a contract, and the platform paid for
// that: this service published appointment.created without amount_cents while
// payment-service required it, and every booking was silently discarded.
//
// Producers and consumers now share the canonical structs in
// telemed/internal/platform/events. A field one side adds and the other does
// not is a compile error there, not a zero value at 3am here.
//
// What is published, and with which canonical type:
//
//	slot.generated          events.SlotsGenerated
//	slot.booked             events.SlotBooked
//	slot.released           events.SlotReleased
//	appointment.created     events.AppointmentCreated
//	appointment.confirmed   events.AppointmentConfirmed
//	appointment.cancelled   events.AppointmentCancelled
//	appointment.completed   events.AppointmentTerminal
//	appointment.no_show     events.AppointmentTerminal
//	waitlist.joined         events.WaitlistJoined
//	waitlist.slot_offered   events.WaitlistSlotOffered
//
// What is consumed:
//
//	doctor.approved         events.DoctorApproved  (+ DoctorScheduleHints, below)
//	doctor.updated          events.DoctorUpdated
//	payment.succeeded       PaymentResultPayload   (see below)
//	payment.failed          PaymentResultPayload
//
// Only the two types below remain, and each remains for a stated reason.

// DefaultCurrency is the platform's only currency today. It fills in for a
// producer that omits the field, so a missing currency never becomes a booking
// priced in nothing.
const DefaultCurrency = "LKR"

// DoctorScheduleHints carries the OPTIONAL scheduling preferences that may ride
// along on doctor.approved.
//
// These fields are not on events.DoctorApproved. doctor-service owns working
// hours, but the canonical payload does not carry them, so today this decodes
// to nothing and a newly approved doctor gets DefaultScheduleSettings with no
// working hours -- which is exactly what happened before this reconciliation
// too, since the old producer sent only {doctor_id, user_id}.
//
// It is kept, rather than deleted along with the other private payloads,
// because deleting it would remove a working code path in order to make a file
// tidier. Adding these fields to events.DoctorApproved is additive and safe
// under the versioning rule in events/payloads.go, and when that happens this
// type can go. Until then it is decoded from the same bytes as the canonical
// payload and is authoritative for nothing.
type DoctorScheduleHints struct {
	DoctorID uuid.UUID `json:"doctor_id"`
	Timezone string    `json:"timezone,omitempty"`

	SlotDurationMinutes int `json:"slot_duration_minutes,omitempty"`
	// BufferMinutes is a pointer because zero is a meaningful value: a doctor
	// who wants back-to-back consultations is saying something different from a
	// doctor whose preferences have not arrived. Treating 0 as "unset" would
	// silently give them the 5 minute default forever.
	BufferMinutes *int `json:"buffer_minutes,omitempty"`
	MaxPerDay     int  `json:"max_per_day,omitempty"`
	AdvanceDays   int  `json:"advance_days,omitempty"`

	WorkingHours []WorkingHourPayload `json:"working_hours,omitempty"`
}

// WorkingHourPayload is one clinic window as doctor-service would publish it.
// Times are "HH:MM" or "HH:MM:SS" wall-clock in the doctor's timezone.
type WorkingHourPayload struct {
	DayOfWeek   int    `json:"day_of_week"`
	StartTime   string `json:"start_time"`
	EndTime     string `json:"end_time"`
	IsAvailable *bool  `json:"is_available,omitempty"`
}

// PaymentResultPayload covers payment.succeeded and payment.failed.
//
// It is NOT the canonical events.PaymentSucceeded / events.PaymentFailed, and
// that is deliberate: those two differ in shape (one carries commission and
// payout, the other a retryable flag), while everything this service does with
// either is "confirm the appointment" or "release the slot". One struct over
// the fields common to both is honest; importing two canonical types to read
// three fields from each would not be clearer.
//
// The json tags here were WRONG before this pass, in the silent way. This
// struct read `amount` and `reason`; payment-service publishes `amount_cents`
// and `failure_reason`. Both fields therefore arrived empty on every single
// payment event, and the slot-release reason fell back to a hardcoded
// "payment_failed" for every failure the platform has ever had. Nothing logged
// an error, because nothing can: encoding/json does not report a field it did
// not find.
//
// Pinned by TestPaymentResultPayloadMatchesProducers.
type PaymentResultPayload struct {
	PaymentID     *uuid.UUID `json:"payment_id,omitempty"`
	AppointmentID uuid.UUID  `json:"appointment_id"`
	Amount        int64      `json:"amount_cents,omitempty"`
	Currency      string     `json:"currency,omitempty"`

	// Two spellings, on purpose. payment-service's PaymentEvent names it
	// failure_reason; the canonical events.PaymentFailed names it reason.
	// Accepting both means this service keeps working through the migration
	// from one to the other, in either direction, instead of silently losing
	// the field for the duration.
	FailureReason string `json:"failure_reason,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// FailureText returns whichever spelling of the failure reason arrived.
func (p PaymentResultPayload) FailureText() string {
	if p.FailureReason != "" {
		return p.FailureReason
	}
	return p.Reason
}
