// Package user owns identity, phone-OTP authentication, sessions, and family
// profiles for the telemedicine platform. It is the front door: every other
// service either trusts a JWT this service issued or calls user.v1 over gRPC
// to resolve a user_id it was handed by an event.
package user

import (
	"time"

	"github.com/google/uuid"
)

// Language is a supported UI/SMS language. Sri Lanka's three official
// languages, matching the users.language CHECK constraint.
type Language string

const (
	LanguageEnglish Language = "en"
	LanguageSinhala Language = "si"
	LanguageTamil   Language = "ta"
)

// Role mirrors middleware.Role. It is duplicated here (rather than importing
// the middleware package into the domain layer) because the domain must not
// depend on transport-layer types -- model.go has no framework imports.
type Role string

const (
	RolePatient    Role = "patient"
	RoleDoctor     Role = "doctor"
	RoleAdmin      Role = "admin"
	RoleSuperAdmin Role = "super_admin"
	RoleOps        Role = "ops"
	RoleFinance    Role = "finance"
	RoleSupport    Role = "support"
)

// Status is the account lifecycle state.
type Status string

const (
	StatusActive    Status = "active"
	StatusSuspended Status = "suspended"
	StatusDeleted   Status = "deleted"
)

// User is the platform identity record.
type User struct {
	ID      uuid.UUID
	Phone   string // E.164, e.g. +94771234567
	Email   *string
	Name    string
	NICHash *string // keyed HMAC of the NIC (see nic.go); the NIC itself is never stored
	// NICHashVersion names the NIC_HASH_PEPPER generation that produced
	// NICHash. It is nil exactly when NICHash is nil -- a database CHECK
	// enforces the pair (migration 000004), because a digest whose key
	// generation is unknown can never be verified again.
	NICHashVersion  *int
	Language        Language
	Role            Role
	Status          Status
	NoShowCount     int
	KeycloakID      *string
	GoogleSub       *string
	EmailVerifiedAt *time.Time
	// PasswordHash is bcrypt of the account password. It is never serialised
	// on HTTP responses; login fetches it through a dedicated repository
	// method so a profile read cannot accidentally leak it.
	PasswordHash string
	ErasureDueAt *time.Time
	AnonymizedAt *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeletedAt    *time.Time
	Version      int
}

// FamilyRelation is who the family member is to the account owner.
type FamilyRelation string

const (
	RelationChild   FamilyRelation = "child"
	RelationParent  FamilyRelation = "parent"
	RelationSpouse  FamilyRelation = "spouse"
	RelationSibling FamilyRelation = "sibling"
	RelationOther   FamilyRelation = "other"
)

// FamilyMember is a dependant the account owner books appointments for --
// commonly a child or an elderly parent, which is the normal booking pattern
// in Sri Lanka.
type FamilyMember struct {
	ID          uuid.UUID
	OwnerUserID uuid.UUID
	Name        string
	DOB         time.Time
	Relation    FamilyRelation
	NICHash     *string
	// NICHashVersion names the NIC_HASH_PEPPER generation that produced
	// NICHash. See User.NICHashVersion.
	NICHashVersion *int
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DeletedAt      *time.Time
	Version        int
}

// RefreshToken is a session record. Only TokenHash is ever persisted; the raw
// token exists solely in the response body handed to the client.
type RefreshToken struct {
	ID           uuid.UUID
	UserID       uuid.UUID
	FamilyID     uuid.UUID // groups every token descended from one login
	TokenHash    string    // SHA-256 hex
	DeviceIDHash *string
	IssuedAt     time.Time
	ExpiresAt    time.Time
	RevokedAt    *time.Time
	ReplacedBy   *uuid.UUID
}

// OTPPurpose is why the caller wants a code.
type OTPPurpose string

const (
	PurposeRegister OTPPurpose = "register"
	PurposeLogin    OTPPurpose = "login"
)

// OTPAction distinguishes a send attempt from a verify attempt in the audit
// trail.
type OTPAction string

const (
	ActionSend   OTPAction = "send"
	ActionVerify OTPAction = "verify"
)

// OTPAttempt is one row of the abuse-investigation audit trail. The OTP code
// itself is never recorded here, in a log, or anywhere else in plaintext.
type OTPAttempt struct {
	ID        int64
	Phone     string
	Purpose   OTPPurpose
	Action    OTPAction
	IP        string
	Success   bool
	CreatedAt time.Time
}

// ConsentKind identifies what the user agreed to.
type ConsentKind string

const (
	ConsentPDPA       ConsentKind = "pdpa"
	ConsentTerms      ConsentKind = "terms"
	ConsentMarketing  ConsentKind = "marketing"
	ConsentTelehealth ConsentKind = "telehealth"
)

// Consent is one append-only entry in a user's PDPA/GDPR consent ledger.
type Consent struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Kind      ConsentKind
	Version   string
	Granted   bool
	CreatedAt time.Time
}
