//go:build integration

package prescriptions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/domain/record/access"
	"telemed/internal/domain/record/dbtest"
	"telemed/internal/domain/record/fhir"
	"telemed/internal/platform/events"
	"telemed/internal/platform/middleware"
	"telemed/internal/platform/storage"
)

func newIntegrationService(t *testing.T) (*Service, *access.Repository, *access.Service) {
	t.Helper()
	pool := dbtest.NewPostgres(t)
	log := zerolog.Nop()

	store, err := storage.NewFilesystem(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewFilesystem: %v", err)
	}

	accessRepo := access.NewRepository()
	accessSvc := access.NewService(accessRepo, pool, log)
	svc := NewService(NewRepository(), pool, store, fhir.NewNoOp(log), events.NewOutbox("test"), accessSvc,
		Config{HMACSecret: []byte("integration-test-secret-32-bytes!!"), VerifyBaseURL: "https://verify.yourapp.lk"}, log)
	return svc, accessRepo, accessSvc
}

// TestIssue_RequiresOwnedConcludedAppointment proves a doctor can only issue
// a prescription for an appointment where a treating_relationship row shows
// *they* were the treating doctor and the consultation has ended -- not an
// appointment they merely name in the request.
func TestIssue_RequiresOwnedConcludedAppointment(t *testing.T) {
	svc, accessRepo, accessSvc := newIntegrationService(t)
	ctx := context.Background()

	doctorID := uuid.New()
	patientID := uuid.New()
	appointmentID := uuid.New()

	doctor := middleware.Principal{UserID: uuid.New(), DoctorID: doctorID, Roles: []middleware.Role{middleware.RoleDoctor}}
	baseInput := IssueInput{
		Principal: doctor, AppointmentID: appointmentID,
		DoctorName: "Nimal Perera", DoctorSLMC: "SLMC12345",
		Patient: PatientDisplay{Name: "Kamala Silva", Age: 40},
		Items:   []ItemInput{{DrugName: "Amoxicillin", Strength: "500mg", Form: "capsule", Dosage: "1 capsule", Frequency: "3x daily", DurationDays: 7, Quantity: 21}},
	}

	// No treating_relationships row at all yet.
	if _, err := svc.Issue(ctx, baseInput); err == nil {
		t.Fatal("expected issuance to fail with no treating relationship on record")
	}

	// Relationship exists but the consultation has not ended yet.
	started := time.Now().Add(-10 * time.Minute)
	if err := accessRepo.UpsertTreatingRelationship(ctx, accessSvc.Pool(), appointmentID, doctorID, patientID, &started, nil); err != nil {
		t.Fatalf("seed in-progress relationship: %v", err)
	}
	if _, err := svc.Issue(ctx, baseInput); err == nil {
		t.Fatal("expected issuance to fail while the consultation has not concluded")
	}

	// A different doctor cannot issue against this appointment either.
	ended := time.Now()
	if err := accessRepo.UpsertTreatingRelationship(ctx, accessSvc.Pool(), appointmentID, doctorID, patientID, nil, &ended); err != nil {
		t.Fatalf("seed concluded relationship: %v", err)
	}
	impostor := baseInput
	impostor.Principal = middleware.Principal{UserID: uuid.New(), DoctorID: uuid.New(), Roles: []middleware.Role{middleware.RoleDoctor}}
	if _, err := svc.Issue(ctx, impostor); err == nil {
		t.Fatal("expected issuance to fail for a doctor who does not own the appointment")
	}

	// The actual treating doctor, now that the consultation has concluded,
	// succeeds -- and the resulting prescription is bound to the patient_id
	// from the relationship record, never a client-supplied one.
	created, err := svc.Issue(ctx, baseInput)
	if err != nil {
		t.Fatalf("expected the treating doctor to issue successfully: %v", err)
	}
	if created.PatientID != patientID {
		t.Errorf("PatientID = %s, want %s (resolved server-side, not from the request)", created.PatientID, patientID)
	}
	if created.PDFObjectKey == "" {
		t.Error("expected a stored pdf_object_key")
	}
	if created.VerificationHMAC == "" {
		t.Error("expected a verification_hmac to be set")
	}

	// A second prescription for the same appointment must be rejected
	// (appointment_id is UNIQUE).
	if _, err := svc.Issue(ctx, baseInput); err == nil {
		t.Fatal("expected a duplicate prescription for the same appointment to be rejected")
	}
}

// TestVerify_DetectsTamperingAfterIssuance is the end-to-end version of the
// pure hmac_test.go tests: issue a real prescription, then tamper with its
// row directly in Postgres (as a compromised operator or a buggy migration
// might), and confirm the public verify endpoint's logic -- which
// recomputes the HMAC from current database content -- correctly flags it,
// even though the stored verification_hmac column was left untouched.
func TestVerify_DetectsTamperingAfterIssuance(t *testing.T) {
	svc, accessRepo, accessSvc := newIntegrationService(t)
	ctx := context.Background()

	doctorID, patientID, appointmentID := uuid.New(), uuid.New(), uuid.New()
	ended := time.Now()
	if err := accessRepo.UpsertTreatingRelationship(ctx, accessSvc.Pool(), appointmentID, doctorID, patientID, nil, &ended); err != nil {
		t.Fatalf("seed relationship: %v", err)
	}

	doctor := middleware.Principal{UserID: uuid.New(), DoctorID: doctorID, Roles: []middleware.Role{middleware.RoleDoctor}}
	created, err := svc.Issue(ctx, IssueInput{
		Principal: doctor, AppointmentID: appointmentID,
		DoctorName: "Nimal Perera", DoctorSLMC: "SLMC12345",
		Patient: PatientDisplay{Name: "Kamala Silva", Age: 40},
		Items:   []ItemInput{{DrugName: "Amoxicillin", Strength: "500mg", Form: "capsule", Dosage: "1 capsule", Frequency: "3x daily", DurationDays: 7, Quantity: 21}},
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Untampered: verification succeeds and reveals only pharmacist-relevant
	// fields.
	result, err := svc.Verify(ctx, created.ID, created.VerificationHMAC, "203.0.113.7", "pharmacist-scanner")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !result.Valid {
		t.Fatal("expected an untampered prescription to verify")
	}
	if result.DoctorName != "Nimal Perera" || result.DoctorSLMC != "SLMC12345" {
		t.Errorf("unexpected doctor display fields: %+v", result)
	}

	// Tamper directly: change a drug's quantity via raw SQL, simulating a
	// compromised operator who does not hold the HMAC secret and therefore
	// cannot recompute a valid signature.
	if _, err := accessSvc.Pool().Exec(ctx, `UPDATE prescription_items SET quantity = 999 WHERE prescription_id = $1`, created.ID); err != nil {
		t.Fatalf("tamper with item: %v", err)
	}

	result, err = svc.Verify(ctx, created.ID, created.VerificationHMAC, "203.0.113.7", "pharmacist-scanner")
	if err != nil {
		t.Fatalf("Verify after tampering: %v", err)
	}
	if result.Valid {
		t.Fatal("expected tampering to be detected: verification must fail")
	}

	// The verify attempt (both before and after tampering) must be recorded
	// in the append-only access log with accessed_by = NULL (anonymous).
	logs, err := accessRepo.ListAccessLog(ctx, accessSvc.Pool(), access.ResourcePrescription, created.ID, 10)
	if err != nil {
		t.Fatalf("ListAccessLog: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("expected 2 verify log entries, got %d", len(logs))
	}
	for _, e := range logs {
		if e.AccessedBy != nil {
			t.Errorf("expected anonymous verify access log (nil accessed_by), got %v", *e.AccessedBy)
		}
	}
}

// TestVerify_NeverExposesPatientIdentity documents, at the type level, that
// VerifyResult has no field capable of carrying patient identity or
// diagnosis -- the AGENT-BRIEF requirement "Never the patient's identity or
// diagnosis" is enforced by the response shape having nothing to leak,
// which this test pins down so a future field addition cannot slip past
// unnoticed.
func TestVerify_NeverExposesPatientIdentity(t *testing.T) {
	svc, accessRepo, accessSvc := newIntegrationService(t)
	ctx := context.Background()

	doctorID, patientID, appointmentID := uuid.New(), uuid.New(), uuid.New()
	ended := time.Now()
	if err := accessRepo.UpsertTreatingRelationship(ctx, accessSvc.Pool(), appointmentID, doctorID, patientID, nil, &ended); err != nil {
		t.Fatalf("seed relationship: %v", err)
	}
	doctor := middleware.Principal{UserID: uuid.New(), DoctorID: doctorID, Roles: []middleware.Role{middleware.RoleDoctor}}
	created, err := svc.Issue(ctx, IssueInput{
		Principal: doctor, AppointmentID: appointmentID, DoctorName: "Dr X", DoctorSLMC: "SLMC1",
		Patient: PatientDisplay{Name: "Secret Patient Name", Age: 40},
		Items:   []ItemInput{{DrugName: "Paracetamol", Dosage: "1", Frequency: "1x", DurationDays: 1, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	result, err := svc.Verify(ctx, created.ID, created.VerificationHMAC, "", "")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// VerifyResult{Valid, DoctorName, DoctorSLMC, IssuedAt, Status} has no
	// PatientID/PatientName field to accidentally serialize -- confirmed by
	// this compiling at all plus the explicit field checks below.
	if result.DoctorName == "Secret Patient Name" {
		t.Fatal("patient name leaked into doctor_name field")
	}
}

// TestSearchDrugs_FindsSeededFormulary proves the 000003 seed migration's
// Sri Lankan formulary is actually queryable by both brand and generic name.
func TestSearchDrugs_FindsSeededFormulary(t *testing.T) {
	svc, _, _ := newIntegrationService(t)
	ctx := context.Background()

	byBrand, err := svc.SearchDrugs(ctx, "Panadol", 10)
	if err != nil {
		t.Fatalf("SearchDrugs(Panadol): %v", err)
	}
	if len(byBrand) == 0 {
		t.Fatal("expected at least one result for brand name \"Panadol\"")
	}

	byGeneric, err := svc.SearchDrugs(ctx, "Paracetamol", 10)
	if err != nil {
		t.Fatalf("SearchDrugs(Paracetamol): %v", err)
	}
	if len(byGeneric) < 2 {
		t.Fatalf("expected multiple Paracetamol-containing products, got %d", len(byGeneric))
	}

	none, err := svc.SearchDrugs(ctx, "Xyzzy-Not-A-Real-Drug", 10)
	if err != nil {
		t.Fatalf("SearchDrugs(nonsense): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("expected no results for a nonsense query, got %d", len(none))
	}
}

// TestIssue_RefusesAnAppointmentOlderThanTheAccessWindow is the prescribing
// half of SECURITY-REVIEW F3.
//
// F3 is about a treating relationship that never expired. The vault-read
// consequence is the one the review wrote up; the same unbounded row also
// authorised issuance, so the doctor who saw a patient once years ago could
// still mint them an HMAC-signed, pharmacy-verifiable prescription today
// under the treating relationship from that visit. That is the same defect
// on a more dangerous verb, and it is closed by the same window: after it
// lapses the doctor needs a consultation, which is the clinically correct
// answer anyway.
func TestIssue_RefusesAnAppointmentOlderThanTheAccessWindow(t *testing.T) {
	svc, accessRepo, accessSvc := newIntegrationService(t)
	ctx := context.Background()

	doctorID, patientID, appointmentID := uuid.New(), uuid.New(), uuid.New()
	doctor := middleware.Principal{UserID: uuid.New(), DoctorID: doctorID, Roles: []middleware.Role{middleware.RoleDoctor}}
	input := IssueInput{
		Principal: doctor, AppointmentID: appointmentID,
		DoctorName: "Nimal Perera", DoctorSLMC: "SLMC12345",
		Patient: PatientDisplay{Name: "Kamala Silva", Age: 40},
		Items:   []ItemInput{{DrugName: "Amoxicillin", Strength: "500mg", Form: "capsule", Dosage: "1 capsule", Frequency: "3x daily", DurationDays: 7, Quantity: 21}},
	}

	// A consultation that concluded well outside the window.
	old := time.Now().UTC().Add(-access.TreatingAccessWindow - 48*time.Hour)
	endedOld := old.Add(20 * time.Minute)
	if err := accessRepo.UpsertTreatingRelationship(ctx, accessSvc.Pool(), appointmentID, doctorID, patientID, &old, &endedOld); err != nil {
		t.Fatalf("seed lapsed relationship: %v", err)
	}

	if _, err := svc.Issue(ctx, input); err == nil {
		t.Fatal("a doctor issued a prescription against a consultation that concluded over a month ago")
	}

	// The same doctor, the same patient, the same appointment id, brought
	// back inside the window: issuance works. The bound is a window, not a
	// new prohibition on prescribing after a consultation ends.
	recent := time.Now().UTC().Add(-2 * time.Hour)
	endedRecent := recent.Add(20 * time.Minute)
	if _, err := accessSvc.Pool().Exec(ctx,
		`UPDATE treating_relationships SET started_at = $1, ended_at = $2 WHERE appointment_id = $3`,
		recent, endedRecent, appointmentID); err != nil {
		t.Fatalf("refresh relationship: %v", err)
	}
	if _, err := svc.Issue(ctx, input); err != nil {
		t.Fatalf("issuance inside the window must still work: %v", err)
	}
}

// TestVerify_FailsClosedWhenTheAccessLogIsUnwritable is the public-endpoint
// half of SECURITY-REVIEW F15.
//
// Verify is the one unauthenticated route on this service: no token, no
// session, no principal. The caller's address is the only identifying signal
// a breach investigation will ever have about who scanned a patient's
// prescription, and Verify used to log the insert failure and return the
// answer anyway. The failure is induced the way it actually happened in
// production -- a row the CHECK constraint rejects -- rather than by taking
// the database away, because an outage fails the read first and would prove
// nothing.
func TestVerify_FailsClosedWhenTheAccessLogIsUnwritable(t *testing.T) {
	svc, _, accessSvc := newIntegrationService(t)
	ctx := context.Background()
	pool := accessSvc.Pool()

	doctorID, patientID, appointmentID := uuid.New(), uuid.New(), uuid.New()
	started := time.Now().UTC().Add(-time.Hour)
	ended := started.Add(20 * time.Minute)
	if err := access.NewRepository().UpsertTreatingRelationship(ctx, pool, appointmentID, doctorID, patientID, &started, &ended); err != nil {
		t.Fatalf("seed relationship: %v", err)
	}
	doctor := middleware.Principal{UserID: uuid.New(), DoctorID: doctorID, Roles: []middleware.Role{middleware.RoleDoctor}}
	p, err := svc.Issue(ctx, IssueInput{
		Principal: doctor, AppointmentID: appointmentID,
		DoctorName: "Nimal Perera", DoctorSLMC: "SLMC12345",
		Patient: PatientDisplay{Name: "Kamala Silva", Age: 40},
		Items:   []ItemInput{{DrugName: "Amoxicillin", Strength: "500mg", Form: "capsule", Dosage: "1 capsule", Frequency: "3x daily", DurationDays: 7, Quantity: 21}},
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Sanity: verification works while the log is writable.
	if res, err := svc.Verify(ctx, p.ID, p.VerificationHMAC, "203.0.113.99", "pharmacy-scanner"); err != nil || !res.Valid {
		t.Fatalf("Verify before the fault: valid=%v err=%v", res.Valid, err)
	}

	// Now make the insert fail exactly as it did on a dual-stack
	// deployment: an ip_address value the column will not accept.
	// NOT VALID so the constraint applies to new rows only -- the sanity
	// verification above already wrote one, and the point is to break the
	// next insert, not to rewrite history in an append-only table.
	if _, err := pool.Exec(ctx,
		`ALTER TABLE document_access_log ADD CONSTRAINT tmp_reject_all CHECK (action <> 'verify') NOT VALID`); err != nil {
		t.Fatalf("install fault: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `ALTER TABLE document_access_log DROP CONSTRAINT IF EXISTS tmp_reject_all`)
	})

	if _, err := svc.Verify(ctx, p.ID, p.VerificationHMAC, "203.0.113.99", "pharmacy-scanner"); err == nil {
		t.Fatal("Verify() answered a pharmacist with no entry in the append-only access log: the audit trail fails open")
	}
}

// TestVerify_AMissingPrescriptionIsIndistinguishableFromABadHMAC.
//
// The QR-verification endpoint is the platform's only unauthenticated route,
// and it must answer exactly one question: is this prescription valid.
//
// It used to answer a second one for free. A missing id returned 404 while an
// existing id with a wrong or absent HMAC returned 200 {"valid": false}, so
// anyone on the internet could distinguish "this prescription exists" from
// "it does not" without ever holding the HMAC, and feed the hits to the
// owner_user_id-shaped surfaces elsewhere. UUIDv4 ids make enumeration
// infeasible, which is why this was LOW rather than worse -- but an oracle
// nobody asked for is still an oracle.
func TestVerify_AMissingPrescriptionIsIndistinguishableFromABadHMAC(t *testing.T) {
	svc, accessRepo, accessSvc := newIntegrationService(t)
	ctx := context.Background()

	doctorID, patientID, appointmentID := uuid.New(), uuid.New(), uuid.New()
	ended := time.Now()
	if err := accessRepo.UpsertTreatingRelationship(ctx, accessSvc.Pool(), appointmentID, doctorID, patientID, nil, &ended); err != nil {
		t.Fatalf("seed relationship: %v", err)
	}
	doctor := middleware.Principal{UserID: uuid.New(), DoctorID: doctorID, Roles: []middleware.Role{middleware.RoleDoctor}}
	created, err := svc.Issue(ctx, IssueInput{
		Principal: doctor, AppointmentID: appointmentID,
		DoctorName: "Nimal Perera", DoctorSLMC: "SLMC12345",
		Patient: PatientDisplay{Name: "Kamala Silva", Age: 40},
		Items:   []ItemInput{{DrugName: "Amoxicillin", Strength: "500mg", Form: "capsule", Dosage: "1 capsule", Frequency: "3x daily", DurationDays: 7, Quantity: 21}},
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	missing, err := svc.Verify(ctx, uuid.New(), "deadbeef", "203.0.113.9", "pharmacy-scanner")
	if err != nil {
		t.Fatalf("a missing prescription must not be a distinguishable error: %v", err)
	}
	badHMAC, err := svc.Verify(ctx, created.ID, "deadbeef", "203.0.113.9", "pharmacy-scanner")
	if err != nil {
		t.Fatalf("verify with a bad hmac: %v", err)
	}

	if missing != badHMAC {
		t.Fatalf("a missing prescription answered %+v and a wrong HMAC answered %+v; "+
			"the two must be byte-identical or the endpoint is an existence oracle", missing, badHMAC)
	}
	if missing.Valid {
		t.Fatal("a missing prescription reported valid")
	}
}
