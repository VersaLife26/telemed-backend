// Package doctor owns doctor profiles, SLMC credentialing state, working
// hours declarations, reviews, and doctor search. It does not own bookable
// slots (telemed_scheduling does) or the video/consultation lifecycle
// (telemed_consultation does).
package doctor

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// VerificationStatus is the SLMC credentialing workflow state. Real SLMC
// registry verification is a manual process performed by an admin
// (telemed-admin-web calls our internal /verify endpoint); there is no
// programmatic SLMC lookup API to call, so this type models a human
// workflow honestly rather than faking an integration.
type VerificationStatus string

const (
	StatusPending     VerificationStatus = "pending"
	StatusUnderReview VerificationStatus = "under_review"
	StatusApproved    VerificationStatus = "approved"
	StatusRejected    VerificationStatus = "rejected"
	StatusSuspended   VerificationStatus = "suspended"
)

// Valid reports whether s is one of the known verification states.
func (s VerificationStatus) Valid() bool {
	switch s {
	case StatusPending, StatusUnderReview, StatusApproved, StatusRejected, StatusSuspended:
		return true
	}
	return false
}

// Language is a platform-supported consultation language.
type Language string

const (
	LanguageEN    Language = "en"
	LanguageSI    Language = "si"
	LanguageTA    Language = "ta"
	LanguageOther Language = "other"
)

// Qualification is one entry in a doctor's qualifications list (degree,
// institution, year). Stored as JSONB; validated at the service layer since
// Postgres JSONB does not enforce shape.
type Qualification struct {
	Degree      string `json:"degree" validate:"required,max=200"`
	Institution string `json:"institution" validate:"required,max=200"`
	Year        int    `json:"year" validate:"required,gte=1950,lte=2100"`
}

// BankDetails is the plaintext shape accepted over the API and encrypted
// before it ever reaches the database. It is never returned by any handler
// and never logged -- see logger.sensitiveKeys and Encryptor.
type BankDetails struct {
	BankName      string `json:"bank_name" validate:"required,max=100"`
	BranchName    string `json:"branch_name" validate:"required,max=100"`
	AccountNumber string `json:"account_number" validate:"required,max=34"`
	AccountName   string `json:"account_name" validate:"required,max=200"`
}

// Doctor is the aggregate root for a credentialed practitioner.
type Doctor struct {
	ID              uuid.UUID
	UserID          uuid.UUID
	SLMCNumber      string
	Specialty       string
	SubSpecialties  []string
	ExperienceYears int
	// FeeCents is the consultation fee in CENTS. The database column is
	// doctors.fee_cents (renamed from fee_lkr in migration 000003, which
	// changed the name only -- the values were always cents).
	FeeCents int64
	// Currency is ISO 4217, "LKR" today. Money never encodes its unit or its
	// currency in a column name.
	Currency       string
	DisplayName    string
	Languages      []Language
	Bio            string
	PhotoURL       string
	Qualifications []Qualification

	VerificationStatus VerificationStatus
	RejectionReason    string
	VerifiedAt         *time.Time
	VerifiedBy         *uuid.UUID

	// BankEncrypted is opaque ciphertext (base64). Never decrypted by this
	// service outside the payout path, which does not exist in this pass --
	// see docs/DESIGN.md "50-Year Maintenance" for the intended consumer.
	BankEncrypted string

	Rating             float64
	ReviewCount        int
	ConsultationCount  int
	NoShowRate         float64
	AcceptsNewPatients bool

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
	Version   int
}

// IsApproved reports whether the doctor is currently bookable.
func (d Doctor) IsApproved() bool { return d.VerificationStatus == StatusApproved }

// WorkingHour is one declared availability window for a day of the week.
// It expresses intent ("I see patients Mondays 09:00-17:00"); it does not
// itself create bookable slots.
type WorkingHour struct {
	ID          uuid.UUID
	DoctorID    uuid.UUID
	DayOfWeek   int    // 0=Sunday .. 6=Saturday, matches Go's time.Weekday
	StartTime   string // "HH:MM:SS", stored as Postgres TIME
	EndTime     string
	IsAvailable bool
}

// ScheduleSettings is a doctor's declaration of how their working hours should
// be sliced into bookable slots. One row per doctor; scheduling-service holds a
// projection of it and materialises the calendar.
//
// BufferMinutes is a pointer, and that is load-bearing rather than stylistic.
// nil means "no preference expressed, use the platform default"; a pointer to 0
// means "back-to-back, no gap", which a busy clinic really does ask for.
// Collapsing them -- treating 0 as unset -- gives that doctor the default gap
// forever with nothing logged to say why. The database column is nullable, the
// event tag is omitempty, and scheduling-service's consumer already reads it as
// a *int. All four layers agree.
type ScheduleSettings struct {
	DoctorID            uuid.UUID
	SlotDurationMinutes int
	BufferMinutes       *int
	MaxPerDay           int // 0 = no cap
	Timezone            string
}

// Schedule setting bounds, enforced in the service layer and mirrored by CHECK
// constraints in migration 000005. A 4-minute consultation or a 6-hour gap is
// a typo, not a preference.
const (
	MinSlotDurationMinutes = 5
	MaxSlotDurationMinutes = 240
	MaxBufferMinutes       = 120
	MaxAppointmentsPerDay  = 100

	// DefaultSlotDurationMinutes matches the doctor app's own default and the
	// column default, so a doctor who never touches the setting gets the same
	// answer from all three.
	DefaultSlotDurationMinutes = 30

	// DefaultScheduleTimezone is the wall-clock zone working hours are
	// expressed in. An IANA name, never a fixed offset.
	DefaultScheduleTimezone = "Asia/Colombo"
)

// DefaultScheduleSettings is what a doctor who has never saved the availability
// editor has. BufferMinutes is deliberately nil rather than zero: they have not
// expressed a preference, and the consumer's own default should apply.
func DefaultScheduleSettings(doctorID uuid.UUID) ScheduleSettings {
	return ScheduleSettings{
		DoctorID:            doctorID,
		SlotDurationMinutes: DefaultSlotDurationMinutes,
		BufferMinutes:       nil,
		MaxPerDay:           0,
		Timezone:            DefaultScheduleTimezone,
	}
}

// Validate rejects a setting that is a typo rather than a preference.
func (s ScheduleSettings) Validate() error {
	if s.SlotDurationMinutes < MinSlotDurationMinutes || s.SlotDurationMinutes > MaxSlotDurationMinutes {
		return fmt.Errorf("%w: slot_duration_minutes must be between %d and %d",
			ErrInvalidSchedule, MinSlotDurationMinutes, MaxSlotDurationMinutes)
	}
	if s.BufferMinutes != nil && (*s.BufferMinutes < 0 || *s.BufferMinutes > MaxBufferMinutes) {
		return fmt.Errorf("%w: buffer_minutes must be between 0 and %d", ErrInvalidSchedule, MaxBufferMinutes)
	}
	if s.MaxPerDay < 0 || s.MaxPerDay > MaxAppointmentsPerDay {
		return fmt.Errorf("%w: max_per_day must be between 0 and %d", ErrInvalidSchedule, MaxAppointmentsPerDay)
	}
	if s.Timezone == "" {
		return fmt.Errorf("%w: timezone must not be empty", ErrInvalidSchedule)
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return fmt.Errorf("%w: unknown timezone %q", ErrInvalidSchedule, s.Timezone)
	}
	return nil
}

// DocumentType enumerates the credential documents SLMC verification checks.
type DocumentType string

const (
	DocumentSLMCCertificate    DocumentType = "slmc_certificate"
	DocumentNIC                DocumentType = "nic"
	DocumentDegreeCertificate  DocumentType = "degree_certificate"
	DocumentSpecialtyBoardCert DocumentType = "specialty_board_certificate"
	// DocumentPhoto is the identifying photograph the admin verification
	// checklist asks a reviewer to confirm is clear. It is a credential in the
	// private doctor-credentials bucket, not doctors.photo_url -- that is the
	// public profile picture patients browse, and the two are not
	// interchangeable.
	DocumentPhoto DocumentType = "photo"
	DocumentOther DocumentType = "other"
	// Apply-time credentials collected before a doctors row exists.
	DocumentSignature DocumentType = "signature"
	DocumentSeal      DocumentType = "seal"
)

func (t DocumentType) Valid() bool {
	switch t {
	case DocumentSLMCCertificate, DocumentNIC, DocumentDegreeCertificate,
		DocumentSpecialtyBoardCert, DocumentPhoto, DocumentOther,
		DocumentSignature, DocumentSeal:
		return true
	}
	return false
}

func (t DocumentType) ValidOnApply() bool {
	switch t {
	case DocumentSignature, DocumentSeal, DocumentSLMCCertificate:
		return true
	}
	return false
}

// Document is a credential upload. object_key points at a MinIO object the
// client uploaded directly (via record-service's presigned URL flow); we
// never see or store the bytes.
type Document struct {
	ID           uuid.UUID
	DoctorID     uuid.UUID
	DocumentType DocumentType
	ObjectKey    string
	UploadedAt   time.Time
	ReviewedAt   *time.Time
	ReviewedBy   *uuid.UUID
	ReviewNotes  string
}

// Review is one patient's rating of a completed appointment. UNIQUE on
// AppointmentID at the database layer enforces "one review per consultation".
type Review struct {
	ID               uuid.UUID
	DoctorID         uuid.UUID
	PatientID        uuid.UUID
	AppointmentID    uuid.UUID
	Rating           int
	Comment          string
	IsPublished      bool
	ModerationReason string
	ModeratedAt      *time.Time
	ModeratedBy      *uuid.UUID
	CreatedAt        time.Time
	UpdatedAt        time.Time
	Version          int
}

// Specialty is a reference-table row driving search filters and the doctor
// registration form.
type Specialty struct {
	Code         string
	NameEN       string
	NameSI       string
	NameTA       string
	DisplayOrder int
	IsActive     bool
}
