// Package credentialing implements the doctor verification queue (SDD
// section 20 / V2 docs section 8.8): a projection of doctor.registered
// events, a per-doctor checklist this service owns outright, and the
// approve/reject decision that publishes doctor.approved/doctor.rejected.
//
// This service never queries telemed_doctor directly -- ADR-004 -- so
// DoctorSummary is a read-optimised local copy fed entirely by events.
// doctor-service remains the system of record for verification_status.
package credentialing

import (
	"time"

	"github.com/google/uuid"
)

// DoctorSummary is one doctor_projection row: enough to render the
// verification queue and the document viewer without a synchronous call.
type DoctorSummary struct {
	DoctorID             uuid.UUID
	FullName             string
	Email                string
	Phone                string
	SLMCNumber           string
	YearsExperience      int
	SpecialtyCode        string
	FeeCents             int64
	VerificationStatus   string
	SLMCCertificateKey   string
	NICDocumentKey       string
	DegreeCertificateKey string
	PhotoKey             string
	RegisteredAt         time.Time
}

// Empty reports whether this doctor has submitted no credential document at
// all -- which is the state a reviewer sees as an application with nothing to
// open, and is normal only in the window between registration and the first
// upload.
func (d DoctorSummary) Empty() bool {
	return d.SLMCCertificateKey == "" && d.NICDocumentKey == "" &&
		d.DegreeCertificateKey == "" && d.PhotoKey == ""
}

// PresignedDoctorSummary adds ready-to-use document URLs for the console.
//
// DocumentsExpireAt is when every URL on this response stops working. The
// console needs it to decide whether to re-fetch the row before opening a
// document rather than showing the reviewer a 403 from MinIO, and it makes the
// TTL visible instead of implicit.
type PresignedDoctorSummary struct {
	DoctorSummary
	SLMCCertificateURL   string    `json:"slmc_certificate_url,omitempty"`
	NICDocumentURL       string    `json:"nic_document_url,omitempty"`
	DegreeCertificateURL string    `json:"degree_certificate_url,omitempty"`
	PhotoURL             string    `json:"photo_url,omitempty"`
	DocumentsExpireAt    time.Time `json:"documents_expire_at,omitempty"`
}

// Checklist is one verification_checklists row.
type Checklist struct {
	ID                    uuid.UUID
	DoctorID              uuid.UUID
	SLMCFormatValid       *bool
	SLMCFormatCheckedBy   *uuid.UUID
	SLMCFormatCheckedAt   *time.Time
	SLMCRegistryChecked   *bool
	SLMCRegistryCheckedBy *uuid.UUID
	SLMCRegistryCheckedAt *time.Time
	ExperienceVerified    *bool
	ExperienceCheckedBy   *uuid.UUID
	ExperienceCheckedAt   *time.Time
	NICMatches            *bool
	NICCheckedBy          *uuid.UUID
	NICCheckedAt          *time.Time
	PhotoClear            *bool
	PhotoCheckedBy        *uuid.UUID
	PhotoCheckedAt        *time.Time
	OverallStatus         string
	DecisionReason        string
	DecidedBy             *uuid.UUID
	DecidedAt             *time.Time
	CreatedAt             time.Time
	UpdatedAt             time.Time
	Version               int
}

// ChecklistUpdate is a partial update to the checklist items, applied by
// PUT /api/v1/admin/doctors/{id}/checklist. Each item is independently
// optional so an admin can tick items off one at a time.
type ChecklistUpdate struct {
	SLMCFormatValid     *bool
	SLMCRegistryChecked *bool
	ExperienceVerified  *bool
	NICMatches          *bool
	PhotoClear          *bool
	CheckerID           uuid.UUID
}

// VerifyDecision is the outcome of POST /api/v1/admin/doctors/{id}/verify.
type VerifyDecision struct {
	Approve   bool
	Reason    string
	DeciderID uuid.UUID
}
