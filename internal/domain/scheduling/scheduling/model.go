// Package scheduling implements the platform's own slot engine: materialised
// slots, race-free booking, waitlists, and no-show accounting.
//
// It is MIT-licensed original work. Cal.com was evaluated and rejected -- its
// core is AGPLv3, which for a SaaS telemedicine platform means open-sourcing
// the payment and medical-record code alongside it, and its free tier withholds
// exactly the features a Sri Lankan marketplace needs (round-robin, SSO, audit
// logs). See the V2 documentation §4 and docs/DESIGN.md §1.
//
// Layering is strict: handler -> service -> repository. Types in this file are
// plain domain values with no framework imports, so they can be shared with the
// gRPC surface and the event consumers without dragging net/http along.
package scheduling

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// SlotStatus is the lifecycle of a materialised slot.
type SlotStatus string

const (
	// SlotAvailable is bookable by anyone.
	SlotAvailable SlotStatus = "AVAILABLE"
	// SlotBooked has exactly one live appointment against it.
	SlotBooked SlotStatus = "BOOKED"
	// SlotBlocked is withheld from the market: either an admin override or a
	// five-minute waitlist reservation.
	SlotBlocked SlotStatus = "BLOCKED"
	// SlotCancelled is retired permanently -- the doctor withdrew the hour.
	SlotCancelled SlotStatus = "CANCELLED"
)

// Valid reports whether s is a status the database will accept.
func (s SlotStatus) Valid() bool {
	switch s {
	case SlotAvailable, SlotBooked, SlotBlocked, SlotCancelled:
		return true
	default:
		return false
	}
}

// AppointmentStatus is the appointment lifecycle. The strings are the wire
// contract with the payment, notification and consultation services; they are
// lowercase because that is what the source documentation specifies and what
// the other repos were built against.
type AppointmentStatus string

const (
	// AppointmentPendingPayment holds the slot while payment completes.
	AppointmentPendingPayment AppointmentStatus = "pending_payment"
	// AppointmentConfirmed means paid, slot held, consultation may proceed.
	AppointmentConfirmed AppointmentStatus = "confirmed"
	// AppointmentCancelled released the slot back to the market.
	AppointmentCancelled AppointmentStatus = "cancelled"
	// AppointmentCompleted means the consultation happened.
	AppointmentCompleted AppointmentStatus = "completed"
	// AppointmentNoShow means the patient never joined.
	AppointmentNoShow AppointmentStatus = "no_show"
)

// IsLive reports whether the appointment still holds its slot. It mirrors the
// predicate of uq_appointments_slot_live exactly; if one changes the other must.
func (s AppointmentStatus) IsLive() bool {
	switch s {
	case AppointmentPendingPayment, AppointmentConfirmed, AppointmentCompleted, AppointmentNoShow:
		return true
	default:
		return false
	}
}

// RefundPolicy is the signal this service publishes on cancellation. Scheduling
// decides *what the policy says*; the payment service decides what money moves.
// Keeping the decision here means the cancellation window is defined once.
type RefundPolicy string

const (
	// RefundFull is a cancellation outside the notice window, or any
	// cancellation initiated by the doctor or an administrator.
	RefundFull RefundPolicy = "FULL"
	// RefundPartial is a late patient cancellation.
	RefundPartial RefundPolicy = "PARTIAL"
	// RefundNone is a no-show.
	RefundNone RefundPolicy = "NONE"
)

// Percent is the share of the fee the policy returns to the patient.
func (p RefundPolicy) Percent() int {
	switch p {
	case RefundFull:
		return 100
	case RefundPartial:
		return 50
	default:
		return 0
	}
}

// WaitlistStatus is the waitlist entry lifecycle.
type WaitlistStatus string

const (
	// WaitlistWaiting is queued for the next cancellation.
	WaitlistWaiting WaitlistStatus = "waiting"
	// WaitlistNotified has been offered a slot and holds it for five minutes.
	WaitlistNotified WaitlistStatus = "notified"
	// WaitlistBooked took the offer.
	WaitlistBooked WaitlistStatus = "booked"
	// WaitlistExpired let too many offers lapse.
	WaitlistExpired WaitlistStatus = "expired"
	// WaitlistCancelled was withdrawn by the patient.
	WaitlistCancelled WaitlistStatus = "cancelled"
)

// Slot is one bookable interval on one doctor's calendar. Instants are always
// UTC in this struct; Asia/Colombo appears only at the presentation edge.
type Slot struct {
	ID            uuid.UUID
	DoctorID      uuid.UUID
	StartAt       time.Time
	EndAt         time.Time
	Status        SlotStatus
	AppointmentID *uuid.UUID
	ReservedFor   *uuid.UUID
	ReservedUntil *time.Time
	Version       int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// IsReservedFor reports whether the slot is currently held for patientID by a
// live waitlist offer.
func (s Slot) IsReservedFor(patientID uuid.UUID, now time.Time) bool {
	return s.ReservedFor != nil && *s.ReservedFor == patientID &&
		s.ReservedUntil != nil && s.ReservedUntil.After(now)
}

// Bookable reports whether patientID may take this slot right now. A slot
// reserved for someone else is not bookable even though it is not yet BOOKED;
// a slot reserved for *this* patient is bookable even though it is BLOCKED.
func (s Slot) Bookable(patientID uuid.UUID, now time.Time) bool {
	switch s.Status {
	case SlotAvailable:
		return s.ReservedUntil == nil || !s.ReservedUntil.After(now) || s.IsReservedFor(patientID, now)
	case SlotBlocked:
		return s.IsReservedFor(patientID, now)
	default:
		return false
	}
}

// Appointment is a booking. intake is opaque JSON: it carries symptoms and
// allergies, which are PHI, so it is never logged and never included in an
// event payload.
type Appointment struct {
	ID               uuid.UUID
	PatientID        uuid.UUID
	DoctorID         uuid.UUID
	SlotID           uuid.UUID
	SlotStartAt      time.Time
	SlotEndAt        time.Time
	Status           AppointmentStatus
	Intake           json.RawMessage
	FamilyMemberID   *uuid.UUID
	VisitPatientName string
	VisitPatientDOB  *time.Time
	// VisitPatientSex, VisitPatientWeightKg and VisitPatientAllergies are
	// optional booking-time snapshots, like the name and DOB above.
	VisitPatientSex       string
	VisitPatientWeightKg  *float64
	VisitPatientAllergies string
	PrepaymentRequired    bool

	// AmountCents is the QUOTE, in cents, fixed at booking from
	// doctor_pricing. It is not a lookup key and it is never recomputed: a
	// patient who booked at LKR 2,000 is charged LKR 2,000 even if the doctor
	// raises their fee a minute later, and this row is the evidence of what
	// they were told. Zero only ever appears on rows booked before pricing
	// existed -- the booking path refuses to create a new one without a quote.
	AmountCents int64
	// Currency is ISO 4217, captured with the amount so the pair is always
	// interpretable without reading another table.
	Currency string
	// Specialty is denormalised from doctor_pricing at booking. payment-service
	// prices its commission off it, and the commission a patient was quoted
	// under must not change because the doctor later re-specialised.
	Specialty          string
	PaymentID          *uuid.UUID
	ConfirmedAt        *time.Time
	CompletedAt        *time.Time
	NoShowAt           *time.Time
	CancelledAt        *time.Time
	CancelledBy        *uuid.UUID
	CancelledByRole    string
	CancellationReason string
	RefundPolicy       *RefundPolicy
	Version            int
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// DoctorPricing is scheduling's projection of doctor-service's list price.
//
// It exists so that booking can quote a price without a synchronous call into
// doctor-service on the hot path -- a call that would make every booking on the
// platform depend on doctor-service being up, and would still race against a
// fee change mid-checkout.
//
// It is a CACHE OF THE CURRENT LIST PRICE, not a record of anything. The record
// is Appointment.AmountCents, written once at booking. Changing a row here
// changes what the NEXT patient is quoted and nothing else.
type DoctorPricing struct {
	DoctorID  uuid.UUID
	Specialty string
	FeeCents  int64
	Currency  string
	Languages []string
	Status    string

	// LastEventAt is the producer's timestamp for the event that last wrote
	// this row -- doctor.approved's approved_at or doctor.updated's updated_at.
	//
	// It is not decoration. JetStream is at-least-once with no cross-delivery
	// ordering guarantee, so a redelivered doctor.approved can arrive AFTER the
	// doctor.updated that superseded it. Guarding the upsert on this being
	// non-decreasing is what stops that redelivery rolling the fee back to an
	// old value that every subsequent booking would then quote.
	LastEventAt time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Quotable reports whether a booking may be priced from this row.
//
// A zero fee is not a free consultation, it is a projection that never received
// a real price, so it is refused. Status is deliberately NOT checked here: slot
// availability already reflects whether a doctor is taking patients, and
// blocking a booking on a projection's status field would make a lagging event
// look to the patient like a doctor who does not exist.
func (p DoctorPricing) Quotable() bool { return p.FeeCents > 0 }

// WaitlistEntry is one patient waiting for a cancellation on a given day.
type WaitlistEntry struct {
	ID             uuid.UUID
	PatientID      uuid.UUID
	DoctorID       uuid.UUID
	PreferredDate  Date
	Status         WaitlistStatus
	NotifiedAt     *time.Time
	OfferedSlotID  *uuid.UUID
	OfferExpiresAt *time.Time
	OfferCount     int
	// QueuedAt is when the entry last entered the waiting state. It is the
	// queue position, not CreatedAt: an entry that lets an offer lapse goes to
	// the back rather than blocking everyone behind it.
	QueuedAt  time.Time
	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ScheduleSettings is a doctor's generation policy, mirrored from
// doctor-service and defaulted when that service has not told us yet.
type ScheduleSettings struct {
	DoctorID            uuid.UUID
	SlotDurationMinutes int
	BufferMinutes       int
	MaxPerDay           int
	Timezone            string
	OverbookingPercent  int
	AdvanceDays         int
	IsActive            bool
	Version             int
}

// DefaultScheduleSettings returns the policy applied to a doctor whose own
// preferences have not arrived yet. The values match the source documentation's
// worked example: 15 minute consultations with a 5 minute buffer.
func DefaultScheduleSettings(doctorID uuid.UUID) ScheduleSettings {
	return ScheduleSettings{
		DoctorID:            doctorID,
		SlotDurationMinutes: 15,
		BufferMinutes:       5,
		MaxPerDay:           24,
		Timezone:            "Asia/Colombo",
		OverbookingPercent:  0,
		AdvanceDays:         30,
		IsActive:            true,
	}
}

// Step is the distance between two consecutive slot starts: the consultation
// plus the buffer the doctor needs to write notes before the next patient.
func (s ScheduleSettings) Step() time.Duration {
	return time.Duration(s.SlotDurationMinutes+s.BufferMinutes) * time.Minute
}

// Duration is the length of the consultation itself.
func (s ScheduleSettings) Duration() time.Duration {
	return time.Duration(s.SlotDurationMinutes) * time.Minute
}

// EffectiveMaxPerDay applies the overbooking allowance. Overbooking here means
// "accept more appointments in the day", never "sell the same minute twice":
// two patients in one video room is a clinical incident, not a revenue
// optimisation. See docs/DESIGN.md §5.
func (s ScheduleSettings) EffectiveMaxPerDay() int {
	if s.OverbookingPercent <= 0 {
		return s.MaxPerDay
	}
	extra := (s.MaxPerDay*s.OverbookingPercent + 99) / 100 // ceil
	return s.MaxPerDay + extra
}

// WorkingHour is one contiguous clinic window on one weekday, in the doctor's
// own wall-clock time.
type WorkingHour struct {
	ID          uuid.UUID
	DoctorID    uuid.UUID
	DayOfWeek   time.Weekday
	StartMinute int // minutes since local midnight
	EndMinute   int
	IsAvailable bool
}

// NoShowStats are the per-patient counters behind the prepayment rule.
type NoShowStats struct {
	PatientID         uuid.UUID
	TotalAppointments int
	NoShowCount       int
	CompletedCount    int
	CancelledCount    int
	LastNoShowAt      *time.Time
}

// NoShowRate is the fraction of this patient's appointments that ended in a
// no-show, in [0,1]. A patient with no history has a rate of zero.
func (n NoShowStats) NoShowRate() float64 {
	if n.TotalAppointments <= 0 {
		return 0
	}
	return float64(n.NoShowCount) / float64(n.TotalAppointments)
}

// Policy constants. They are here rather than in config because changing them
// changes the product's contract with patients and doctors, and that should be
// a reviewed code change, not an environment variable somebody edits at 2am.
const (
	// CancellationNoticeWindow is the boundary between a full and a partial
	// refund for a patient-initiated cancellation.
	CancellationNoticeWindow = 2 * time.Hour

	// NoShowPrepaymentThreshold is the no-show rate above which a patient must
	// pay before the slot is held.
	NoShowPrepaymentThreshold = 0.30

	// NoShowMinimumHistory is how many past appointments a patient needs before
	// the rate means anything. Without it, one no-show on a first-ever booking
	// reads as a 100% rate and locks a new patient out of the platform.
	NoShowMinimumHistory = 3

	// OverbookingPercent is the extra daily capacity given to doctors whose
	// patients no-show heavily (the Mend model, applied simply).
	OverbookingPercent = 10

	// DoctorOverbookingThreshold is the no-show rate at which a doctor's
	// capacity is raised.
	DoctorOverbookingThreshold = 0.20

	// WaitlistOfferWindow is how long a promoted patient holds the slot.
	WaitlistOfferWindow = 5 * time.Minute

	// WaitlistMaxOffers is how many lapsed offers retire an entry.
	WaitlistMaxOffers = 3

	// UnpaidBookingWindow is how long a pending_payment appointment holds its
	// slot before the sweeper releases it. Long enough for a 3DS challenge on a
	// slow connection, short enough that a stalled checkout does not park a
	// prime-time slot for an hour.
	UnpaidBookingWindow = 15 * time.Minute

	// SlotArchiveAge is how long past slots stay in the live partitions.
	SlotArchiveAge = 90 * 24 * time.Hour

	// SlotLockTTL bounds the cheap Redis early-reject lock. It is deliberately
	// short: it is not a mutex and correctness never depends on it (ADR-007).
	SlotLockTTL = 5 * time.Second

	// PartitionRunwayMonths is how many months of slot partitions the
	// maintenance job keeps ahead of today.
	PartitionRunwayMonths = 12
)

// RefundPolicyFor decides what a cancellation is worth. doctorInitiated covers
// both the doctor and an administrator acting on the doctor's behalf: when the
// platform breaks the appointment, the patient is made whole regardless of
// timing.
func RefundPolicyFor(slotStart, now time.Time, doctorInitiated bool) RefundPolicy {
	if doctorInitiated {
		return RefundFull
	}
	if slotStart.Sub(now) >= CancellationNoticeWindow {
		return RefundFull
	}
	return RefundPartial
}

// RequiresPrepayment applies the >30% rule from the source documentation §9.5.
func RequiresPrepayment(stats NoShowStats) bool {
	if stats.TotalAppointments < NoShowMinimumHistory {
		return false
	}
	return stats.NoShowRate() > NoShowPrepaymentThreshold
}

// RescheduleStatus is the lifecycle of a doctor-requested move.
type RescheduleStatus string

const (
	ReschedulePending  RescheduleStatus = "pending"
	RescheduleAccepted RescheduleStatus = "accepted"
	RescheduleDeclined RescheduleStatus = "declined"
	RescheduleExpired  RescheduleStatus = "expired"
)

// RescheduleRequest is a doctor asking to move one confirmed booking. The
// doctor's reason lives only on this row; it is never published on NATS.
type RescheduleRequest struct {
	ID                  uuid.UUID
	AppointmentID       uuid.UUID
	PatientID           uuid.UUID
	DoctorID            uuid.UUID
	OriginalSlotID      uuid.UUID
	OriginalStartAt     time.Time
	OriginalEndAt       time.Time
	ProposedSlotID      uuid.UUID
	ProposedStartAt     time.Time
	ProposedEndAt       time.Time
	ProposedSlotCreated bool
	Reason              string
	Status              RescheduleStatus
	DecidedBy           *uuid.UUID
	DecidedByRole       string
	DecidedAt           *time.Time
	Version             int
	CreatedAt           time.Time
	UpdatedAt           time.Time
}
