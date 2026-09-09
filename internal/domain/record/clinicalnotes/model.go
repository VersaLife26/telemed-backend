// Package clinicalnotes owns the SOAP note a doctor writes during a
// consultation: its autosaved draft, its finalisation into a legal medical
// record, its append-only amendment trail, and the WHO ICD-10 reference
// table the diagnosis picker searches.
//
// It lives in record-service rather than consultation-service because this
// is where the medical record already is. Prescriptions, the
// treating-relationship authorisation model, the append-only
// document_access_log and the FHIR mapping are all here; putting notes
// anywhere else would mean two audit trails and two authorisation
// implementations for one patient's record, and the second implementation is
// always the one with the hole in it.
//
// PHI rule for this package, and it is absolute: the four SOAP fields and
// the ICD-10 codes never appear in a log line, an event payload, or an error
// message. Every error returned from here identifies the note by
// appointment_id or note id and says nothing about its contents.
package clinicalnotes

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// Status is the note's lifecycle. There are exactly two states and the
// transition between them is one-way: a finalised note never becomes a draft
// again. "Unfinalise" is not a missing feature, it is the thing that would
// let a doctor quietly rewrite a signed record with no trail -- an
// amendment, which leaves one, is the supported way to change it.
type Status string

const (
	// StatusDraft is a working note. Freely editable by its author, invisible
	// to everyone else including the patient.
	StatusDraft Status = "draft"
	// StatusFinalised is a signed legal medical record. Immutable except
	// through Amend, which writes a revision.
	StatusFinalised Status = "finalised"
)

// ChangeType records what produced a revision.
type ChangeType string

const (
	// ChangeFinalise is revision 1: the text as it stood when the doctor
	// first signed the note.
	ChangeFinalise ChangeType = "finalise"
	// ChangeAmend is every revision after that.
	ChangeAmend ChangeType = "amend"
)

// MaxSectionRunes bounds one SOAP section. It matches the CHECK constraint in
// migration 000004; the service enforces it too so a doctor gets a clean 422
// naming the section rather than a database error naming a constraint.
const MaxSectionRunes = 20000

// MaxDiagnoses bounds how many ICD-10 codes may be attached to one note. Ten
// is generous for a single teleconsultation and stops an autosave payload
// from growing without limit.
const MaxDiagnoses = 10

// MaxAmendmentReasonRunes bounds the free-text reason on an amendment.
const MaxAmendmentReasonRunes = 1000

// Diagnosis is one coded diagnosis on a note.
//
// Code and Display are both stored, not just Code. The display term is
// captured at the moment the doctor picked it, exactly as prescriptions
// captures the issuing doctor's name and SLMC number: a coded diagnosis on a
// signed record must still read correctly after the reference table has been
// re-seeded from a newer ICD-10 revision.
type Diagnosis struct {
	Code      string
	Display   string
	IsPrimary bool
	SortOrder int
}

// Note is one consultation's SOAP note.
type Note struct {
	ID            uuid.UUID
	AppointmentID uuid.UUID
	DoctorID      uuid.UUID
	PatientID     uuid.UUID

	Subjective string
	Objective  string
	Assessment string
	Plan       string

	Status      Status
	FinalisedAt *time.Time

	FHIRCompositionID string

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
	Version   int

	Diagnoses []Diagnosis
}

// IsFinalised reports whether the note has been signed off.
func (n Note) IsFinalised() bool { return n.Status == StatusFinalised }

// HasContent reports whether any SOAP section carries text. Finalising a
// note with none is refused: an empty signed record is not a record, it is a
// mis-click that will later look like a consultation nobody documented.
func (n Note) HasContent() bool {
	return strings.TrimSpace(n.Subjective) != "" ||
		strings.TrimSpace(n.Objective) != "" ||
		strings.TrimSpace(n.Assessment) != "" ||
		strings.TrimSpace(n.Plan) != ""
}

// SameContentAs reports whether other carries identical SOAP text and an
// identical diagnosis set.
//
// This is what makes autosave genuinely idempotent rather than merely
// repeatable. The doctor app saves on a debounce after every keystroke, and
// a keystroke that gets undone -- or a retry of a request that already
// landed -- must not advance the version, because advancing it would make
// the OTHER device's in-flight save fail with a 409 for no reason at all.
// Comparing content and short-circuiting is cheaper than the write it
// avoids, and it is the difference between "two devices open" being fine and
// being a stream of spurious conflicts.
func (n Note) SameContentAs(other Note) bool {
	if n.Subjective != other.Subjective || n.Objective != other.Objective ||
		n.Assessment != other.Assessment || n.Plan != other.Plan {
		return false
	}
	if len(n.Diagnoses) != len(other.Diagnoses) {
		return false
	}
	// Order is part of the content: the doctor chose it, and a reordered
	// differential reads differently.
	for i := range n.Diagnoses {
		a, b := n.Diagnoses[i], other.Diagnoses[i]
		if a.Code != b.Code || a.Display != b.Display || a.IsPrimary != b.IsPrimary {
			return false
		}
	}
	return true
}

// Revision is one immutable row of clinical_note_revisions: the note's
// content as it stood at the end of one change, plus who made it and why.
type Revision struct {
	ID              int64
	NoteID          uuid.UUID
	Revision        int
	Subjective      string
	Objective       string
	Assessment      string
	Plan            string
	Diagnoses       []Diagnosis
	ChangeType      ChangeType
	AmendmentReason string
	ChangedBy       uuid.UUID
	ChangedByRole   string
	CreatedAt       time.Time
}

// ICD10Code is one row of the WHO ICD-10 reference table.
type ICD10Code struct {
	Code     string
	Display  string
	Category string
}
