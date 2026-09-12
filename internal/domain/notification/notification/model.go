// Package notification implements telemed-notification-service: multi-channel,
// trilingual, preference-aware notification delivery. It is mostly an event
// consumer -- the platform's mouth, not one of its decision makers.
package notification

import (
	"time"

	"github.com/google/uuid"
)

// Channel is a delivery channel. It is also the column value stored on
// notifications, templates and preferences, so keep the four values in sync
// across all three.
type Channel string

const (
	ChannelSMS   Channel = "sms"
	ChannelPush  Channel = "push"
	ChannelEmail Channel = "email"
	ChannelInApp Channel = "in_app"
)

// Locale is one of the platform's three supported languages.
type Locale string

const (
	LocaleEnglish Locale = "en"
	LocaleSinhala Locale = "si"
	LocaleTamil   Locale = "ta"
)

// ValidLocale reports whether l is one of the three supported locales.
func ValidLocale(l Locale) bool {
	switch l {
	case LocaleEnglish, LocaleSinhala, LocaleTamil:
		return true
	default:
		return false
	}
}

// Urgency classifies how a template's send should treat quiet hours and
// channel preferences. It is a property of the template (booking_confirmed
// is never urgent; reminder_1h always is), not of an individual send.
type Urgency string

const (
	// UrgencyCritical bypasses both channel preference and quiet hours. There
	// is exactly one legitimate use of this on the platform today: otp_code.
	// A user cannot "opt out" of the code that lets them log in.
	UrgencyCritical Urgency = "critical"
	// UrgencyUrgent bypasses quiet hours but still respects the user having
	// turned a channel off entirely. reminder_1h, waitlist_available and
	// consultation_starting are urgent: a slot offer that waits for morning
	// is a slot offer given to someone else.
	UrgencyUrgent Urgency = "urgent"
	// UrgencyNormal respects both quiet hours and channel preference. Most
	// templates are normal.
	UrgencyNormal Urgency = "normal"
)

// Status is the lifecycle state of a notification row.
type Status string

const (
	StatusQueued     Status = "queued"
	StatusSending    Status = "sending"
	StatusSent       Status = "sent"
	StatusDelivered  Status = "delivered"
	StatusFailed     Status = "failed"
	StatusSuppressed Status = "suppressed"
)

// ErrorClass distinguishes a permanent failure (retrying is pointless: the
// phone number is invalid, the token is unregistered) from a transient one
// (the provider returned 503; try again shortly).
type ErrorClass string

const (
	ErrorClassTransient ErrorClass = "transient"
	ErrorClassPermanent ErrorClass = "permanent"
)

// TemplateKey identifies a seeded notification template.
type TemplateKey string

const (
	TemplateBookingConfirmed           TemplateKey = "booking_confirmed"
	TemplateReminder1h                 TemplateKey = "reminder_1h"
	TemplateReminder24h                TemplateKey = "reminder_24h"
	TemplateDoctorApproved             TemplateKey = "doctor_approved"
	TemplateDoctorRejected             TemplateKey = "doctor_rejected"
	TemplatePrescriptionReady          TemplateKey = "prescription_ready"
	TemplatePaymentReceipt             TemplateKey = "payment_receipt"
	TemplatePaymentFailed              TemplateKey = "payment_failed"
	TemplatePayoutSent                 TemplateKey = "payout_sent"
	TemplateWaitlistAvailable          TemplateKey = "waitlist_available"
	TemplateConsultationStarting       TemplateKey = "consultation_starting"
	TemplateAppointmentCancelled       TemplateKey = "appointment_cancelled"
	TemplateOTPCode                    TemplateKey = "otp_code"
	TemplateDoctorApplicationSubmitted TemplateKey = "doctor_application_submitted"
	TemplateDoctorApplicationApproved  TemplateKey = "doctor_application_approved"
	TemplateDoctorApplicationRejected  TemplateKey = "doctor_application_rejected"
	TemplateRescheduleRequested        TemplateKey = "reschedule_requested"
	TemplateRescheduleConfirmed        TemplateKey = "reschedule_confirmed"
	TemplateDoctorRunningLate          TemplateKey = "doctor_running_late"
	TemplateEarlyJoinOffered           TemplateKey = "early_join_offered"
)

// Notification is one rendered, addressed message and its delivery state.
type Notification struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	Channel     Channel
	TemplateKey TemplateKey
	Locale      Locale
	Urgency     Urgency
	Subject     string
	Body        string
	// Recipient is the phone number (sms) or email address (email) this
	// notification goes to. Empty for push (resolved from this service's own
	// device_tokens at dispatch time) and in_app (no external recipient).
	Recipient         string
	Status            Status
	Provider          string
	ProviderMessageID string
	Attempts          int
	LastError         string
	ErrorClass        ErrorClass
	ScheduledFor      *time.Time
	NextAttemptAt     *time.Time
	SentAt            *time.Time
	DeliveredAt       *time.Time
	ReadAt            *time.Time
	DedupeKey         string
	SourceEventID     *uuid.UUID
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Version           int
}

// Preferences holds one user's channel opt-in/opt-out, locale, and quiet
// hours. A missing row (see Repository.GetPreferences) means "use these
// zero-value defaults", which is why the DB row's own defaults and this
// struct's DefaultPreferences must agree.
type Preferences struct {
	UserID       uuid.UUID
	SMSEnabled   bool
	PushEnabled  bool
	EmailEnabled bool
	InAppEnabled bool
	Locale       Locale
	// QuietHoursStart/End are wall-clock offsets since local midnight (e.g.
	// 22h means 22:00), evaluated in Timezone. nil means "no quiet hours
	// configured" for that boundary; both must be set for quiet hours to
	// apply at all.
	QuietHoursStart *time.Duration
	QuietHoursEnd   *time.Duration
	Timezone        string
	Version         int
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// DefaultPreferences returns the platform default for a user who has never
// touched their notification settings: every channel on, English, no quiet
// hours, Colombo time.
func DefaultPreferences(userID uuid.UUID) Preferences {
	return Preferences{
		UserID:       userID,
		SMSEnabled:   true,
		PushEnabled:  true,
		EmailEnabled: true,
		InAppEnabled: true,
		Locale:       LocaleEnglish,
		Timezone:     "Asia/Colombo",
	}
}

// Enabled reports whether the user has channel ch turned on.
func (p Preferences) Enabled(ch Channel) bool {
	switch ch {
	case ChannelSMS:
		return p.SMSEnabled
	case ChannelPush:
		return p.PushEnabled
	case ChannelEmail:
		return p.EmailEnabled
	case ChannelInApp:
		return p.InAppEnabled
	default:
		return false
	}
}

// Platform is one of the three device platforms a push token can belong to.
type Platform string

const (
	PlatformIOS     Platform = "ios"
	PlatformAndroid Platform = "android"
	PlatformWeb     Platform = "web"
)

// DeviceToken is one push endpoint registered for a user.
type DeviceToken struct {
	ID            uuid.UUID
	UserID        uuid.UUID
	Token         string
	Platform      Platform
	LastSeenAt    time.Time
	InvalidatedAt *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Template is one versioned, rendered-with-text/template-or-html/template
// message body for one (key, channel, locale).
type Template struct {
	ID              uuid.UUID
	Key             TemplateKey
	Channel         Channel
	Locale          Locale
	Urgency         Urgency
	SubjectTemplate string
	BodyTemplate    string
	Version         int
	IsActive        bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// DeliveryLogEntry is one append-only per-attempt record.
type DeliveryLogEntry struct {
	ID               uuid.UUID
	NotificationID   uuid.UUID
	Attempt          int
	Provider         string
	Status           string
	ErrorClass       ErrorClass
	ProviderResponse []byte // JSON, PHI-free (status/codes/IDs only)
	CreatedAt        time.Time
}

// DeadLetterReason explains why a notification landed in the dead-letter
// table instead of being retried again.
type DeadLetterReason string

const (
	DeadLetterPermanentError   DeadLetterReason = "permanent_error"
	DeadLetterRetriesExhausted DeadLetterReason = "retries_exhausted"
)

// AppointmentReminderState is the local, disposable projection this service
// keeps of scheduling's appointments -- see the migration file for the full
// rationale. It carries only what the reminder cron needs.
type AppointmentReminderState struct {
	AppointmentID uuid.UUID
	PatientID     uuid.UUID
	DoctorID      uuid.UUID
	DoctorName    string
	Specialty     string
	PatientPhone  string
	PatientEmail  string
	JoinLink      string
	StartsAt      time.Time
	Status        string
	// AmountCents is the price QUOTED at booking, taken from
	// events.AppointmentCreated. It is never recomputed from the doctor's
	// current fee: a patient who booked at LKR 2,000 is told LKR 2,000 even
	// if the doctor raised their price a minute later.
	AmountCents       int64
	Currency          string
	Reminder24hSentAt *time.Time
	Reminder1hSentAt  *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}
