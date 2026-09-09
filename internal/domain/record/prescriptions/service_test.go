package prescriptions

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/domain/record/access"
	"telemed/internal/domain/record/fhir"
	"telemed/internal/platform/events"
	"telemed/internal/platform/middleware"
)

// newTestService builds a Service with a nil database pool. This is safe
// only for the validation guard clauses that return before Issue reaches
// the treating-relationship lookup -- role check, empty items, and per-item
// field validation. The full issuance flow (which needs a real appointment
// -> treating_relationship row) is covered by the //go:build integration
// suite against a real Postgres.
func newTestService(t *testing.T) *Service {
	t.Helper()
	log := zerolog.Nop()
	accessSvc := access.NewService(access.NewRepository(), nil, log)
	return NewService(NewRepository(), nil, nil, fhir.NewNoOp(log), events.NewOutbox("test"), accessSvc,
		Config{HMACSecret: []byte("test-secret-at-least-32-bytes-long!"), VerifyBaseURL: "https://verify.yourapp.lk"}, log)
}

func TestIssue_RejectsNonDoctorCaller(t *testing.T) {
	svc := newTestService(t)
	patient := middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{middleware.RolePatient}}

	_, err := svc.Issue(context.Background(), IssueInput{
		Principal: patient, AppointmentID: uuid.New(),
		DoctorName: "Dr X", DoctorSLMC: "SLMC1", Patient: PatientDisplay{Name: "P"},
		Items: []ItemInput{{DrugName: "Paracetamol", Dosage: "1 tab", Frequency: "2x", DurationDays: 3, Quantity: 6}},
	})
	if err == nil {
		t.Fatal("expected an error when a patient tries to issue a prescription")
	}
}

func TestIssue_RejectsDoctorWithNoDoctorIDClaim(t *testing.T) {
	svc := newTestService(t)
	// Role says doctor but the token carries no telemed_doctor_id claim --
	// must not be trusted as "some doctor, any doctor".
	broken := middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{middleware.RoleDoctor}}

	_, err := svc.Issue(context.Background(), IssueInput{
		Principal: broken, AppointmentID: uuid.New(),
		DoctorName: "Dr X", DoctorSLMC: "SLMC1", Patient: PatientDisplay{Name: "P"},
		Items: []ItemInput{{DrugName: "Paracetamol", Dosage: "1 tab", Frequency: "2x", DurationDays: 3, Quantity: 6}},
	})
	if err == nil {
		t.Fatal("expected an error when the caller has no doctor id claim")
	}
}

func TestIssue_RejectsEmptyItems(t *testing.T) {
	svc := newTestService(t)
	doctor := middleware.Principal{UserID: uuid.New(), DoctorID: uuid.New(), Roles: []middleware.Role{middleware.RoleDoctor}}

	_, err := svc.Issue(context.Background(), IssueInput{
		Principal: doctor, AppointmentID: uuid.New(),
		DoctorName: "Dr X", DoctorSLMC: "SLMC1", Patient: PatientDisplay{Name: "P"},
		Items: nil,
	})
	if err == nil {
		t.Fatal("expected an error for zero items")
	}
}

func TestIssue_RejectsInvalidItemFields(t *testing.T) {
	svc := newTestService(t)
	doctor := middleware.Principal{UserID: uuid.New(), DoctorID: uuid.New(), Roles: []middleware.Role{middleware.RoleDoctor}}
	base := IssueInput{
		Principal: doctor, AppointmentID: uuid.New(),
		DoctorName: "Dr X", DoctorSLMC: "SLMC1", Patient: PatientDisplay{Name: "P"},
	}

	cases := []struct {
		name string
		item ItemInput
	}{
		{"missing drug name", ItemInput{Dosage: "1 tab", Frequency: "2x", DurationDays: 3, Quantity: 6}},
		{"missing dosage", ItemInput{DrugName: "Paracetamol", Frequency: "2x", DurationDays: 3, Quantity: 6}},
		{"missing frequency", ItemInput{DrugName: "Paracetamol", Dosage: "1 tab", DurationDays: 3, Quantity: 6}},
		{"zero duration", ItemInput{DrugName: "Paracetamol", Dosage: "1 tab", Frequency: "2x", DurationDays: 0, Quantity: 6}},
		{"negative duration", ItemInput{DrugName: "Paracetamol", Dosage: "1 tab", Frequency: "2x", DurationDays: -1, Quantity: 6}},
		{"zero quantity", ItemInput{DrugName: "Paracetamol", Dosage: "1 tab", Frequency: "2x", DurationDays: 3, Quantity: 0}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			in.Items = []ItemInput{tc.item}
			if _, err := svc.Issue(context.Background(), in); err == nil {
				t.Fatalf("expected an error for item case %q", tc.name)
			}
		})
	}
}

func TestSearchDrugs_RejectsEmptyQuery(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.SearchDrugs(context.Background(), "", 10); err == nil {
		t.Fatal("expected an error for an empty query")
	}
	if _, err := svc.SearchDrugs(context.Background(), "   ", 10); err == nil {
		t.Fatal("expected an error for a whitespace-only query")
	}
}

// --- F20b: a drug name never reaches the log sink ----------------------

// alwaysFailingFHIR is a fhir.Client whose every call fails, so attachFHIR
// takes its error branch on every item.
type alwaysFailingFHIR struct{ err error }

func (c alwaysFailingFHIR) UpsertPatient(context.Context, fhir.Patient) (string, error) {
	return "", c.err
}
func (c alwaysFailingFHIR) UpsertPractitioner(context.Context, fhir.Practitioner) (string, error) {
	return "", c.err
}
func (c alwaysFailingFHIR) UpsertEncounter(context.Context, fhir.Encounter) (string, error) {
	return "", c.err
}
func (c alwaysFailingFHIR) CreateMedicationRequest(context.Context, fhir.MedicationRequest) (string, error) {
	return "", c.err
}
func (c alwaysFailingFHIR) CreateDocumentReference(context.Context, fhir.DocumentReference) (string, error) {
	return "", c.err
}
func (c alwaysFailingFHIR) CreateComposition(context.Context, fhir.Composition) (string, error) {
	return "", c.err
}
func (c alwaysFailingFHIR) CreateCondition(context.Context, fhir.Condition) (string, error) {
	return "", c.err
}

// TestAttachFHIR_DoesNotWriteDrugNamesToTheLog is SECURITY-REVIEW F20b,
// pinned at the sink rather than at the call site.
//
// The line used to be:
//
//	s.log.Warn().Err(err).Str("prescription_id", ...).Str("drug", it.DrugName).Msg(...)
//
// .Str() bypasses logger.Redact entirely, and "drug" singular is not in the
// denylist anyway -- only "drugs" is -- so the medication went to the sink
// verbatim, on the same line as an unmasked prescription_id that makes it
// directly patient-linkable. A drug name is a diagnosis: an antiretroviral,
// an antipsychotic or a chemotherapy agent names the condition without ever
// naming it.
//
// This asserts against the actual bytes zerolog emits, not against the call
// site, because the call site is the thing that keeps changing.
func TestAttachFHIR_DoesNotWriteDrugNamesToTheLog(t *testing.T) {
	var sink bytes.Buffer
	log := zerolog.New(&sink)
	accessSvc := access.NewService(access.NewRepository(), nil, log)
	svc := NewService(NewRepository(), nil, nil,
		alwaysFailingFHIR{err: errors.New("fhir: POST MedicationRequest failed: 422 (OperationOutcome error/invalid)")},
		events.NewOutbox("test"), accessSvc,
		Config{HMACSecret: []byte("test-secret-at-least-32-bytes-long!"), VerifyBaseURL: "https://verify.yourapp.lk"}, log)

	p := Prescription{
		ID: uuid.New(), AppointmentID: uuid.New(), DoctorID: uuid.New(), PatientID: uuid.New(),
		DoctorSLMC: "SLMC12345",
		Items: []Item{
			{DrugName: "Tenofovir/Emtricitabine", Strength: "300mg", Form: "tablet", Dosage: "1 tab", Frequency: "once daily", DurationDays: 30, Quantity: 30},
			{DrugName: "Sertraline", Strength: "50mg", Form: "tablet", Dosage: "1 tab", Frequency: "once daily", DurationDays: 28, Quantity: 28, SortOrder: 1},
		},
	}

	svc.attachFHIR(context.Background(), p)

	out := sink.String()
	if out == "" {
		t.Fatal("expected attachFHIR to log the failures; it logged nothing, so this test would pass vacuously")
	}
	for _, drug := range []string{"Tenofovir", "Emtricitabine", "Sertraline"} {
		if strings.Contains(out, drug) {
			t.Errorf("the log stream contains the drug name %q -- a drug name is PHI and names the condition.\nlog:\n%s", drug, out)
		}
	}
	// The operator still has to be able to tell WHICH line failed.
	if !strings.Contains(out, `"item_index":0`) || !strings.Contains(out, `"item_index":1`) {
		t.Errorf("expected an item_index on each failure so the failing line is still identifiable.\nlog:\n%s", out)
	}
}
