package clinicalnotes

import (
	"context"
	"fmt"
	"time"

	"telemed/internal/domain/record/fhir"
)

// syncFHIR mirrors a finalised or amended note into the FHIR store as a
// Composition plus one Condition per coded diagnosis.
//
// Why Composition and not DocumentReference: see docs/DESIGN.md. The short
// version is Composition.status, whose defined values are
// preliminary | final | amended | entered-in-error -- the exact lifecycle
// this note has -- and Composition.section, which carries the four SOAP
// sections as first-class coded sections rather than as an opaque blob a
// DocumentReference would merely point at.
//
// Best-effort, exactly like prescriptions' FHIR attachment. The note and its
// revision are already durable in Postgres by the time this runs; a Medplum
// outage must not make a doctor unable to sign a note. Failures are logged
// with the note id and NOTHING ELSE -- not the section text, not the ICD-10
// codes, because a code is a diagnosis and a log sink is not the medical
// record.
func (s *Service) syncFHIR(ctx context.Context, n Note, revision int) {
	conditionRefs := make([]fhir.Reference, 0, len(n.Diagnoses))
	for i := range n.Diagnoses {
		d := &n.Diagnoses[i]
		id, err := s.fhirCli.CreateCondition(ctx, s.conditionFor(n, *d))
		if err != nil {
			// No code, no display: the failure is what matters here, and the
			// diagnosis is not going in a log line.
			s.log.Warn().Err(err).Str("note_id", n.ID.String()).Msg("fhir Condition creation failed")
			continue
		}
		conditionRefs = append(conditionRefs, fhir.Reference{Reference: "Condition/" + id})
	}

	compositionID, err := s.fhirCli.CreateComposition(ctx, s.compositionFor(n, revision, conditionRefs))
	if err != nil {
		s.log.Warn().Err(err).Str("note_id", n.ID.String()).Int("revision", revision).
			Msg("fhir Composition creation failed")
		return
	}
	if err := s.repo.SetFHIRComposition(ctx, s.pool, n.ID, compositionID); err != nil {
		s.log.Warn().Err(err).Str("note_id", n.ID.String()).Msg("failed to persist fhir composition id")
	}
}

// compositionIdentifier scopes a Composition to one REVISION of one note.
// Each revision is its own immutable document -- that is what makes
// relatesTo/replaces meaningful -- so the note id alone would not identify
// it.
func compositionIdentifier(noteID fmt.Stringer, revision int) fhir.Identifier {
	return fhir.Identifier{
		System: fhir.SystemTelemedNote,
		Value:  fmt.Sprintf("%s:r%d", noteID, revision),
	}
}

func (s *Service) compositionFor(n Note, revision int, conditionRefs []fhir.Reference) fhir.Composition {
	// preliminary is never emitted: drafts are not sent to FHIR at all. A
	// draft is the doctor's private working text, and pushing it to a shared
	// clinical record on every keystroke would publish exactly the content
	// the draft-visibility rule exists to withhold.
	status := "final"
	if revision > 1 {
		status = "amended"
	}

	signedAt := n.UpdatedAt
	if n.FinalisedAt != nil {
		signedAt = *n.FinalisedAt
	}
	author := fhir.Reference{
		Identifier: &fhir.Identifier{System: fhir.SystemTelemedDoctor, Value: n.DoctorID.String()},
	}
	subject := fhir.Reference{
		Identifier: &fhir.Identifier{System: fhir.SystemTelemedUser, Value: n.PatientID.String()},
	}
	encounter := fhir.Reference{
		Identifier: &fhir.Identifier{System: fhir.SystemTelemedAppointment, Value: n.AppointmentID.String()},
	}
	identifier := compositionIdentifier(n.ID, revision)

	c := fhir.Composition{
		ResourceType: "Composition",
		Identifier:   &identifier,
		Status:       status,
		Type: fhir.CodeableConcept{
			Coding: []fhir.Coding{{System: fhir.SystemLOINC, Code: fhir.LOINCConsultNote, Display: "Consult note"}},
			Text:   "Consultation note",
		},
		Subject:   &subject,
		Encounter: &encounter,
		Date:      signedAt.UTC().Format(time.RFC3339),
		Author:    []fhir.Reference{author},
		Title:     "Teleconsultation clinical note",
		Attester: []fhir.CompositionAttester{{
			// "professional" is FHIR's mode for "the clinician responsible
			// for the content attested to it", which is what finalising a
			// note means here.
			Mode: "professional",
			Time: signedAt.UTC().Format(time.RFC3339),
			Party: &fhir.Reference{
				Identifier: &fhir.Identifier{System: fhir.SystemTelemedDoctor, Value: n.DoctorID.String()},
			},
		}},
		Section: []fhir.CompositionSection{
			soapSection("Subjective", fhir.LOINCSubjective, "Subjective Narrative", n.Subjective, nil),
			soapSection("Objective", fhir.LOINCObjective, "Objective Narrative", n.Objective, nil),
			soapSection("Assessment", fhir.LOINCAssessment, "Assessment note", n.Assessment, conditionRefs),
			soapSection("Plan", fhir.LOINCPlan, "Plan of care note", n.Plan, nil),
		},
	}

	if revision > 1 {
		// targetIdentifier, not targetReference: the identifier is ours and
		// deterministic, so this link is correct even when the previous
		// Composition was written by a different replica, or by a run where
		// the FHIR client was the no-op and no server id ever existed.
		prev := compositionIdentifier(n.ID, revision-1)
		c.RelatesTo = []fhir.CompositionRelatesTo{{Code: "replaces", TargetIdentifier: &prev}}
	}
	return c
}

// soapSection builds one document section. FHIR requires a section to carry
// text, entries, sub-sections or an explicit emptyReason; a doctor who wrote
// nothing under "Objective" is an everyday case, so the section is still
// emitted with emptyReason "nilknown" rather than dropped. A dropped section
// is indistinguishable, to a reader, from one that was lost.
func soapSection(title, loinc, display, text string, entries []fhir.Reference) fhir.CompositionSection {
	section := fhir.CompositionSection{
		Title: title,
		Code: fhir.CodeableConcept{
			Coding: []fhir.Coding{{System: fhir.SystemLOINC, Code: loinc, Display: display}},
			Text:   title,
		},
		Entry: entries,
	}
	if text == "" {
		if len(entries) == 0 {
			section.EmptyReason = &fhir.CodeableConcept{
				Coding: []fhir.Coding{{System: fhir.SystemListEmptyReason, Code: "nilknown", Display: "Nil Known"}},
			}
		}
		return section
	}
	section.Text = &fhir.Narrative{Status: "generated", Div: fhir.NarrativeDiv(text)}
	return section
}

func (s *Service) conditionFor(n Note, d Diagnosis) fhir.Condition {
	recordedAt := n.UpdatedAt
	if n.FinalisedAt != nil {
		recordedAt = *n.FinalisedAt
	}
	return fhir.Condition{
		ResourceType: "Condition",
		Identifier: []fhir.Identifier{{
			System: fhir.SystemTelemedNote,
			Value:  fmt.Sprintf("%s:dx:%s", n.ID, d.Code),
		}},
		ClinicalStatus: &fhir.CodeableConcept{
			Coding: []fhir.Coding{{System: fhir.SystemConditionClinical, Code: "active", Display: "Active"}},
		},
		VerificationStatus: &fhir.CodeableConcept{
			Coding: []fhir.Coding{{System: fhir.SystemConditionVer, Code: "confirmed", Display: "Confirmed"}},
		},
		Category: []fhir.CodeableConcept{{
			Coding: []fhir.Coding{{System: fhir.SystemConditionCategory, Code: "encounter-diagnosis", Display: "Encounter Diagnosis"}},
		}},
		// SystemICD10 is WHO ICD-10, not ICD-10-CM. Getting that URI wrong
		// produces a resource a FHIR server accepts happily and no consumer
		// can ever resolve.
		Code: fhir.CodeableConcept{
			Coding: []fhir.Coding{{System: fhir.SystemICD10, Code: d.Code, Display: d.Display}},
			Text:   d.Display,
		},
		Subject: fhir.Reference{
			Identifier: &fhir.Identifier{System: fhir.SystemTelemedUser, Value: n.PatientID.String()},
		},
		Encounter: &fhir.Reference{
			Identifier: &fhir.Identifier{System: fhir.SystemTelemedAppointment, Value: n.AppointmentID.String()},
		},
		RecordedDate: recordedAt.UTC().Format(time.RFC3339),
		Recorder: &fhir.Reference{
			Identifier: &fhir.Identifier{System: fhir.SystemTelemedDoctor, Value: n.DoctorID.String()},
		},
	}
}
