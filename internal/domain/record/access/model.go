// Package access is the authorization crux of the record service: it is the
// single place that decides whether a caller may read a patient's vault, and
// the single place that writes the append-only trail proving that decision
// was made. Every other domain package (records, prescriptions) calls into
// Service.Check rather than implementing its own ownership logic -- that is
// what "enforced centrally" means in practice.
package access

import (
	"time"

	"github.com/google/uuid"
)

// ResourceType distinguishes what document_access_log describes.
type ResourceType string

const (
	ResourceDocument     ResourceType = "document"
	ResourcePrescription ResourceType = "prescription"
	// ResourceClinicalNote is the SOAP note written during a consultation.
	ResourceClinicalNote ResourceType = "clinical_note"
	// ResourceCredentialDocument is a doctor's own registration paperwork --
	// SLMC certificate, degree certificate, NIC -- uploaded as
	// records.DocumentTypeCredential and stored in the separate
	// "doctor-credentials" bucket, never in "medical-reports".
	//
	// It is split out from ResourceDocument because it is the ONE thing on
	// this service an administrator legitimately reads: the credentialing
	// queue cannot verify a doctor without opening the certificate they
	// uploaded. It is not patient data, it is not clinical, and it belongs
	// to the doctor being reviewed rather than to a patient's vault.
	//
	// Splitting it means "an admin may read credentials" and "an admin may
	// not read a patient's vault" are two separate rows in decideAccess
	// instead of one rule doing double duty, which is how the second half
	// got lost the first time (SECURITY-REVIEW F4).
	//nolint:gosec // G101 false positive: this is a resource-type name written into an audit column, not a credential.
	ResourceCredentialDocument ResourceType = "credential_document"
)

// Action is what the caller did with the resource.
type Action string

const (
	ActionView     Action = "view"
	ActionDownload Action = "download"
	ActionVerify   Action = "verify"
	ActionList     Action = "list"
	ActionUpload   Action = "upload"
	// ActionFinalise and ActionAmend are writes, not reads, and are logged
	// because each one changes the content of a legal medical record. Draft
	// autosave is deliberately NOT logged -- see migration 000004.
	ActionFinalise Action = "finalise"
	ActionAmend    Action = "amend"
	// ActionDelete is a patient (or, deliberately, nobody else) removing a
	// document from their own vault. It needed migration 000008 to exist at
	// all: the action CHECK constraint did not permit it, so the one PHI
	// operation that destroys data could not be recorded even in principle.
	ActionDelete Action = "delete"
)

// IsWrite reports whether an action changes what is in a patient's vault, as
// opposed to reading what is already there. A grant that permits a read does
// not automatically permit a write; see ReasonDeniedWrite.
func (a Action) IsWrite() bool {
	switch a {
	case ActionUpload, ActionFinalise, ActionAmend, ActionDelete:
		return true
	default:
		return false
	}
}

// Reason is a stable, greppable explanation for an authorization decision.
// These strings are written verbatim into document_access_log.reason and are
// what an incident responder greps for.
type Reason string

const (
	ReasonOwner          Reason = "owner"
	ReasonTreatingDoctor Reason = "treating_doctor"
	ReasonShare          Reason = "share"
	ReasonAdmin          Reason = "admin"
	ReasonPublicVerify   Reason = "public_verify"
	ReasonDeniedNoLink   Reason = "denied:no_relationship"
	ReasonDeniedRole     Reason = "denied:unsupported_role"
	ReasonDeniedAnon     Reason = "denied:anonymous"
	// ReasonDeniedAdminClinical records an administrator being refused
	// patient clinical content -- a clinical note, a medical document, or a
	// prescription. It is a distinct reason rather than a bare
	// "denied:unsupported_role" precisely so a compliance officer can grep
	// for it and see the control working, rather than having to infer it
	// from an absence of rows.
	ReasonDeniedAdminClinical Reason = "denied:admin_no_clinical_access"
	// ReasonDeniedWrite records a caller who may READ a vault being refused
	// permission to WRITE into it.
	//
	// decideAccess used to answer one question -- "may this principal touch
	// this resource" -- and the answer was reused for both. Two of the grants
	// are read-shaped and became write permissions for free:
	//
	//   - An administrator is granted ResourceCredentialDocument so the
	//     credentialing queue can OPEN a doctor's SLMC certificate. The same
	//     grant let any of the five admin roles POST /records/upload with
	//     owner_user_id=<any victim> and document_type=credential, planting a
	//     file in that person's medical-record index -- or a forged
	//     certificate under a real doctor's identity, which is the credential
	//     substitution F8 describes on the other side of the platform.
	//   - The treating-doctor branch ignores the resource entirely, so a
	//     doctor mid-consultation could file a "credential" into their
	//     patient's vault. A patient has no credentials.
	//
	// Writing into someone else's chart is a narrower right than reading it,
	// and it is now decided separately.
	ReasonDeniedWrite Reason = "denied:read_only_grant"
	// ReasonDeniedDraft records a caller who would otherwise be entitled to
	// the note being refused because it is still an unfinished draft.
	ReasonDeniedDraft Reason = "denied:draft_not_author"
)

// Decision is the outcome of a Check call.
type Decision struct {
	Granted bool
	Reason  Reason
	// ShareID is set when Granted came from a record_shares row, so callers
	// that want to display "shared until <date>" do not need a second query.
	ShareID uuid.UUID
}

// TreatingRelationship is the local read-model of "this doctor treated this
// patient during this appointment", built from consumed consultation.*
// events (see consumer.go). It is what lets Check answer synchronously with
// no call to another service's database.
type TreatingRelationship struct {
	ID            uuid.UUID
	AppointmentID uuid.UUID
	DoctorID      uuid.UUID
	PatientID     uuid.UUID
	StartedAt     *time.Time
	EndedAt       *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// RecordShare is a patient-granted, time-boxed exception: "let Dr X read my
// vault until <expires_at>, regardless of whether we have a treating
// relationship".
type RecordShare struct {
	ID        uuid.UUID
	PatientID uuid.UUID
	DoctorID  uuid.UUID
	GrantedBy uuid.UUID
	ExpiresAt time.Time
	RevokedAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
	Version   int
}

// Active reports whether the share currently grants access.
func (s RecordShare) Active(now time.Time) bool {
	return s.RevokedAt == nil && now.Before(s.ExpiresAt)
}

// LogEntry is one row of the append-only document_access_log.
type LogEntry struct {
	ResourceType   ResourceType
	ResourceID     uuid.UUID
	OwnerUserID    uuid.UUID
	AccessedBy     *uuid.UUID // nil for the anonymous prescription-verification endpoint
	AccessedByRole string
	Action         Action
	Granted        bool
	Reason         Reason
	IPAddress      string
	UserAgent      string
}

// MaxShareTTL bounds how far in the future a patient may set expires_at, so a
// share cannot be granted "forever" by mistake or by a compromised account.
const MaxShareTTL = 90 * 24 * time.Hour

// TreatingAccessWindow bounds the IMPLICIT grant a consultation creates.
//
// The explicit grant -- a record_shares row the patient consciously created
// -- is capped at MaxShareTTL. The implicit one, which the patient never
// consciously grants at all, was unbounded: one consultation gave a doctor
// permanent read+download on that patient's whole vault, including documents
// uploaded years afterwards, with no revocation path (SECURITY-REVIEW F3).
//
// 30 days is the number, and it is chosen rather than inherited:
//
//   - The doctor's legitimate after-the-fact need is real but short. Notes
//     are written and finalised within hours; a prescription is issued the
//     same day; the longest genuinely clinical tail is a lab or imaging
//     result ordered during the consultation and reported days later. Sri
//     Lankan private-lab turnaround for routine chemistry and histopathology
//     tops out around three weeks. 30 days covers all of it with room.
//   - A follow-up does not need this window. A follow-up is a new
//     appointment, which produces a new treating_relationships row and its
//     own fresh window, so the bound never interrupts continuing care.
//   - An implicit grant must never outlive an explicit one. A patient who
//     deliberately shares their vault gets at most 90 days and can revoke it
//     at any time; a grant they never made must be strictly weaker, and
//     strictly weaker means shorter -- not equal.
//   - It must be long enough that no clinician is tempted to ask for a
//     standing exception, and short enough that "a doctor I saw once still
//     reads my file" is measured in weeks rather than in never.
//
// After it lapses the doctor is not locked out of care -- they are simply
// back to needing what everyone else needs: a share the patient granted, or
// a new consultation.
const TreatingAccessWindow = 30 * 24 * time.Hour

// treatingFutureGrace tolerates clock skew between this service and whoever
// published consultation.started. The timestamps in treating_relationships
// come from another service's clock via an event payload, so a consultation
// that started two seconds ago can legitimately arrive stamped slightly in
// the future. Without a grace, that would deny a doctor access mid-call.
//
// It is bounded rather than unbounded because "started_at is in the future"
// is otherwise a way to make a window that never closes: a corrupt or
// hostile producer that stamps started_at in 2099 would restore exactly the
// permanent grant this window exists to remove.
const treatingFutureGrace = 5 * time.Minute

// GrantsAccessAt reports whether this relationship still confers implicit
// access at now.
//
// The window is anchored on the timestamps the relationship actually has.
// firstSeen is when the consultation began as best this service knows;
// lastSeen is when it concluded as best this service knows. A row with
// neither timestamp -- which nothing produces today, but which a future
// event could -- grants nothing, because "a row exists" was never supposed
// to be the authorisation fact. A consultation whose end event never
// arrived expires TreatingAccessWindow after it STARTED rather than never,
// so "admit and never end" is not a way back to a permanent grant.
//
// This is the Go statement of the same rule Repository's SQL applies, kept
// beside it deliberately: the SQL is what authorises a read, this is what
// authorises a write (prescriptions.Issue), and they must not drift.
func (t TreatingRelationship) GrantsAccessAt(now time.Time) bool {
	firstSeen := t.StartedAt
	if firstSeen == nil {
		firstSeen = t.EndedAt
	}
	lastSeen := t.EndedAt
	if lastSeen == nil {
		lastSeen = t.StartedAt
	}
	if firstSeen == nil || lastSeen == nil {
		return false
	}
	if firstSeen.After(now.Add(treatingFutureGrace)) {
		return false
	}
	return lastSeen.After(now.Add(-TreatingAccessWindow))
}
