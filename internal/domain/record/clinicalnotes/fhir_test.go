package clinicalnotes

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/domain/record/fhir"
)

func sampleNote() Note {
	finalisedAt := time.Date(2026, 8, 20, 4, 30, 0, 0, time.UTC)
	return Note{
		ID:            uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		AppointmentID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		DoctorID:      uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		PatientID:     uuid.MustParse("44444444-4444-4444-4444-444444444444"),
		Subjective:    soapSubjective,
		Objective:     soapObjective,
		Assessment:    soapAssessment,
		Plan:          soapPlan,
		Status:        StatusFinalised,
		FinalisedAt:   &finalisedAt,
		Diagnoses: []Diagnosis{
			{Code: "A90", Display: "Dengue fever [classical dengue]", IsPrimary: true},
		},
	}
}

// TestCompositionForFinalisedNote pins the mapping choices docs/DESIGN.md
// argues for. Composition.status is the field that made Composition the
// right resource, so it is the first thing asserted.
func TestCompositionForFinalisedNote(t *testing.T) {
	svc := &Service{log: zerolog.Nop()}
	n := sampleNote()

	c := svc.compositionFor(n, 1, []fhir.Reference{{Reference: "Condition/abc"}})

	if c.Status != "final" {
		t.Errorf("revision 1 status = %q, want \"final\"", c.Status)
	}
	if c.Identifier == nil || c.Identifier.Value != n.ID.String()+":r1" {
		t.Errorf("identifier = %+v, want <note-id>:r1", c.Identifier)
	}
	if len(c.RelatesTo) != 0 {
		t.Errorf("the first revision replaces nothing, got %+v", c.RelatesTo)
	}
	if c.Type.Coding[0].Code != fhir.LOINCConsultNote || c.Type.Coding[0].System != fhir.SystemLOINC {
		t.Errorf("type = %+v, want LOINC %s", c.Type.Coding, fhir.LOINCConsultNote)
	}
	if len(c.Author) != 1 || c.Author[0].Identifier.Value != n.DoctorID.String() {
		t.Errorf("author = %+v, want the treating doctor", c.Author)
	}
	if c.Subject == nil || c.Subject.Identifier.Value != n.PatientID.String() {
		t.Errorf("subject = %+v, want the patient", c.Subject)
	}
	if c.Encounter == nil || c.Encounter.Identifier.Value != n.AppointmentID.String() {
		t.Errorf("encounter = %+v, want the appointment", c.Encounter)
	}
	if c.Date != "2026-08-20T04:30:00Z" {
		t.Errorf("date = %q, want the moment it was signed", c.Date)
	}
	if len(c.Attester) != 1 || c.Attester[0].Mode != "professional" {
		t.Errorf("attester = %+v, want one professional attestation", c.Attester)
	}

	// Four sections, in SOAP order, each with its LOINC code.
	wantSections := []struct{ title, code, text string }{
		{"Subjective", fhir.LOINCSubjective, soapSubjective},
		{"Objective", fhir.LOINCObjective, soapObjective},
		{"Assessment", fhir.LOINCAssessment, soapAssessment},
		{"Plan", fhir.LOINCPlan, soapPlan},
	}
	if len(c.Section) != len(wantSections) {
		t.Fatalf("got %d sections, want %d", len(c.Section), len(wantSections))
	}
	for i, want := range wantSections {
		got := c.Section[i]
		if got.Title != want.title {
			t.Errorf("section %d title = %q, want %q", i, got.Title, want.title)
		}
		if len(got.Code.Coding) != 1 || got.Code.Coding[0].Code != want.code {
			t.Errorf("section %d code = %+v, want LOINC %s", i, got.Code.Coding, want.code)
		}
		if got.Text == nil || !strings.Contains(got.Text.Div, want.text) {
			t.Errorf("section %d does not carry its narrative", i)
		}
	}
	// The assessment section is the one that links the coded diagnoses.
	if len(c.Section[2].Entry) != 1 {
		t.Errorf("the assessment section should reference the Condition resources, got %+v", c.Section[2].Entry)
	}
}

func TestCompositionForAmendedNote(t *testing.T) {
	svc := &Service{log: zerolog.Nop()}
	n := sampleNote()

	c := svc.compositionFor(n, 3, nil)

	if c.Status != "amended" {
		t.Errorf("revision 3 status = %q, want \"amended\"", c.Status)
	}
	if c.Identifier == nil || c.Identifier.Value != n.ID.String()+":r3" {
		t.Errorf("identifier = %+v, want <note-id>:r3", c.Identifier)
	}
	if len(c.RelatesTo) != 1 || c.RelatesTo[0].Code != "replaces" {
		t.Fatalf("an amendment must replace its predecessor, got %+v", c.RelatesTo)
	}
	// targetIdentifier, not targetReference: the link has to hold even when
	// the previous Composition was written by another replica, or by a run
	// where the FHIR client was the no-op and no server id ever existed.
	target := c.RelatesTo[0].TargetIdentifier
	if target == nil || target.Value != n.ID.String()+":r2" {
		t.Errorf("relatesTo target = %+v, want <note-id>:r2", target)
	}
}

// TestSectionWithNoTextCarriesAnEmptyReason: FHIR requires a section to have
// text, entries, sub-sections or an explicit emptyReason. A doctor who wrote
// nothing under "Objective" is an everyday case, and a dropped section is
// indistinguishable to a reader from one that was lost.
func TestSectionWithNoTextCarriesAnEmptyReason(t *testing.T) {
	svc := &Service{log: zerolog.Nop()}
	n := sampleNote()
	n.Objective = ""

	c := svc.compositionFor(n, 1, nil)
	objective := c.Section[1]
	if objective.Text != nil {
		t.Errorf("an empty section should carry no narrative, got %+v", objective.Text)
	}
	if objective.EmptyReason == nil || objective.EmptyReason.Coding[0].Code != "nilknown" {
		t.Errorf("an empty section must say why it is empty, got %+v", objective.EmptyReason)
	}
}

func TestConditionForDiagnosis(t *testing.T) {
	svc := &Service{log: zerolog.Nop()}
	n := sampleNote()

	c := svc.conditionFor(n, n.Diagnoses[0])

	if len(c.Code.Coding) != 1 {
		t.Fatalf("expected one coding, got %+v", c.Code.Coding)
	}
	coding := c.Code.Coding[0]
	// WHO ICD-10, not ICD-10-CM. Getting this URI wrong produces a resource
	// a FHIR server accepts happily and no consumer can ever resolve.
	if coding.System != "http://hl7.org/fhir/sid/icd-10" {
		t.Errorf("system = %q, want the WHO ICD-10 URI", coding.System)
	}
	if coding.Code != "A90" || coding.Display == "" {
		t.Errorf("coding = %+v, want A90 with its rubric", coding)
	}
	if c.Subject.Identifier.Value != n.PatientID.String() {
		t.Errorf("subject = %+v, want the patient", c.Subject)
	}
	if c.ClinicalStatus == nil || c.ClinicalStatus.Coding[0].Code != "active" {
		t.Errorf("clinicalStatus = %+v", c.ClinicalStatus)
	}
	if c.VerificationStatus == nil || c.VerificationStatus.Coding[0].Code != "confirmed" {
		t.Errorf("verificationStatus = %+v", c.VerificationStatus)
	}
	if len(c.Category) != 1 || c.Category[0].Coding[0].Code != "encounter-diagnosis" {
		t.Errorf("category = %+v, want encounter-diagnosis", c.Category)
	}
}

// TestNarrativeEscapesClinicalProse is not cosmetic. Clinical text contains
// "<" and "&" constantly -- "BP <90 systolic", "P&A clear" -- and an
// unescaped div is a resource the FHIR server rejects, i.e. a note that
// silently never reaches the record.
func TestNarrativeEscapesClinicalProse(t *testing.T) {
	svc := &Service{log: zerolog.Nop()}
	n := sampleNote()
	n.Objective = "BP <90 systolic, P&A clear\nSpO2 94% on air"

	c := svc.compositionFor(n, 1, nil)
	div := c.Section[1].Text.Div

	if strings.Contains(div, "<90") {
		t.Errorf("unescaped '<' survived into the narrative: %q", div)
	}
	if !strings.Contains(div, "&lt;90") || !strings.Contains(div, "P&amp;A") {
		t.Errorf("narrative was not escaped: %q", div)
	}
	if !strings.Contains(div, "<br/>") {
		t.Errorf("line breaks should be rendered, got %q", div)
	}
	if !strings.HasPrefix(div, `<div xmlns="http://www.w3.org/1999/xhtml">`) {
		t.Errorf("the div must carry the XHTML namespace, got %q", div)
	}

	// The whole resource must still marshal to valid JSON.
	if _, err := json.Marshal(c); err != nil {
		t.Fatalf("composition does not marshal: %v", err)
	}
}

// TestNoOpFHIRClientSatisfiesTheNewMethods keeps the default binding honest:
// with no FHIR server configured, finalising a note must still work end to
// end and return distinguishable "noop:" ids rather than empty strings.
func TestNoOpFHIRClientSatisfiesTheNewMethods(t *testing.T) {
	client := fhir.NewNoOp(zerolog.Nop())

	compID, err := client.CreateComposition(t.Context(), fhir.Composition{})
	if err != nil || !strings.HasPrefix(compID, "noop:Composition:") {
		t.Errorf("CreateComposition = %q, %v", compID, err)
	}
	condID, err := client.CreateCondition(t.Context(), fhir.Condition{})
	if err != nil || !strings.HasPrefix(condID, "noop:Condition:") {
		t.Errorf("CreateCondition = %q, %v", condID, err)
	}
}
