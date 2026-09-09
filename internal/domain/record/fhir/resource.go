// Package fhir maps the record service's domain objects onto FHIR R4
// resources (Patient, Practitioner, Encounter, MedicationRequest,
// DocumentReference) and talks to a FHIR server through the FHIRClient
// interface (client.go). The struct shapes below follow the R4 specification
// (https://hl7.org/fhir/R4/) field-for-field rather than using map[string]any,
// so a malformed resource fails at compile time, not at the FHIR server's
// validator.
//
// This service only ever sends resources it owns the lifecycle of
// (MedicationRequest, DocumentReference) and upserts lightweight references
// for Patient/Practitioner/Encounter so those resources resolve -- it is not
// the system of record for demographics or scheduling (ADR-004: no
// cross-service foreign keys, and the same discipline applies to FHIR
// references).
package fhir

import (
	"html"
	"strings"
)

// Identifier is a business identifier such as an NIC, an SLMC registration
// number, or our own UUID, scoped by the system that issued it.
type Identifier struct {
	System string `json:"system,omitempty"`
	Value  string `json:"value"`
}

// HumanName follows FHIR's structured name; Sri Lankan names are recorded as
// a single given name plus family name in our upstream services, so Given is
// always a one-element slice here.
type HumanName struct {
	Use    string   `json:"use,omitempty"` // official | usual | nickname
	Text   string   `json:"text,omitempty"`
	Family string   `json:"family,omitempty"`
	Given  []string `json:"given,omitempty"`
	Prefix []string `json:"prefix,omitempty"` // e.g. "Dr"
}

// ContactPoint is a phone number or email, FHIR's telecom shape.
type ContactPoint struct {
	System string `json:"system"` // phone | email
	Value  string `json:"value"`
	Use    string `json:"use,omitempty"` // mobile | home | work
}

// Address is deliberately minimal: Sri Lankan addresses in the upstream
// services are free-text lines plus a district, not a fully structured
// postal address.
type Address struct {
	Line     []string `json:"line,omitempty"`
	City     string   `json:"city,omitempty"`
	District string   `json:"district,omitempty"`
	Country  string   `json:"country,omitempty"`
}

// CodeableConcept pairs a coded value with human-readable text. Coding is
// optional throughout this service: where we do not yet have a SNOMED/LOINC
// mapping, Text alone is a valid FHIR CodeableConcept.
type CodeableConcept struct {
	Coding []Coding `json:"coding,omitempty"`
	Text   string   `json:"text,omitempty"`
}

// Coding is one code from one system within a CodeableConcept.
type Coding struct {
	System  string `json:"system,omitempty"`
	Code    string `json:"code,omitempty"`
	Display string `json:"display,omitempty"`
}

// Reference points at another resource, either by relative URL
// ("Patient/123") or, when the target has no server-assigned id yet, by
// Identifier.
type Reference struct {
	Reference  string      `json:"reference,omitempty"`
	Identifier *Identifier `json:"identifier,omitempty"`
	Display    string      `json:"display,omitempty"`
}

// Period is a start/end instant pair, both RFC3339.
type Period struct {
	Start string `json:"start,omitempty"`
	End   string `json:"end,omitempty"`
}

// Attachment carries the pointer to binary content -- here, the presigned
// URL to a document stored in MinIO -- rather than the bytes themselves.
type Attachment struct {
	ContentType string `json:"contentType,omitempty"`
	URL         string `json:"url,omitempty"`
	Title       string `json:"title,omitempty"`
	Creation    string `json:"creation,omitempty"`
}

// Meta carries the resource's versioning/profile metadata. We only ever read
// it back from the server; we never set it on the way in.
type Meta struct {
	VersionID   string   `json:"versionId,omitempty"`
	LastUpdated string   `json:"lastUpdated,omitempty"`
	Profile     []string `json:"profile,omitempty"`
}

// Patient maps to a telemed user. This service does not own patient
// demographics (user-service does); it upserts a minimal Patient resource
// only so Encounter/MedicationRequest/DocumentReference have somewhere valid
// to point their `subject` reference.
type Patient struct {
	ResourceType string         `json:"resourceType"`
	ID           string         `json:"id,omitempty"`
	Meta         *Meta          `json:"meta,omitempty"`
	Identifier   []Identifier   `json:"identifier,omitempty"`
	Name         []HumanName    `json:"name,omitempty"`
	Telecom      []ContactPoint `json:"telecom,omitempty"`
	Gender       string         `json:"gender,omitempty"`
	BirthDate    string         `json:"birthDate,omitempty"`
	Address      []Address      `json:"address,omitempty"`
}

// Practitioner maps to a telemed doctor, identified by SLMC registration
// number.
type Practitioner struct {
	ResourceType  string          `json:"resourceType"`
	ID            string          `json:"id,omitempty"`
	Meta          *Meta           `json:"meta,omitempty"`
	Identifier    []Identifier    `json:"identifier,omitempty"`
	Name          []HumanName     `json:"name,omitempty"`
	Telecom       []ContactPoint  `json:"telecom,omitempty"`
	Qualification []Qualification `json:"qualification,omitempty"`
}

// Qualification records a professional qualification, e.g. "MBBS" plus the
// issuing body.
type Qualification struct {
	Code   CodeableConcept `json:"code"`
	Issuer *Reference      `json:"issuer,omitempty"`
}

// EncounterParticipant links a participant (typically the treating
// Practitioner) to the Encounter.
type EncounterParticipant struct {
	Individual *Reference `json:"individual,omitempty"`
}

// Encounter maps to a completed appointment/consultation.
type Encounter struct {
	ResourceType string                 `json:"resourceType"`
	ID           string                 `json:"id,omitempty"`
	Meta         *Meta                  `json:"meta,omitempty"`
	Identifier   []Identifier           `json:"identifier,omitempty"`
	Status       string                 `json:"status"` // planned | in-progress | finished | cancelled
	Class        Coding                 `json:"class"`
	Subject      *Reference             `json:"subject,omitempty"`
	Participant  []EncounterParticipant `json:"participant,omitempty"`
	Period       *Period                `json:"period,omitempty"`
	ReasonCode   []CodeableConcept      `json:"reasonCode,omitempty"`
}

// Timing describes a dosage schedule ("twice daily for 7 days" ->
// frequency=2, period=1, periodUnit=d).
type Timing struct {
	Repeat TimingRepeat `json:"repeat"`
}

// TimingRepeat is FHIR's structured recurrence: Frequency times per Period
// PeriodUnit, for BoundsDuration units total.
type TimingRepeat struct {
	Frequency      int       `json:"frequency,omitempty"`
	Period         float64   `json:"period,omitempty"`
	PeriodUnit     string    `json:"periodUnit,omitempty"` // s | min | h | d | wk | mo | a
	BoundsDuration *Duration `json:"boundsDuration,omitempty"`
}

// Duration is a quantity with a UCUM time unit.
type Duration struct {
	Value  float64 `json:"value"`
	Unit   string  `json:"unit"`
	System string  `json:"system,omitempty"`
	Code   string  `json:"code,omitempty"`
}

// DosageInstruction is one line of a prescription: free text plus its
// structured timing and route.
type DosageInstruction struct {
	Text        string          `json:"text,omitempty"`
	Timing      *Timing         `json:"timing,omitempty"`
	Route       CodeableConcept `json:"route,omitempty"`
	DoseAndRate []DoseAndRate   `json:"doseAndRate,omitempty"`
}

// DoseAndRate carries the quantity dispensed per administration.
type DoseAndRate struct {
	DoseQuantity *Quantity `json:"doseQuantity,omitempty"`
}

// Quantity is FHIR's generic measured amount.
type Quantity struct {
	Value  float64 `json:"value,omitempty"`
	Unit   string  `json:"unit,omitempty"`
	System string  `json:"system,omitempty"`
	Code   string  `json:"code,omitempty"`
}

// MedicationRequest maps one prescribed drug line to a FHIR order.
type MedicationRequest struct {
	ResourceType              string              `json:"resourceType"`
	ID                        string              `json:"id,omitempty"`
	Meta                      *Meta               `json:"meta,omitempty"`
	Identifier                []Identifier        `json:"identifier,omitempty"`
	Status                    string              `json:"status"` // active | completed | cancelled
	Intent                    string              `json:"intent"` // order
	MedicationCodeableConcept CodeableConcept     `json:"medicationCodeableConcept"`
	Subject                   Reference           `json:"subject"`
	Encounter                 *Reference          `json:"encounter,omitempty"`
	Requester                 *Reference          `json:"requester,omitempty"`
	AuthoredOn                string              `json:"authoredOn,omitempty"`
	DosageInstruction         []DosageInstruction `json:"dosageInstruction,omitempty"`
	DispenseRequest           *DispenseRequest    `json:"dispenseRequest,omitempty"`
}

// DispenseRequest carries the quantity a pharmacist should dispense.
type DispenseRequest struct {
	Quantity *Quantity `json:"quantity,omitempty"`
}

// DocumentReferenceContent wraps the Attachment plus an optional format code.
type DocumentReferenceContent struct {
	Attachment Attachment `json:"attachment"`
}

// DocumentReference maps one uploaded medical record or prescription PDF.
type DocumentReference struct {
	ResourceType string                     `json:"resourceType"`
	ID           string                     `json:"id,omitempty"`
	Meta         *Meta                      `json:"meta,omitempty"`
	Identifier   []Identifier               `json:"identifier,omitempty"`
	Status       string                     `json:"status"` // current | superseded | entered-in-error
	Type         CodeableConcept            `json:"type,omitempty"`
	Subject      *Reference                 `json:"subject,omitempty"`
	Date         string                     `json:"date,omitempty"`
	Author       []Reference                `json:"author,omitempty"`
	Content      []DocumentReferenceContent `json:"content"`
	Context      *DocumentReferenceContext  `json:"context,omitempty"`
}

// DocumentReferenceContext links the document back to the Encounter it was
// produced during, when known.
type DocumentReferenceContext struct {
	Encounter []Reference `json:"encounter,omitempty"`
}

// Narrative is FHIR's human-readable rendering of a resource: an XHTML
// fragment plus a status saying where it came from. Composition.section.text
// is where the actual SOAP prose lives on the wire, so this is not
// decoration -- for a document resource the narrative IS the content a
// clinician reads.
type Narrative struct {
	// Status is generated | extensions | additional | empty. Ours is always
	// "generated": the div is produced mechanically from the stored section
	// text and adds nothing the structured data does not already carry.
	Status string `json:"status"`
	// Div must be a valid XHTML fragment carrying the XHTML namespace. Build
	// it with NarrativeDiv, never by concatenation -- unescaped clinical text
	// containing "<" or "&" produces a resource the FHIR server rejects, and
	// a note that silently fails to reach the record is worse than one that
	// never tried.
	Div string `json:"div"`
}

// CompositionSection is one section of a document. For a SOAP note there are
// four, each carrying its LOINC section code and its prose.
type CompositionSection struct {
	Title string          `json:"title,omitempty"`
	Code  CodeableConcept `json:"code,omitempty"`
	Text  *Narrative      `json:"text,omitempty"`
	// Entry references the structured resources this section is about --
	// for the assessment section, the Condition resources carrying the
	// ICD-10 diagnoses.
	Entry []Reference `json:"entry,omitempty"`
	// EmptyReason explains a section with no text. FHIR requires a section to
	// have text, entries, sub-sections or an emptyReason; a SOAP note where
	// the doctor wrote nothing under "Objective" is a real and common case,
	// so we say so explicitly rather than omitting the section and leaving a
	// reader to wonder whether it was empty or lost.
	EmptyReason *CodeableConcept `json:"emptyReason,omitempty"`
}

// CompositionAttester records who attested to the document and when. A
// finalised clinical note is attested "professional" by its author: that is
// the FHIR expression of "the doctor signed this".
type CompositionAttester struct {
	Mode  string     `json:"mode"` // personal | professional | legal | official
	Time  string     `json:"time,omitempty"`
	Party *Reference `json:"party,omitempty"`
}

// CompositionRelatesTo links this document to another. An amendment uses
// code "replaces" pointing at the Composition it supersedes, which is how
// FHIR expresses the same thing clinical_note_revisions expresses in
// Postgres: the previous text is not gone, it is superseded.
type CompositionRelatesTo struct {
	Code             string      `json:"code"` // replaces | transforms | signs | appends
	TargetReference  *Reference  `json:"targetReference,omitempty"`
	TargetIdentifier *Identifier `json:"targetIdentifier,omitempty"`
}

// Composition is FHIR R4's clinical document: a set of section narratives
// assembled under a single author, subject and attestation. It is the
// resource a SOAP note maps to -- see docs/DESIGN.md for why this and not
// DocumentReference.
//
// Note that Composition.identifier is 0..1 in R4 (it only became repeating
// in R5), so this is a pointer to one Identifier rather than a slice, unlike
// every other resource in this file.
type Composition struct {
	ResourceType string      `json:"resourceType"`
	ID           string      `json:"id,omitempty"`
	Meta         *Meta       `json:"meta,omitempty"`
	Identifier   *Identifier `json:"identifier,omitempty"`
	// Status is preliminary | final | amended | entered-in-error. This is
	// the field that makes Composition the right resource for a note with a
	// draft/finalised/amended lifecycle: the states already exist in the
	// specification and mean exactly what we mean by them.
	Status    string                 `json:"status"`
	Type      CodeableConcept        `json:"type"`
	Category  []CodeableConcept      `json:"category,omitempty"`
	Subject   *Reference             `json:"subject,omitempty"`
	Encounter *Reference             `json:"encounter,omitempty"`
	Date      string                 `json:"date"`
	Author    []Reference            `json:"author"`
	Title     string                 `json:"title"`
	Attester  []CompositionAttester  `json:"attester,omitempty"`
	RelatesTo []CompositionRelatesTo `json:"relatesTo,omitempty"`
	Section   []CompositionSection   `json:"section,omitempty"`
}

// Condition is FHIR R4's coded clinical problem. Each ICD-10 diagnosis on a
// clinical note becomes one Condition, referenced from the Composition's
// assessment section. Keeping the codes in Condition rather than burying
// them in the narrative is what makes them queryable -- "every patient coded
// A90 this month" is a Condition search, and is not answerable at all if the
// code only exists as words inside a section's XHTML.
type Condition struct {
	ResourceType       string            `json:"resourceType"`
	ID                 string            `json:"id,omitempty"`
	Meta               *Meta             `json:"meta,omitempty"`
	Identifier         []Identifier      `json:"identifier,omitempty"`
	ClinicalStatus     *CodeableConcept  `json:"clinicalStatus,omitempty"`
	VerificationStatus *CodeableConcept  `json:"verificationStatus,omitempty"`
	Category           []CodeableConcept `json:"category,omitempty"`
	Code               CodeableConcept   `json:"code"`
	Subject            Reference         `json:"subject"`
	Encounter          *Reference        `json:"encounter,omitempty"`
	RecordedDate       string            `json:"recordedDate,omitempty"`
	Recorder           *Reference        `json:"recorder,omitempty"`
}

// FHIR code systems this service references. Spelled out once so a typo in a
// system URI -- which a FHIR server accepts silently and which makes the
// code unresolvable forever after -- cannot happen twice.
const (
	// SystemICD10 is WHO ICD-10, the classification Sri Lanka codes against.
	// It is NOT http://hl7.org/fhir/sid/icd-10-cm, which is the US clinical
	// modification and a different code set.
	SystemICD10 = "http://hl7.org/fhir/sid/icd-10"
	SystemLOINC = "http://loinc.org"
	// SystemConditionClinical and SystemConditionVer are the FHIR-defined
	// terminologies for Condition.clinicalStatus / verificationStatus.
	SystemConditionClinical = "http://terminology.hl7.org/CodeSystem/condition-clinical"
	SystemConditionVer      = "http://terminology.hl7.org/CodeSystem/condition-ver-status"
	// SystemListEmptyReason codes why a document section carries no content.
	SystemListEmptyReason = "http://terminology.hl7.org/CodeSystem/list-empty-reason"
	// SystemTelemedUser and SystemTelemedNote scope our own identifiers.
	SystemTelemedUser        = "https://telemed.lk/user"
	SystemTelemedAppointment = "https://telemed.lk/appointment"
	SystemTelemedNote        = "https://telemed.lk/clinical-note"
	SystemTelemedDoctor      = "https://telemed.lk/doctor"
	SystemConditionCategory  = "http://terminology.hl7.org/CodeSystem/condition-category"
	SystemSLMC               = "https://slmc.lk/registration"
)

// LOINC section codes for a SOAP note. These are the standard codes for the
// four sections; using them means a receiving system can identify "this is
// the assessment" without parsing our section titles.
const (
	LOINCConsultNote = "11488-4" // Consult note -- the document type
	LOINCSubjective  = "61150-9" // Subjective Narrative
	LOINCObjective   = "61149-1" // Objective Narrative
	LOINCAssessment  = "51848-0" // Assessment note
	LOINCPlan        = "18776-5" // Plan of care note
)

// NarrativeDiv wraps plain clinical text as an XHTML narrative div, escaping
// it so that text containing "<", "&" or a quotation mark -- which clinical
// prose does, constantly ("BP <90 systolic", "P&A clear") -- produces a valid
// resource instead of one the server rejects.
func NarrativeDiv(text string) string {
	var b strings.Builder
	b.WriteString(`<div xmlns="http://www.w3.org/1999/xhtml">`)
	for i, line := range strings.Split(text, "\n") {
		if i > 0 {
			b.WriteString("<br/>")
		}
		b.WriteString(html.EscapeString(line))
	}
	b.WriteString("</div>")
	return b.String()
}
