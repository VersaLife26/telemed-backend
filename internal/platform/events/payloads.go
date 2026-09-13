package events

import (
	"time"

	"github.com/google/uuid"
)

// Canonical event payloads.
//
// Every payload on the platform is defined here exactly once, and both the
// producer and every consumer import THIS type. That is the whole point: when
// each side declares its own private struct, a field one side adds and the
// other does not is invisible. encoding/json does not error on a missing field
// -- it leaves the zero value -- so the mismatch surfaces at runtime as an
// empty string or a zero amount, in a code path nobody is watching.
//
// That is not hypothetical. During this build, scheduling-service published
// appointment.created without amount_cents while payment-service required it
// and dropped any event whose amount was zero. Every booking was silently
// discarded and no patient could pay. Both services were individually correct
// and individually well tested. Sharing these types would have made it a
// compile error.
//
// Versioning rule for the next fifty years: payloads are additive only.
// Adding an optional field is safe -- old consumers ignore it. Removing or
// renaming a field, or changing its type, requires a new subject
// (`slot.booked.v2`) and a migration window in which both are published. Never
// repurpose a field name; a 2041 consumer will still be decoding 2026 events
// out of a replayed stream.

// ─── user ────────────────────────────────────────────────────────────────────

// UserRegistered announces a new account. It deliberately carries no NIC and no
// full phone number: this event fans out to every consumer, and PHI-adjacent
// identifiers should be fetched deliberately, not broadcast.
type UserRegistered struct {
	UserID    uuid.UUID `json:"user_id"`
	Role      string    `json:"role"` // patient | doctor | admin
	Language  string    `json:"language"`
	CreatedAt time.Time `json:"created_at"`
}

// UserStatusChanged backs both user.suspended and user.reinstated.
//
// Status is carried explicitly rather than inferred from the subject name, so
// a consumer that filters on one subject and a consumer that filters on both
// read the same fact the same way.
type UserStatusChanged struct {
	UserID    uuid.UUID `json:"user_id"`
	Status    string    `json:"status"` // active | suspended
	Reason    string    `json:"reason,omitempty"`
	ActorID   uuid.UUID `json:"actor_id"`
	ChangedAt time.Time `json:"changed_at"`
}

// AdminUserStatusRequested backs admin.user_suspend_requested and
// admin.user_reinstate_requested.
//
// These are COMMANDS, not facts (see the admin.* block in events.go). The
// admin service owns no account data, so it asks user-service -- which does --
// to apply the change. user-service applies it, and publishes
// user.suspended / user.reinstated back as the fact. Nothing is true until
// that return trip happens: an admin console that renders the command's 202 as
// "suspended" is lying to the operator.
//
// The wire shape is deliberately identical to the private struct admin-service
// published before this type existed, so events already sitting in a replayed
// stream still decode.
type AdminUserStatusRequested struct {
	UserID uuid.UUID `json:"user_id"`
	Reason string    `json:"reason,omitempty"`
	// AdminID is the admin who asked. It is recorded on the resulting
	// user.suspended/user.reinstated as ActorID, so the fact carries its
	// author and the audit trail survives the hop between services.
	AdminID uuid.UUID `json:"admin_id"`
}

// ─── doctor ──────────────────────────────────────────────────────────────────

// DoctorCredentialDocuments is the set of object keys the admin verification
// queue must be able to open: the SLMC certificate, the NIC, the degree
// certificate and the identifying photograph. They live in the
// `doctor-credentials` bucket and admin-service presigns them, briefly, at
// read time -- the keys are safe to carry on an event, the bytes are not.
//
// This is embedded (not nested) so the four keys appear as flat top-level
// fields on the wire, which is how admin-service's doctor_projection columns
// are already named.
//
// It exists as its own type because the same four keys travel on two subjects:
// doctor.registered, which announces the application, and
// doctor.documents_updated, which announces a document arriving afterwards.
// Both matter: a doctor registers first and uploads credentials second, so an
// event fired only at registration would always show an empty folder.
type DoctorCredentialDocuments struct {
	SLMCCertificateKey   string `json:"slmc_certificate_key,omitempty"`
	NICDocumentKey       string `json:"nic_document_key,omitempty"`
	DegreeCertificateKey string `json:"degree_certificate_key,omitempty"`
	PhotoKey             string `json:"photo_key,omitempty"`
}

// Empty reports whether no credential document has been submitted yet.
func (d DoctorCredentialDocuments) Empty() bool {
	return d.SLMCCertificateKey == "" && d.NICDocumentKey == "" &&
		d.DegreeCertificateKey == "" && d.PhotoKey == ""
}

// DoctorRegistered announces a credentialing application.
//
// Its one consumer that matters is admin-service's verification queue, and
// that queue is the flagship admin screen: a human reviewer decides whether a
// doctor may treat patients. It therefore has to carry what the reviewer must
// see. It previously carried four fields against the eleven the queue
// declared, so every application projected as
// {"full_name":"","years_experience":0,"registered_at":"0001-01-01"} with no
// attachments -- an unusable screen, and nothing logged an error, because
// encoding/json does not complain about a field nobody sent.
//
// FullName is the display name on the APPLICATION, which doctor-service owns.
// It is not a duplicate of the user record: admin-service still resolves email
// and phone from user-service over gRPC, because those are contact details
// that change, whereas the name a doctor applied under is a fact about the
// application and must not silently change under a reviewer mid-review.
type DoctorRegistered struct {
	DoctorID        uuid.UUID `json:"doctor_id"`
	UserID          uuid.UUID `json:"user_id"`
	FullName        string    `json:"full_name,omitempty"`
	SLMCNumber      string    `json:"slmc_number"`
	Specialty       string    `json:"specialty"`
	YearsExperience int       `json:"years_experience,omitempty"`

	DoctorCredentialDocuments

	CreatedAt time.Time `json:"created_at"`
}

// DoctorDocumentsUpdated announces that a doctor's credential document set has
// changed -- a new upload, or a replacement of one already there.
//
// It exists because registration and credential upload are two separate calls,
// in that order: POST /doctors/register creates the application, and each
// POST /doctors/me/documents attaches one file to it. Without this subject the
// only event carrying document keys is fired before any document exists, and
// the reviewer's document viewer is permanently empty.
//
// It carries the FULL current key set, not a delta. A consumer that misses one
// delivery and receives the next is still correct, and a replay in any order
// converges -- which a delta would not.
type DoctorDocumentsUpdated struct {
	DoctorID uuid.UUID `json:"doctor_id"`
	UserID   uuid.UUID `json:"user_id"`

	DoctorCredentialDocuments

	UpdatedAt time.Time `json:"updated_at"`
}

// WorkingHour is one recurring availability window, in the doctor's local
// wall-clock time. Times are "HH:MM" strings, not timestamps: a doctor works
// 09:00-12:00 every Tuesday regardless of what UTC offset that lands on.
type WorkingHour struct {
	DayOfWeek   int    `json:"day_of_week"` // 0=Sunday .. 6=Saturday
	StartTime   string `json:"start_time"`  // "09:00"
	EndTime     string `json:"end_time"`    // "12:00"
	IsAvailable *bool  `json:"is_available,omitempty"`
}

// DoctorApproved announces that a doctor may now accept patients.
//
// FeeCents and Specialty are on this event because scheduling-service needs to
// quote a price at booking time and must not call into doctor-service on the
// booking hot path. See DoctorUpdated for how the quote stays current.
type DoctorApproved struct {
	DoctorID   uuid.UUID `json:"doctor_id"`
	UserID     uuid.UUID `json:"user_id"`
	DoctorName string    `json:"doctor_name"`
	Email      string    `json:"email,omitempty"`
	Specialty  string    `json:"specialty"`
	FeeCents   int64     `json:"fee_cents"`
	Currency   string    `json:"currency"` // ISO 4217, "LKR"
	Languages  []string  `json:"languages,omitempty"`

	// WorkingHours is what makes the doctor bookable. Without it,
	// scheduling-service has nothing to slice into slots: it creates default
	// schedule settings, generates zero slots, and the doctor is approved but
	// permanently unbookable -- with nothing logged to say why.
	WorkingHours []WorkingHour `json:"working_hours,omitempty"`
	Timezone     string        `json:"timezone,omitempty"`

	SlotDurationMinutes int  `json:"slot_duration_minutes,omitempty"`
	BufferMinutes       *int `json:"buffer_minutes,omitempty"`
	MaxPerDay           int  `json:"max_per_day,omitempty"`

	ApprovedAt time.Time `json:"approved_at"`
}

// DoctorRejected announces a failed credentialing decision.
type DoctorRejected struct {
	DoctorID   uuid.UUID `json:"doctor_id"`
	UserID     uuid.UUID `json:"user_id"`
	DoctorName string    `json:"doctor_name"`
	Email      string    `json:"email,omitempty"`
	Reason     string    `json:"reason"`
	RejectedAt time.Time `json:"rejected_at"`
}

// DoctorApplicationSubmitted announces a public apply (no user account yet).
// Contact fields live on the payload because there is no user_id to resolve.
type DoctorApplicationSubmitted struct {
	ApplicationID   uuid.UUID `json:"application_id"`
	FullName        string    `json:"full_name"`
	Email           string    `json:"email"`
	Phone           string    `json:"phone"`
	SLMCNumber      string    `json:"slmc_number"`
	Specialty       string    `json:"specialty"`
	YearsExperience int       `json:"years_experience"`
	FeeCents        int64     `json:"fee_cents"`
	Languages       []string  `json:"languages,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// DoctorApplicationApproved tells user-service to provision the doctor account.
type DoctorApplicationApproved struct {
	ApplicationID uuid.UUID `json:"application_id"`
	FullName      string    `json:"full_name"`
	Email         string    `json:"email"`
	Phone         string    `json:"phone"`
	Specialty     string    `json:"specialty"`
	ApprovedAt    time.Time `json:"approved_at"`
}

// DoctorApplicationRejected tells the applicant their application was refused.
type DoctorApplicationRejected struct {
	ApplicationID uuid.UUID `json:"application_id"`
	FullName      string    `json:"full_name"`
	Email         string    `json:"email"`
	Phone         string    `json:"phone"`
	Reason        string    `json:"reason"`
	RejectedAt    time.Time `json:"rejected_at"`
}

// DoctorUpdated announces a change to a doctor's public profile.
//
// This is what keeps scheduling-service's pricing projection from going stale.
// A doctor who raises their fee must not have that price applied to bookings
// already quoted at the old one -- scheduling stores the quote on the
// appointment, and this event only updates the list price used for FUTURE
// quotes.
type DoctorUpdated struct {
	DoctorID  uuid.UUID `json:"doctor_id"`
	Specialty string    `json:"specialty"`
	FeeCents  int64     `json:"fee_cents"`
	Currency  string    `json:"currency"`
	Languages []string  `json:"languages,omitempty"`
	Status    string    `json:"status"` // approved | suspended | ...

	// WorkingHours is sent whenever the doctor edits their availability, so
	// scheduling can regenerate future slots from the new pattern.
	WorkingHours []WorkingHour `json:"working_hours,omitempty"`
	Timezone     string        `json:"timezone,omitempty"`

	SlotDurationMinutes int  `json:"slot_duration_minutes,omitempty"`
	BufferMinutes       *int `json:"buffer_minutes,omitempty"`
	MaxPerDay           int  `json:"max_per_day,omitempty"`

	UpdatedAt time.Time `json:"updated_at"`
}

// ─── slots ───────────────────────────────────────────────────────────────────

// SlotsGenerated announces a batch of newly materialised slots for one doctor.
type SlotsGenerated struct {
	DoctorID uuid.UUID `json:"doctor_id"`
	FromDate string    `json:"from_date"` // YYYY-MM-DD, Asia/Colombo
	ToDate   string    `json:"to_date"`
	Count    int       `json:"count"`
}

// SlotBooked and SlotReleased share a shape; a slot returning to AVAILABLE is
// the same fact as it leaving, read the other way.
type SlotBooked struct {
	SlotID        uuid.UUID `json:"slot_id"`
	DoctorID      uuid.UUID `json:"doctor_id"`
	AppointmentID uuid.UUID `json:"appointment_id"`
	StartAt       time.Time `json:"start_at"`
	EndAt         time.Time `json:"end_at"`
}

// SlotReleased announces a slot returning to AVAILABLE.
type SlotReleased struct {
	SlotID   uuid.UUID `json:"slot_id"`
	DoctorID uuid.UUID `json:"doctor_id"`
	StartAt  time.Time `json:"start_at"`
	EndAt    time.Time `json:"end_at"`
	Reason   string    `json:"reason,omitempty"` // cancelled | expired | admin_block_lifted
}

// SlotWithdrawn announces a slot removed from sale for good -- today, because
// the doctor registered leave over it. Distinct from SlotReleased on purpose:
// a consumer mirroring slot state must not put it back on the market.
type SlotWithdrawn struct {
	SlotID   uuid.UUID `json:"slot_id"`
	DoctorID uuid.UUID `json:"doctor_id"`
	StartAt  time.Time `json:"start_at"`
	EndAt    time.Time `json:"end_at"`
	Reason   string    `json:"reason,omitempty"` // holiday
}

// ─── appointments ────────────────────────────────────────────────────────────

// AppointmentCreated announces a held, unpaid booking.
//
// AmountCents is the QUOTE, fixed at booking. It is not a lookup key and must
// never be recomputed downstream: a patient who booked at LKR 2,000 is charged
// LKR 2,000 even if the doctor raises their fee a minute later.
//
// An AppointmentCreated with AmountCents == 0 is a bug in the producer, not a
// free consultation. Consumers should reject it loudly.
type AppointmentCreated struct {
	AppointmentID   uuid.UUID `json:"appointment_id"`
	PatientID       uuid.UUID `json:"patient_id"`
	DoctorID        uuid.UUID `json:"doctor_id"`
	SlotID          uuid.UUID `json:"slot_id"`
	StartAt         time.Time `json:"start_at"`
	EndAt           time.Time `json:"end_at"`
	Status          string    `json:"status"`
	AmountCents     int64     `json:"amount_cents"`
	Currency        string    `json:"currency"`
	Specialty       string    `json:"specialty"`
	CorporateClient string    `json:"corporate_client,omitempty"`
	// PrepaymentRequired is set for patients over the no-show threshold. The
	// payment service must refuse pay-on-completion for these.
	PrepaymentRequired bool `json:"prepayment_required"`
	// ExpiresAt is when the unpaid-booking sweeper releases the slot. The
	// payment service sizes its checkout timeout from this.
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

// AppointmentConfirmed announces a paid, held appointment.
type AppointmentConfirmed struct {
	AppointmentID uuid.UUID `json:"appointment_id"`
	PatientID     uuid.UUID `json:"patient_id"`
	DoctorID      uuid.UUID `json:"doctor_id"`
	SlotID        uuid.UUID `json:"slot_id"`
	StartAt       time.Time `json:"start_at"`
	EndAt         time.Time `json:"end_at"`
	PaymentID     uuid.UUID `json:"payment_id"`
	ConfirmedAt   time.Time `json:"confirmed_at"`
}

// AppointmentCancelled announces a cancellation.
//
// CancelledAt is the moment the cancellation was REQUESTED, not the moment the
// event was consumed. The refund is priced off this field: pricing off "now"
// silently converts a full refund into a partial one whenever the event sits in
// a queue past the policy boundary.
//
// RefundPercent is decided by scheduling, which owns the cancellation policy.
// Payment must not re-derive it, or the policy exists in two places and drifts.
type AppointmentCancelled struct {
	AppointmentID uuid.UUID `json:"appointment_id"`
	PatientID     uuid.UUID `json:"patient_id"`
	DoctorID      uuid.UUID `json:"doctor_id"`
	SlotID        uuid.UUID `json:"slot_id"`
	StartAt       time.Time `json:"start_at"`
	CancelledBy   string    `json:"cancelled_by"` // patient | doctor | system | admin
	Reason        string    `json:"reason,omitempty"`
	NoShow        bool      `json:"no_show"`
	RefundPolicy  string    `json:"refund_policy"`  // full | partial | none
	RefundPercent int       `json:"refund_percent"` // 0-100
	CancelledAt   time.Time `json:"cancelled_at"`
}

// AppointmentRescheduleRequested announces a pending move. Reason prose is
// deliberately absent: it is caller-authored and lives only on the scheduling
// row, the same rule as appointment.cancelled.
type AppointmentRescheduleRequested struct {
	RequestID      uuid.UUID `json:"request_id"`
	AppointmentID  uuid.UUID `json:"appointment_id"`
	PatientID      uuid.UUID `json:"patient_id"`
	DoctorID       uuid.UUID `json:"doctor_id"`
	OriginalSlotID uuid.UUID `json:"original_slot_id"`
	OriginalStart  time.Time `json:"original_start_at"`
	OriginalEnd    time.Time `json:"original_end_at"`
	ProposedSlotID uuid.UUID `json:"proposed_slot_id"`
	ProposedStart  time.Time `json:"proposed_start_at"`
	ProposedEnd    time.Time `json:"proposed_end_at"`
	RequestedAt    time.Time `json:"requested_at"`
}

// AppointmentRescheduled announces that a paid appointment now occupies a
// different slot. PaymentID is unchanged; there is no refund.
type AppointmentRescheduled struct {
	RequestID      uuid.UUID `json:"request_id"`
	AppointmentID  uuid.UUID `json:"appointment_id"`
	PatientID      uuid.UUID `json:"patient_id"`
	DoctorID       uuid.UUID `json:"doctor_id"`
	OriginalSlotID uuid.UUID `json:"original_slot_id"`
	OriginalStart  time.Time `json:"original_start_at"`
	ProposedSlotID uuid.UUID `json:"proposed_slot_id"`
	ProposedStart  time.Time `json:"proposed_start_at"`
	ProposedEnd    time.Time `json:"proposed_end_at"`
	DecidedByRole  string    `json:"decided_by_role"`
	RescheduledAt  time.Time `json:"rescheduled_at"`
}

// AppointmentTerminal backs appointment.completed and appointment.no_show.
type AppointmentTerminal struct {
	AppointmentID uuid.UUID `json:"appointment_id"`
	PatientID     uuid.UUID `json:"patient_id"`
	DoctorID      uuid.UUID `json:"doctor_id"`
	StartAt       time.Time `json:"start_at"`
	OccurredAt    time.Time `json:"occurred_at"`
}

// AppointmentReminderDue is emitted by the scheduler ahead of an appointment.
type AppointmentReminderDue struct {
	AppointmentID uuid.UUID `json:"appointment_id"`
	PatientID     uuid.UUID `json:"patient_id"`
	DoctorID      uuid.UUID `json:"doctor_id"`
	DoctorName    string    `json:"doctor_name,omitempty"`
	StartAt       time.Time `json:"start_at"`
	LeadMinutes   int       `json:"lead_minutes"` // 60 | 1440
}

// ─── waitlist ────────────────────────────────────────────────────────────────

// WaitlistSlotOffered announces a time-boxed reservation for a waiting patient.
type WaitlistSlotOffered struct {
	WaitlistID uuid.UUID `json:"waitlist_id"`
	PatientID  uuid.UUID `json:"patient_id"`
	DoctorID   uuid.UUID `json:"doctor_id"`
	SlotID     uuid.UUID `json:"slot_id"`
	StartAt    time.Time `json:"start_at"`
	// ExpiresAt is when the hold lapses and the offer passes to the next
	// patient. Notification copy depends on it, so it is not optional.
	ExpiresAt time.Time `json:"expires_at"`
}

// ─── payments ────────────────────────────────────────────────────────────────

// PaymentSucceeded announces a settled payment.
type PaymentSucceeded struct {
	PaymentID       uuid.UUID `json:"payment_id"`
	AppointmentID   uuid.UUID `json:"appointment_id"`
	PatientID       uuid.UUID `json:"patient_id"`
	DoctorID        uuid.UUID `json:"doctor_id"`
	AmountCents     int64     `json:"amount_cents"`
	Currency        string    `json:"currency"`
	CommissionCents int64     `json:"commission_cents"`
	PayoutCents     int64     `json:"payout_cents"`
	Provider        string    `json:"provider"` // stripe | payhere | dialog
	SucceededAt     time.Time `json:"succeeded_at"`
}

// PaymentFailed announces a failed attempt.
type PaymentFailed struct {
	PaymentID     uuid.UUID `json:"payment_id"`
	AppointmentID uuid.UUID `json:"appointment_id"`
	PatientID     uuid.UUID `json:"patient_id"`
	AmountCents   int64     `json:"amount_cents"`
	Currency      string    `json:"currency"`
	Provider      string    `json:"provider"`
	Reason        string    `json:"reason"`
	Retryable     bool      `json:"retryable"`
	FailedAt      time.Time `json:"failed_at"`
}

// PaymentRefunded announces a refund.
type PaymentRefunded struct {
	RefundID      uuid.UUID `json:"refund_id"`
	PaymentID     uuid.UUID `json:"payment_id"`
	AppointmentID uuid.UUID `json:"appointment_id"`
	PatientID     uuid.UUID `json:"patient_id"`
	AmountCents   int64     `json:"amount_cents"`
	Currency      string    `json:"currency"`
	Reason        string    `json:"reason"`
	RefundedAt    time.Time `json:"refunded_at"`
}

// PayoutSent announces a settled doctor payout.
type PayoutSent struct {
	PayoutID    uuid.UUID `json:"payout_id"`
	DoctorID    uuid.UUID `json:"doctor_id"`
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
	PeriodStart string    `json:"period_start"` // YYYY-MM-DD
	PeriodEnd   string    `json:"period_end"`
	TransferID  string    `json:"transfer_id,omitempty"`
	SentAt      time.Time `json:"sent_at"`
}

// ─── consultations ───────────────────────────────────────────────────────────

// ConsultationStarted announces a call going live.
type ConsultationStarted struct {
	ConsultationID uuid.UUID `json:"consultation_id"`
	AppointmentID  uuid.UUID `json:"appointment_id"`
	PatientID      uuid.UUID `json:"patient_id"`
	DoctorID       uuid.UUID `json:"doctor_id"`
	RoomName       string    `json:"room_name"`
	StartedAt      time.Time `json:"started_at"`
}

// ConsultationDoctorRunningLate asks notification to ping the next patient
// while this doctor is still finishing the previous visit.
type ConsultationDoctorRunningLate struct {
	ActiveConsultationID uuid.UUID `json:"active_consultation_id"`
	ActiveAppointmentID  uuid.UUID `json:"active_appointment_id"`
	NextAppointmentID    uuid.UUID `json:"next_appointment_id"`
	NextPatientID        uuid.UUID `json:"next_patient_id"`
	DoctorID             uuid.UUID `json:"doctor_id"`
	MinutesLate          int       `json:"minutes_late"`
	DetectedAt           time.Time `json:"detected_at"`
}

// ConsultationEarlyJoinOffered asks notification to ping the next patient
// because the doctor finished the previous visit early. PHI is the doctor
// id only; notification resolves a display name. No slot times are moved.
type ConsultationEarlyJoinOffered struct {
	SourceConsultationID uuid.UUID `json:"source_consultation_id"`
	SourceAppointmentID  uuid.UUID `json:"source_appointment_id"`
	NextConsultationID   uuid.UUID `json:"next_consultation_id"`
	NextAppointmentID    uuid.UUID `json:"next_appointment_id"`
	NextPatientID        uuid.UUID `json:"next_patient_id"`
	DoctorID             uuid.UUID `json:"doctor_id"`
	OfferedAt            time.Time `json:"offered_at"`
}

// ConsultationPatientNoShow asks scheduling to close a confirmed appointment
// because the patient never joined after the late-join window. No later slots
// are moved.
type ConsultationPatientNoShow struct {
	ConsultationID uuid.UUID `json:"consultation_id"`
	AppointmentID  uuid.UUID `json:"appointment_id"`
	PatientID      uuid.UUID `json:"patient_id"`
	DoctorID       uuid.UUID `json:"doctor_id"`
	ScheduledAt    time.Time `json:"scheduled_at"`
	DetectedAt     time.Time `json:"detected_at"`
}

// ConsultationEnded announces a call finishing.
//
// EndReason distinguishes a completed consultation from an abandoned one, which
// is what lets scheduling decide between appointment.completed and
// appointment.no_show without guessing from duration.
type ConsultationEnded struct {
	ConsultationID  uuid.UUID `json:"consultation_id"`
	AppointmentID   uuid.UUID `json:"appointment_id"`
	PatientID       uuid.UUID `json:"patient_id"`
	DoctorID        uuid.UUID `json:"doctor_id"`
	DurationSeconds int       `json:"duration_seconds"`
	EndReason       string    `json:"end_reason"` // completed | abandoned | failed | no_show
	Recorded        bool      `json:"recorded"`
	EndedAt         time.Time `json:"ended_at"`
}

// ─── records ─────────────────────────────────────────────────────────────────

// PrescriptionIssued announces an e-prescription. It carries no drug names:
// this event fans out to notification, analytics and audit consumers, and the
// contents of a prescription are exactly the kind of PHI that must be fetched
// deliberately, with an access-log entry, rather than broadcast.
type PrescriptionIssued struct {
	PrescriptionID uuid.UUID `json:"prescription_id"`
	AppointmentID  uuid.UUID `json:"appointment_id"`
	PatientID      uuid.UUID `json:"patient_id"`
	DoctorID       uuid.UUID `json:"doctor_id"`
	ItemCount      int       `json:"item_count"`
	IssuedAt       time.Time `json:"issued_at"`
}

// ─── notification ────────────────────────────────────────────────────────────

// ─── admin commands ──────────────────────────────────────────────────────────
//
// These four are COMMANDS, not statements of fact: the admin domain decides
// something should happen and another domain is what makes it happen. They are
// declared here, with every other canonical payload, for the ordinary reason --
// a consumer in payment or scheduling must not import the admin domain to
// learn the shape of a message.
//
// Each one is named for the request, not the outcome. admin.refund_approved
// means an administrator approved a refund; whether the rail accepted it is
// payment.refunded's job to say.

// AdminAppointmentForceCancelRequested asks scheduling to cancel an
// appointment on an administrator's authority, including one whose slot has
// already started.
type AdminAppointmentForceCancelRequested struct {
	AppointmentID uuid.UUID `json:"appointment_id"`
	Reason        string    `json:"reason"`
	AdminID       uuid.UUID `json:"admin_id"`
}

// AdminDoubleBookingResolveRequested asks scheduling to keep one appointment
// and cancel the conflicting other.
type AdminDoubleBookingResolveRequested struct {
	KeepAppointmentID   uuid.UUID `json:"keep_appointment_id"`
	CancelAppointmentID uuid.UUID `json:"cancel_appointment_id"`
	Reason              string    `json:"reason"`
	AdminID             uuid.UUID `json:"admin_id"`
}

// AdminPayoutBatchRequested asks payment to run a settlement pass.
//
// From and To describe the window the administrator was looking at when they
// pressed the button. PayoutRunner settles everything that is due rather than
// a window, so they are carried for the audit trail and not as instructions.
type AdminPayoutBatchRequested struct {
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	AdminID uuid.UUID `json:"admin_id"`
}

// AdminRefundApproved asks payment to return money for a specific payment.
//
// AmountCents is an explicit override decided by a human, so it bypasses the
// cancellation policy -- that is the entire point of an approved refund.
type AdminRefundApproved struct {
	PaymentID   uuid.UUID `json:"payment_id"`
	AmountCents int64     `json:"amount_cents"`
	Reason      string    `json:"reason"`
	AdminID     uuid.UUID `json:"admin_id"`
}
