//go:build integration

package clinicalnotes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"telemed/internal/domain/record/access"
	"telemed/internal/domain/record/dbtest"
	"telemed/internal/domain/record/fhir"
	"telemed/internal/platform/events"
	"telemed/internal/platform/httpx"
	"telemed/internal/platform/middleware"
)

// fixture is one integration environment: a real Postgres with every
// migration applied, a real access service, and the clinical notes service
// wired to both.
type fixture struct {
	pool      *pgxpool.Pool
	svc       *Service
	accessSvc *access.Service
	repo      *Repository

	appointmentID uuid.UUID
	doctorID      uuid.UUID
	patientUserID uuid.UUID
	doctorUserID  uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := dbtest.NewPostgres(t)
	log := zerolog.Nop()

	accessRepo := access.NewRepository()
	accessSvc := access.NewService(accessRepo, pool, log)
	repo := NewRepository()
	svc := NewService(repo, pool, fhir.NewNoOp(log), events.NewOutbox("test"), accessSvc, log)

	f := &fixture{
		pool: pool, svc: svc, accessSvc: accessSvc, repo: repo,
		appointmentID: uuid.New(), doctorID: uuid.New(),
		patientUserID: uuid.New(), doctorUserID: uuid.New(),
	}

	// The treating relationship is what authorises everything here. It
	// normally arrives from a consumed consultation.started event; seeding it
	// directly is the same thing the consumer would have done.
	started := time.Now().Add(-15 * time.Minute)
	if err := accessRepo.UpsertTreatingRelationship(context.Background(), pool,
		f.appointmentID, f.doctorID, f.patientUserID, &started, nil); err != nil {
		t.Fatalf("seed treating relationship: %v", err)
	}
	return f
}

func (f *fixture) doctor() middleware.Principal {
	return middleware.Principal{UserID: f.doctorUserID, DoctorID: f.doctorID, Roles: []middleware.Role{middleware.RoleDoctor}}
}

func (f *fixture) patient() middleware.Principal {
	return middleware.Principal{UserID: f.patientUserID, Roles: []middleware.Role{middleware.RolePatient}}
}

func (f *fixture) otherDoctor() middleware.Principal {
	return middleware.Principal{UserID: uuid.New(), DoctorID: uuid.New(), Roles: []middleware.Role{middleware.RoleDoctor}}
}

func (f *fixture) admin() middleware.Principal {
	return middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{middleware.RoleAdmin}}
}

func (f *fixture) superAdmin() middleware.Principal {
	return middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{middleware.RoleSuperAdmin}}
}

func (f *fixture) anonymous() middleware.Principal { return middleware.Principal{} }

// saveDraft is the happy-path autosave used as a setup step throughout.
func (f *fixture) saveDraft(t *testing.T, version int, diagnoses ...DiagnosisInput) Note {
	t.Helper()
	n, err := f.svc.Save(context.Background(), SaveInput{
		Principal: f.doctor(), AppointmentID: f.appointmentID,
		Subjective: soapSubjective, Objective: soapObjective,
		Assessment: soapAssessment, Plan: soapPlan,
		Diagnoses: diagnoses, Version: version,
	})
	if err != nil {
		t.Fatalf("saveDraft(version=%d): %v", version, err)
	}
	return n
}

func statusOf(t *testing.T, err error) int {
	t.Helper()
	apiErr := &httpx.APIError{}
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not an *httpx.APIError: %v", err)
	}
	return apiErr.Status()
}

func codeOf(t *testing.T, err error) httpx.ErrorCode {
	t.Helper()
	apiErr := &httpx.APIError{}
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not an *httpx.APIError: %v", err)
	}
	return apiErr.Code
}

// TestAutosave_UpsertIsIdempotentAndVersionGuarded walks the exact sequence
// the doctor app performs: first keystroke creates, subsequent keystrokes
// update, an unchanged body is a no-op, and a stale version is a clean 409
// rather than a lost update.
func TestAutosave_UpsertIsIdempotentAndVersionGuarded(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// First keystroke: no note exists, client sends version 0.
	created, err := f.svc.Save(ctx, SaveInput{
		Principal: f.doctor(), AppointmentID: f.appointmentID,
		Subjective: "Fever", Version: 0,
	})
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	if created.Version != 1 {
		t.Errorf("Version = %d, want 1 on creation", created.Version)
	}
	if created.Status != StatusDraft {
		t.Errorf("Status = %s, want draft", created.Status)
	}
	// The doctor and patient come from the treating relationship, never from
	// the request.
	if created.DoctorID != f.doctorID || created.PatientID != f.patientUserID {
		t.Errorf("note bound to doctor=%s patient=%s, want %s/%s",
			created.DoctorID, created.PatientID, f.doctorID, f.patientUserID)
	}

	// Second keystroke.
	updated, err := f.svc.Save(ctx, SaveInput{
		Principal: f.doctor(), AppointmentID: f.appointmentID,
		Subjective: "Fever for 4 days", Version: created.Version,
	})
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if updated.Version != 2 {
		t.Errorf("Version = %d, want 2", updated.Version)
	}

	// Re-sending identical content is not an edit. This is the property that
	// stops a retried request from advancing the version and turning the
	// other device's in-flight save into a spurious conflict.
	same, err := f.svc.Save(ctx, SaveInput{
		Principal: f.doctor(), AppointmentID: f.appointmentID,
		Subjective: "Fever for 4 days", Version: updated.Version,
	})
	if err != nil {
		t.Fatalf("idempotent save: %v", err)
	}
	if same.Version != updated.Version {
		t.Errorf("an unchanged save advanced the version from %d to %d", updated.Version, same.Version)
	}

	// A stale version is refused, and nothing is written.
	_, err = f.svc.Save(ctx, SaveInput{
		Principal: f.doctor(), AppointmentID: f.appointmentID,
		Subjective: "STALE DEVICE WROTE THIS", Version: updated.Version - 1,
	})
	if err == nil {
		t.Fatal("expected a conflict for a stale version")
	}
	if got := statusOf(t, err); got != http.StatusConflict {
		t.Errorf("stale save status = %d, want 409", got)
	}
	current, _, err := f.repo.GetByAppointment(ctx, f.pool, f.appointmentID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if current.Subjective != "Fever for 4 days" {
		t.Errorf("the stale save was not rejected cleanly; subjective = %q", current.Subjective)
	}

	// Claiming to update a note that does not exist is also a conflict, not a
	// silent create -- that shape means the client is talking to a restored
	// database and should be told.
	_, err = f.svc.Save(ctx, SaveInput{
		Principal: f.doctor(), AppointmentID: uuid.New(), Subjective: "x", Version: 3,
	})
	if err == nil {
		t.Fatal("expected an error saving version>0 against a nonexistent note")
	}
}

// TestAutosave_ConcurrentDevicesProduceExactlyOneWinner is the concurrency
// test that matters for this feature, in the shape it actually occurs: one
// doctor, a phone and a tablet, both holding version N, both saving.
//
// Exactly one must win. Every loser must get a clean 409 -- not a 500, not a
// raw Postgres error, and above all not a silent success that discards the
// winner's text.
func TestAutosave_ConcurrentDevicesProduceExactlyOneWinner(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	base := f.saveDraft(t, 0)

	const devices = 16
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		conflicts int
		other     []error
		winners   []string
	)
	for i := range devices {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			text := "device " + string(rune('A'+i)) + " wrote this assessment"
			out, err := f.svc.Save(ctx, SaveInput{
				Principal: f.doctor(), AppointmentID: f.appointmentID,
				Subjective: soapSubjective, Objective: soapObjective,
				Assessment: text, Plan: soapPlan,
				Version: base.Version,
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
				winners = append(winners, out.Assessment)
			case statusOfNoT(err) == http.StatusConflict:
				conflicts++
			default:
				other = append(other, err)
			}
		}(i)
	}
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("every loser must fail with a clean 409; got %d other errors, first: %v", len(other), other[0])
	}
	if succeeded != 1 {
		t.Fatalf("expected exactly 1 winner, got %d (conflicts=%d)", succeeded, conflicts)
	}
	if conflicts != devices-1 {
		t.Errorf("expected %d conflicts, got %d", devices-1, conflicts)
	}

	// No lost update: the stored text is the winner's, not a mixture and not
	// a later loser's.
	final, ok, err := f.repo.GetByAppointment(ctx, f.pool, f.appointmentID)
	if err != nil || !ok {
		t.Fatalf("re-read: %v ok=%v", err, ok)
	}
	if final.Assessment != winners[0] {
		t.Errorf("stored assessment = %q, want the winner's %q", final.Assessment, winners[0])
	}
	if final.Version != base.Version+1 {
		t.Errorf("version = %d, want exactly one increment to %d", final.Version, base.Version+1)
	}
}

func statusOfNoT(err error) int {
	apiErr := &httpx.APIError{}
	if !errors.As(err, &apiErr) {
		return 0
	}
	return apiErr.Status()
}

// TestAuthorizationMatrix is the read matrix in both note states, driven
// through the real service against a real database, including the
// document_access_log row every outcome must leave behind.
func TestAuthorizationMatrix(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A doctor with a patient-granted share, to prove sharing still works
	// for a finalised note and still does not reveal a draft.
	sharedDoctorID := uuid.New()
	sharedDoctor := middleware.Principal{UserID: uuid.New(), DoctorID: sharedDoctorID, Roles: []middleware.Role{middleware.RoleDoctor}}
	if _, err := f.accessSvc.CreateShare(ctx, f.patient(), f.patientUserID, sharedDoctorID, 24*time.Hour); err != nil {
		t.Fatalf("create share: %v", err)
	}

	draft := f.saveDraft(t, 0)

	type row struct {
		name       string
		principal  middleware.Principal
		wantStatus int // 0 means success
	}

	// While the note is a DRAFT only its author may see it. Everyone else --
	// including the patient whose record it will become -- gets 404: from
	// their side there is genuinely no note for this consultation yet, and
	// 403 would confirm one exists.
	draftRows := []row{
		{"treating doctor (author) reads their own draft", f.doctor(), 0},
		{"patient cannot read a draft", f.patient(), http.StatusNotFound},
		{"another doctor cannot read a draft", f.otherDoctor(), http.StatusNotFound},
		{"doctor with a share cannot read a draft", sharedDoctor, http.StatusNotFound},
		{"admin cannot read a draft", f.admin(), http.StatusNotFound},
		{"super_admin cannot read a draft", f.superAdmin(), http.StatusNotFound},
		{"anonymous cannot read a draft", f.anonymous(), http.StatusNotFound},
	}
	for _, tc := range draftRows {
		t.Run("draft/"+tc.name, func(t *testing.T) {
			_, err := f.svc.Get(ctx, tc.principal, f.appointmentID, "10.0.0.1", "test")
			assertOutcome(t, err, tc.wantStatus)
		})
	}

	// Finalise, then run the same matrix. Now the note is a record.
	finalised, err := f.svc.Finalise(ctx, FinaliseInput{
		Principal: f.doctor(), AppointmentID: f.appointmentID, Version: draft.Version,
		IPAddress: "10.0.0.1", UserAgent: "test",
	})
	if err != nil {
		t.Fatalf("finalise: %v", err)
	}
	if finalised.Status != StatusFinalised || finalised.FinalisedAt == nil {
		t.Fatalf("finalise did not set status/finalised_at: %+v", finalised.Status)
	}

	finalRows := []row{
		{"treating doctor reads the finalised note", f.doctor(), 0},
		{"patient reads their own finalised note", f.patient(), 0},
		{"doctor with an active share reads it", sharedDoctor, 0},
		{"another doctor with no relationship is refused", f.otherDoctor(), http.StatusForbidden},
		{"admin is refused clinical content", f.admin(), http.StatusForbidden},
		{"super_admin is refused clinical content", f.superAdmin(), http.StatusForbidden},
		{"anonymous is refused", f.anonymous(), http.StatusForbidden},
	}
	for _, tc := range finalRows {
		t.Run("finalised/"+tc.name, func(t *testing.T) {
			_, err := f.svc.Get(ctx, tc.principal, f.appointmentID, "10.0.0.1", "test")
			assertOutcome(t, err, tc.wantStatus)
		})
	}

	// Every one of those reads -- granted and denied -- must have left a row.
	total := len(draftRows) + len(finalRows)
	var logged int
	if err := f.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM document_access_log WHERE resource_type = 'clinical_note' AND action = 'view'`).
		Scan(&logged); err != nil {
		t.Fatalf("count access log: %v", err)
	}
	if logged != total {
		t.Errorf("document_access_log holds %d view rows, want %d -- every read must be audited", logged, total)
	}

	// And the admin refusals must be greppable as such, not lost in a
	// generic denial reason.
	var adminDenials int
	if err := f.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM document_access_log
		 WHERE resource_type = 'clinical_note' AND granted = FALSE AND reason = $1`,
		string(access.ReasonDeniedAdminClinical)).Scan(&adminDenials); err != nil {
		t.Fatalf("count admin denials: %v", err)
	}
	if adminDenials != 2 {
		t.Errorf("expected 2 logged admin/super_admin refusals with reason %q, got %d",
			access.ReasonDeniedAdminClinical, adminDenials)
	}
}

func assertOutcome(t *testing.T, err error, wantStatus int) {
	t.Helper()
	if wantStatus == 0 {
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("expected status %d, got success", wantStatus)
	}
	if got := statusOf(t, err); got != wantStatus {
		t.Errorf("status = %d, want %d (err: %v)", got, wantStatus, err)
	}
}

// TestWriteAuthorization proves the write rule is narrower than the read
// rule: only the treating doctor authors the note, and a doctor holding a
// patient-granted share -- who may READ a finalised note -- may not write
// one.
func TestWriteAuthorization(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	sharedDoctorID := uuid.New()
	sharedDoctor := middleware.Principal{UserID: uuid.New(), DoctorID: sharedDoctorID, Roles: []middleware.Role{middleware.RoleDoctor}}
	if _, err := f.accessSvc.CreateShare(ctx, f.patient(), f.patientUserID, sharedDoctorID, 24*time.Hour); err != nil {
		t.Fatalf("create share: %v", err)
	}

	for _, tc := range []struct {
		name string
		p    middleware.Principal
	}{
		{"the patient", f.patient()},
		{"another doctor", f.otherDoctor()},
		{"a doctor holding a read share", sharedDoctor},
		{"an admin", f.admin()},
		{"a super_admin", f.superAdmin()},
		{"an anonymous caller", f.anonymous()},
	} {
		t.Run(tc.name+" cannot write", func(t *testing.T) {
			_, err := f.svc.Save(ctx, SaveInput{
				Principal: tc.p, AppointmentID: f.appointmentID, Subjective: "unauthorised", Version: 0,
			})
			if err == nil {
				t.Fatal("expected the write to be refused")
			}
			if got := statusOf(t, err); got != http.StatusForbidden {
				t.Errorf("status = %d, want 403", got)
			}
		})
	}

	// And no note was created by any of them.
	if _, ok, err := f.repo.GetByAppointment(ctx, f.pool, f.appointmentID); err != nil || ok {
		t.Fatalf("a refused write created a note (ok=%v, err=%v)", ok, err)
	}
}

// TestFinaliseAmendRevisionChain is the legal-record test: finalising
// captures the text as signed, amending never overwrites it, and the trail
// says who changed what and why.
func TestFinaliseAmendRevisionChain(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	const (
		originalAssessment = "Viral fever, likely self-limiting."
		amendedAssessment  = "Dengue fever confirmed on NS1 antigen. Warning signs absent."
		amendmentReason    = "NS1 antigen returned positive after the consultation"
	)

	// Seed a real ICD-10 code so the diagnosis path is exercised end to end.
	draft, err := f.svc.Save(ctx, SaveInput{
		Principal: f.doctor(), AppointmentID: f.appointmentID,
		Subjective: soapSubjective, Objective: soapObjective,
		Assessment: originalAssessment, Plan: soapPlan,
		Diagnoses: []DiagnosisInput{{Code: "B34.9", IsPrimary: true}},
		Version:   0,
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(draft.Diagnoses) != 1 || draft.Diagnoses[0].Display == "" {
		t.Fatalf("the display term must be resolved server-side from icd10_codes, got %+v", draft.Diagnoses)
	}

	// A draft cannot be amended -- there is nothing signed to amend.
	if _, err := f.svc.Amend(ctx, AmendInput{
		Principal: f.doctor(), AppointmentID: f.appointmentID,
		Assessment: strPtr(amendedAssessment), Reason: amendmentReason, Version: draft.Version,
	}); err == nil {
		t.Fatal("expected amending a draft to be refused")
	} else if got := statusOf(t, err); got != http.StatusConflict {
		t.Errorf("amend-a-draft status = %d, want 409", got)
	}

	finalised, err := f.svc.Finalise(ctx, FinaliseInput{
		Principal: f.doctor(), AppointmentID: f.appointmentID, Version: draft.Version,
		IPAddress: "10.0.0.1", UserAgent: "test",
	})
	if err != nil {
		t.Fatalf("finalise: %v", err)
	}

	// A finalised note cannot be edited through the draft path, and the code
	// says so distinctly so a client does not retry forever.
	_, err = f.svc.Save(ctx, SaveInput{
		Principal: f.doctor(), AppointmentID: f.appointmentID,
		Assessment: "sneaky rewrite", Version: finalised.Version,
	})
	if err == nil {
		t.Fatal("expected saving a finalised note as a draft to be refused")
	}
	if got := codeOf(t, err); got != httpx.CodeNoteFinalised {
		t.Errorf("code = %s, want %s", got, httpx.CodeNoteFinalised)
	}

	// Finalising twice is refused, so a double tap cannot re-stamp
	// finalised_at over the original signature time.
	if _, err := f.svc.Finalise(ctx, FinaliseInput{
		Principal: f.doctor(), AppointmentID: f.appointmentID, Version: finalised.Version,
	}); err == nil {
		t.Fatal("expected a second finalise to be refused")
	}

	// Amend.
	amended, err := f.svc.Amend(ctx, AmendInput{
		Principal: f.doctor(), AppointmentID: f.appointmentID,
		Assessment: strPtr(amendedAssessment),
		Diagnoses:  &[]DiagnosisInput{{Code: "A90", IsPrimary: true}},
		Reason:     amendmentReason, Version: finalised.Version,
		IPAddress: "10.0.0.1", UserAgent: "test",
	})
	if err != nil {
		t.Fatalf("amend: %v", err)
	}
	if amended.Status != StatusFinalised {
		t.Errorf("an amendment must leave the note finalised, got %s", amended.Status)
	}
	if amended.FinalisedAt == nil || !amended.FinalisedAt.Equal(*finalised.FinalisedAt) {
		t.Errorf("finalised_at must record when the note was SIGNED, not when it was last touched: %v vs %v",
			amended.FinalisedAt, finalised.FinalisedAt)
	}
	// Sections the amendment did not name are untouched.
	if amended.Subjective != soapSubjective || amended.Plan != soapPlan {
		t.Error("an amendment must leave unnamed sections exactly as signed")
	}

	// The trail.
	revisions, err := f.svc.ListRevisions(ctx, f.doctor(), f.appointmentID, "10.0.0.1", "test")
	if err != nil {
		t.Fatalf("list revisions: %v", err)
	}
	if len(revisions) != 2 {
		t.Fatalf("expected 2 revisions, got %d", len(revisions))
	}

	first, second := revisions[0], revisions[1]
	if first.Revision != 1 || first.ChangeType != ChangeFinalise {
		t.Errorf("revision 1 = %d/%s, want 1/finalise", first.Revision, first.ChangeType)
	}
	// THE assertion this whole table exists for.
	if first.Assessment != originalAssessment {
		t.Errorf("the text as signed was not preserved: revision 1 assessment = %q, want %q",
			first.Assessment, originalAssessment)
	}
	if len(first.Diagnoses) != 1 || first.Diagnoses[0].Code != "B34.9" {
		t.Errorf("revision 1 must freeze the codes that were on the note when it was signed, got %+v", first.Diagnoses)
	}
	if first.AmendmentReason != "" {
		t.Errorf("the finalisation revision needs no reason, got %q", first.AmendmentReason)
	}

	if second.Revision != 2 || second.ChangeType != ChangeAmend {
		t.Errorf("revision 2 = %d/%s, want 2/amend", second.Revision, second.ChangeType)
	}
	if second.Assessment != amendedAssessment {
		t.Errorf("revision 2 assessment = %q, want %q", second.Assessment, amendedAssessment)
	}
	if second.AmendmentReason != amendmentReason {
		t.Errorf("revision 2 reason = %q, want %q", second.AmendmentReason, amendmentReason)
	}
	if second.ChangedBy != f.doctorUserID || second.ChangedByRole != "doctor" {
		t.Errorf("revision 2 attributed to %s/%s, want %s/doctor", second.ChangedBy, second.ChangedByRole, f.doctorUserID)
	}
	if len(second.Diagnoses) != 1 || second.Diagnoses[0].Code != "A90" {
		t.Errorf("revision 2 must carry the amended codes, got %+v", second.Diagnoses)
	}

	// An amendment with no reason, and one that changes nothing, are both
	// refused: an unexplained or empty change to a signed record is not
	// auditable.
	if _, err := f.svc.Amend(ctx, AmendInput{
		Principal: f.doctor(), AppointmentID: f.appointmentID,
		Assessment: strPtr("something else"), Reason: "   ", Version: amended.Version,
	}); err == nil {
		t.Error("expected an amendment with no reason to be refused")
	}
	if _, err := f.svc.Amend(ctx, AmendInput{
		Principal: f.doctor(), AppointmentID: f.appointmentID,
		Assessment: strPtr(amendedAssessment), Reason: "no-op", Version: amended.Version,
	}); err == nil {
		t.Error("expected an amendment that changes nothing to be refused")
	} else if got := statusOf(t, err); got != http.StatusUnprocessableEntity {
		t.Errorf("no-op amendment status = %d, want 422", got)
	}

	// Only the author may amend.
	if _, err := f.svc.Amend(ctx, AmendInput{
		Principal: f.otherDoctor(), AppointmentID: f.appointmentID,
		Assessment: strPtr("hijacked"), Reason: "not mine", Version: amended.Version,
	}); err == nil {
		t.Error("expected another doctor's amendment to be refused")
	}

	// Both write acts left an audit entry.
	var finaliseRows, amendRows int
	if err := f.pool.QueryRow(ctx,
		`SELECT COUNT(*) FILTER (WHERE action='finalise'), COUNT(*) FILTER (WHERE action='amend')
		 FROM document_access_log WHERE resource_type='clinical_note'`).Scan(&finaliseRows, &amendRows); err != nil {
		t.Fatalf("count write audit rows: %v", err)
	}
	if finaliseRows != 1 || amendRows != 1 {
		t.Errorf("audit rows: finalise=%d amend=%d, want 1 and 1", finaliseRows, amendRows)
	}
}

func strPtr(s string) *string { return &s }

// TestRevisionsAreAppendOnlyInTheDatabase goes around the application
// entirely. An application-level rule protects a legal record only until
// someone opens psql, so the guarantee has to hold at the database.
func TestRevisionsAreAppendOnlyInTheDatabase(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	draft := f.saveDraft(t, 0)
	finalised, err := f.svc.Finalise(ctx, FinaliseInput{
		Principal: f.doctor(), AppointmentID: f.appointmentID, Version: draft.Version,
	})
	if err != nil {
		t.Fatalf("finalise: %v", err)
	}

	_, err = f.pool.Exec(ctx, `UPDATE clinical_note_revisions SET assessment = 'rewritten' WHERE note_id = $1`, finalised.ID)
	if err == nil {
		t.Fatal("UPDATE on clinical_note_revisions must be refused by the database")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("unexpected UPDATE error: %v", err)
	}

	_, err = f.pool.Exec(ctx, `DELETE FROM clinical_note_revisions WHERE note_id = $1`, finalised.ID)
	if err == nil {
		t.Fatal("DELETE on clinical_note_revisions must be refused by the database")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("unexpected DELETE error: %v", err)
	}

	// The trigger message names the table it fired on, so an operator who
	// hits this at 3am is told which append-only table stopped them.
	if !strings.Contains(err.Error(), "clinical_note_revisions") {
		t.Errorf("the error should name the table, got: %v", err)
	}

	// The row survived both attempts.
	var assessment string
	if err := f.pool.QueryRow(ctx,
		`SELECT assessment FROM clinical_note_revisions WHERE note_id = $1 AND revision = 1`, finalised.ID).
		Scan(&assessment); err != nil {
		t.Fatalf("re-read revision: %v", err)
	}
	if assessment != soapAssessment {
		t.Errorf("revision text = %q, want the original %q", assessment, soapAssessment)
	}

	// And the document_access_log trigger from 000002 still works after
	// 000004 redefined the shared forbid_mutation() function.
	if _, err := f.pool.Exec(ctx, `DELETE FROM document_access_log`); err == nil {
		t.Error("document_access_log must still be append-only after migration 000004")
	}
}

// TestFinaliseRefusesAnEmptyNote: an empty signed record is not a record, it
// is a mis-click that later looks like an undocumented consultation.
func TestFinaliseRefusesAnEmptyNote(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	empty, err := f.svc.Save(ctx, SaveInput{Principal: f.doctor(), AppointmentID: f.appointmentID, Version: 0})
	if err != nil {
		t.Fatalf("save empty draft: %v", err)
	}
	_, err = f.svc.Finalise(ctx, FinaliseInput{Principal: f.doctor(), AppointmentID: f.appointmentID, Version: empty.Version})
	if err == nil {
		t.Fatal("expected finalising an empty note to be refused")
	}
	if got := statusOf(t, err); got != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", got)
	}

	// Whitespace is not content either.
	ws, err := f.svc.Save(ctx, SaveInput{
		Principal: f.doctor(), AppointmentID: f.appointmentID,
		Subjective: "   \n\t ", Version: empty.Version,
	})
	if err != nil {
		t.Fatalf("save whitespace draft: %v", err)
	}
	if _, err := f.svc.Finalise(ctx, FinaliseInput{
		Principal: f.doctor(), AppointmentID: f.appointmentID, Version: ws.Version,
	}); err == nil {
		t.Error("expected finalising a whitespace-only note to be refused")
	}
}

// TestFinalisedEventCarriesIdentifiersOnly reads the row that would actually
// be published and searches it for the note's own sentences and its ICD-10
// code. This is the strongest form of the PHI assertion: it inspects the
// bytes on their way to NATS, not the struct they came from.
func TestFinalisedEventCarriesIdentifiersOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	draft := f.saveDraft(t, 0, DiagnosisInput{Code: "A90", IsPrimary: true}, DiagnosisInput{Code: "A27.9"})
	finalised, err := f.svc.Finalise(ctx, FinaliseInput{
		Principal: f.doctor(), AppointmentID: f.appointmentID, Version: draft.Version,
	})
	if err != nil {
		t.Fatalf("finalise: %v", err)
	}

	var subject, payload string
	if err := f.pool.QueryRow(ctx,
		`SELECT subject, payload::text FROM outbox_events WHERE aggregate_id = $1`, finalised.ID.String()).
		Scan(&subject, &payload); err != nil {
		t.Fatalf("read outbox row: %v", err)
	}
	if subject != string(events.SubjectClinicalNoteFinalised) {
		t.Errorf("subject = %q, want %q", subject, events.SubjectClinicalNoteFinalised)
	}

	for _, forbidden := range []string{
		soapSubjective, soapObjective, soapAssessment, soapPlan,
		"Dengue", "dengue", "Leptospirosis", "leptospirosis",
		`"A90"`, `"A27.9"`, "retro-orbital", "platelets",
	} {
		if strings.Contains(payload, forbidden) {
			t.Errorf("the outbox payload contains %q -- clinical_note.finalised must carry identifiers only.\npayload: %s",
				forbidden, payload)
		}
	}
	// It does carry what a consumer legitimately needs. Decoded rather than
	// string-matched, because Postgres normalises JSONB whitespace and key
	// order on the way back out.
	var envelope struct {
		Payload events.ClinicalNoteFinalised `json:"payload"`
	}
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		t.Fatalf("decode outbox envelope: %v", err)
	}
	got := envelope.Payload
	if got.NoteID != finalised.ID || got.AppointmentID != finalised.AppointmentID ||
		got.PatientID != finalised.PatientID || got.DoctorID != finalised.DoctorID {
		t.Errorf("the event does not identify the note it announces: %+v", got)
	}
	if got.DiagnosisCount != 2 {
		t.Errorf("diagnosis_count = %d, want 2", got.DiagnosisCount)
	}
	if got.FinalisedAt.IsZero() {
		t.Error("finalised_at must be set")
	}

	// Exactly one event: an amendment does not re-announce a finalisation.
	if _, err := f.svc.Amend(ctx, AmendInput{
		Principal: f.doctor(), AppointmentID: f.appointmentID,
		Plan: strPtr("Reviewed; discharge advice given."), Reason: "follow-up review", Version: finalised.Version,
	}); err != nil {
		t.Fatalf("amend: %v", err)
	}
	var count int
	if err := f.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM outbox_events WHERE subject = $1`, string(events.SubjectClinicalNoteFinalised)).
		Scan(&count); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if count != 1 {
		t.Errorf("expected exactly 1 clinical_note.finalised event, got %d", count)
	}
}

// TestICD10Search covers the relevance the diagnosis picker lives or dies on,
// including the Sri Lankan entries a generic "common codes" list omits.
func TestICD10Search(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	tests := []struct {
		name     string
		query    string
		wantTop  string   // must be the first result
		wantAny  []string // must appear somewhere in the results
		wantNone bool
	}{
		{name: "exact rubric word", query: "dengue", wantTop: "A90", wantAny: []string{"A91"}},
		{name: "two words narrow it", query: "dengue haemorrhagic", wantTop: "A91"},
		{name: "code prefix returns the family", query: "E11", wantAny: []string{"E11.9", "E11.2", "E11.4"}},
		{name: "full code is the top hit", query: "E11.9", wantTop: "E11.9"},
		{name: "colloquial synonym", query: "sugar", wantAny: []string{"E11.9"}},
		{name: "abbreviation", query: "COPD", wantAny: []string{"J44.9"}},
		{name: "local disease", query: "leptospirosis", wantAny: []string{"A27.9", "A27.0"}},
		{name: "local colloquialism", query: "polonga", wantAny: []string{"T63.0"}},
		{name: "prefix match while typing", query: "hypert", wantAny: []string{"I10"}},
		{name: "british spelling in the rubric", query: "diarrhoea", wantAny: []string{"A09", "R19.7"}},
		{name: "mental health", query: "anxiety", wantAny: []string{"F41.1", "F41.9"}},
		{name: "paediatrics", query: "bronchiolitis", wantAny: []string{"J21.9"}},
		{name: "dermatology", query: "scabies", wantAny: []string{"B86"}},
		{name: "nonsense returns nothing", query: "zzzqqqxxx", wantNone: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.svc.SearchICD10(ctx, tc.query, 20)
			if err != nil {
				t.Fatalf("search %q: %v", tc.query, err)
			}
			if tc.wantNone {
				if len(got) != 0 {
					t.Errorf("search %q returned %d results, want none", tc.query, len(got))
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("search %q returned nothing", tc.query)
			}
			if tc.wantTop != "" && got[0].Code != tc.wantTop {
				t.Errorf("search %q top result = %s (%s), want %s", tc.query, got[0].Code, got[0].Display, tc.wantTop)
			}
			for _, want := range tc.wantAny {
				found := false
				for _, c := range got {
					if c.Code == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("search %q did not return %s; got %v", tc.query, want, codesOf(got))
				}
			}
			for _, c := range got {
				if c.Display == "" {
					t.Errorf("code %s has no display term", c.Code)
				}
			}
		})
	}

	// A single character is not an error -- the client fires while the doctor
	// is still typing -- but an entirely empty q is a caller bug.
	if got, err := f.svc.SearchICD10(ctx, "a", 20); err != nil || len(got) != 0 {
		t.Errorf("a one-character query should return an empty list and no error, got %d results / %v", len(got), err)
	}
	if _, err := f.svc.SearchICD10(ctx, "  ", 20); err == nil {
		t.Error("an empty q should be a 400")
	}
	// The limit is honoured and capped.
	if got, err := f.svc.SearchICD10(ctx, "diabetes", 3); err != nil || len(got) > 3 {
		t.Errorf("limit not honoured: %d results, %v", len(got), err)
	}
}

func codesOf(in []ICD10Code) []string {
	out := make([]string, len(in))
	for i, c := range in {
		out[i] = c.Code
	}
	return out
}

// TestUnknownDiagnosisCodeIsRefused: the code column is a controlled
// vocabulary. Free text smuggled through it would be PHI in a field the
// whole platform treats as a code, and would break every report that groups
// by it.
func TestUnknownDiagnosisCodeIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for _, code := range []string{"ZZ99.9", "patient is malingering", "'; DROP TABLE clinical_notes; --"} {
		_, err := f.svc.Save(ctx, SaveInput{
			Principal: f.doctor(), AppointmentID: f.appointmentID,
			Subjective: "x", Diagnoses: []DiagnosisInput{{Code: code}}, Version: 0,
		})
		if err == nil {
			t.Fatalf("code %q must be refused", code)
		}
		if got := statusOf(t, err); got != http.StatusUnprocessableEntity {
			t.Errorf("code %q: status = %d, want 422", code, got)
		}
	}

	// A known code is stored with the classification's own display term, not
	// whatever the client claimed.
	n, err := f.svc.Save(ctx, SaveInput{
		Principal: f.doctor(), AppointmentID: f.appointmentID,
		Subjective: "x", Diagnoses: []DiagnosisInput{{Code: "a90", IsPrimary: true}}, Version: 0,
	})
	if err != nil {
		t.Fatalf("save with a valid code: %v", err)
	}
	if len(n.Diagnoses) != 1 || n.Diagnoses[0].Code != "A90" {
		t.Fatalf("expected the code to be normalised to A90, got %+v", n.Diagnoses)
	}
	if !strings.Contains(strings.ToLower(n.Diagnoses[0].Display), "dengue") {
		t.Errorf("display term = %q, want the ICD-10 rubric", n.Diagnoses[0].Display)
	}
}

// TestListForDoctorCarriesNoClinicalContent: the list is metadata only,
// which is what makes it safe to serve without a per-row audit entry.
func TestListForDoctorCarriesNoClinicalContent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.saveDraft(t, 0)

	// A second appointment for the same doctor, plus one belonging to
	// somebody else that must never appear.
	second := uuid.New()
	if err := access.NewRepository().UpsertTreatingRelationship(ctx, f.pool, second, f.doctorID, uuid.New(), nil, nil); err != nil {
		t.Fatalf("seed second relationship: %v", err)
	}
	if _, err := f.svc.Save(ctx, SaveInput{
		Principal: f.doctor(), AppointmentID: second, Assessment: "Second consultation", Version: 0,
	}); err != nil {
		t.Fatalf("save second note: %v", err)
	}

	otherDoctorID := uuid.New()
	third := uuid.New()
	if err := access.NewRepository().UpsertTreatingRelationship(ctx, f.pool, third, otherDoctorID, uuid.New(), nil, nil); err != nil {
		t.Fatalf("seed third relationship: %v", err)
	}
	if _, err := f.svc.Save(ctx, SaveInput{
		Principal:     middleware.Principal{UserID: uuid.New(), DoctorID: otherDoctorID, Roles: []middleware.Role{middleware.RoleDoctor}},
		AppointmentID: third, Assessment: "Another doctor's note", Version: 0,
	}); err != nil {
		t.Fatalf("save third note: %v", err)
	}

	notes, total, err := f.svc.ListForDoctor(ctx, f.doctor(), "", 1, 20)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 2 || len(notes) != 2 {
		t.Fatalf("got %d notes (total %d), want 2 -- a doctor sees only their own", len(notes), total)
	}
	for _, n := range notes {
		if n.DoctorID != f.doctorID {
			t.Errorf("note %s belongs to doctor %s", n.ID, n.DoctorID)
		}
		if len(n.Diagnoses) != 0 {
			t.Errorf("the list must not carry diagnoses, got %+v", n.Diagnoses)
		}
	}

	// A patient cannot list notes at all.
	if _, _, err := f.svc.ListForDoctor(ctx, f.patient(), "", 1, 20); err == nil {
		t.Error("a patient must not be able to list clinical notes")
	}
	// Nor an admin.
	if _, _, err := f.svc.ListForDoctor(ctx, f.admin(), "", 1, 20); err == nil {
		t.Error("an admin must not be able to list clinical notes")
	}

	// Status filtering works.
	drafts, total, err := f.svc.ListForDoctor(ctx, f.doctor(), StatusDraft, 1, 20)
	if err != nil {
		t.Fatalf("list drafts: %v", err)
	}
	if total != 2 || len(drafts) != 2 {
		t.Errorf("expected 2 drafts, got %d", len(drafts))
	}
	finalisedNotes, total, err := f.svc.ListForDoctor(ctx, f.doctor(), StatusFinalised, 1, 20)
	if err != nil {
		t.Fatalf("list finalised: %v", err)
	}
	if total != 0 || len(finalisedNotes) != 0 {
		t.Errorf("expected no finalised notes yet, got %d", len(finalisedNotes))
	}
}

// --- HTTP wire-shape tests ----------------------------------------------

// newTestRouter mounts the real handler behind a middleware that injects an
// already-verified principal, which is what RequireAuth does in production
// after checking a signature.
func newTestRouter(h *Handler, p middleware.Principal) http.Handler {
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(middleware.WithPrincipal(req.Context(), p)))
			})
		})
		r.Mount("/clinical-notes", h.Routes())
		r.Mount("/icd10", h.ICD10Routes())
	})
	return r
}

// TestDoctorAppPayloadIsAcceptedVerbatim is the compatibility test for
// telemed-doctor-app. Its `SoapNote.toJson()` emits appointment_id,
// subjective, objective, assessment, plan, diagnoses[{code, description}] and
// updated_at, and httpx.DecodeJSON rejects unknown fields -- so every one of
// those has to be a field this handler declares, or the doctor app's existing
// payload 400s.
//
// The one thing the client must change is the method and path: it currently
// POSTs to /clinical-notes, and this service exposes
// PUT /clinical-notes/{appointment_id}. That is asserted here too, so the
// mismatch is a failing test rather than a paragraph in a report.
func TestDoctorAppPayloadIsAcceptedVerbatim(t *testing.T) {
	f := newFixture(t)
	router := newTestRouter(NewHandler(f.svc), f.doctor())

	// Byte-for-byte the shape lib/features/clinical_notes/domain/
	// clinical_note_models.dart's SoapNote.toJson() produces, plus the
	// `version` field the client must start echoing back.
	body := `{
	  "appointment_id": "` + f.appointmentID.String() + `",
	  "subjective": "Fever 4 days, retro-orbital pain.",
	  "objective": "T 38.9C, platelets 92000.",
	  "assessment": "Dengue fever with warning signs.",
	  "plan": "Admit. FBC 12-hourly. Avoid NSAIDs.",
	  "diagnoses": [{"code": "A90", "description": "Dengue fever [classic dengue]"}],
	  "updated_at": "2026-08-20T04:29:11.000Z",
	  "version": 0
	}`

	req := httptest.NewRequest(http.MethodPut, "/api/v1/clinical-notes/"+f.appointmentID.String(), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT returned %d: %s", rec.Code, rec.Body.String())
	}

	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body.String())
	}

	// Everything SoapNote.fromJson() reads must be present and of the right
	// shape, or the doctor app throws a decode error on a screen a doctor is
	// mid-consultation on.
	for _, k := range []string{"appointment_id", "subjective", "objective", "assessment", "plan", "diagnoses", "updated_at"} {
		if _, ok := envelope.Data[k]; !ok {
			t.Errorf("response is missing %q, which SoapNote.fromJson() requires: %v", k, envelope.Data)
		}
	}
	// updated_at must be DateTime.parse-able, and it is the SERVER's, not the
	// client's echoed value.
	updatedAt, ok := envelope.Data["updated_at"].(string)
	if !ok {
		t.Fatalf("updated_at is not a string: %T", envelope.Data["updated_at"])
	}
	if _, err := time.Parse(time.RFC3339, updatedAt); err != nil {
		t.Errorf("updated_at %q is not RFC3339: %v", updatedAt, err)
	}
	if strings.HasPrefix(updatedAt, "2026-08-20T04:29:11") {
		t.Error("the client's updated_at was echoed back; a phone's clock must not decide when a record was written")
	}

	diagnoses, ok := envelope.Data["diagnoses"].([]any)
	if !ok || len(diagnoses) != 1 {
		t.Fatalf("diagnoses = %v, want a one-element array", envelope.Data["diagnoses"])
	}
	dx := diagnoses[0].(map[string]any)
	// Icd10Code.fromJson() reads exactly these two and requires both non-null.
	if dx["code"] != "A90" {
		t.Errorf("diagnosis code = %v, want A90", dx["code"])
	}
	// The client's description was ignored and the classification's own
	// rubric stored instead -- note the British "classical", not the
	// client-supplied "classic".
	if desc, _ := dx["description"].(string); !strings.Contains(desc, "classical") {
		t.Errorf("description = %q, want the ICD-10 rubric rather than the client's text", desc)
	}

	// `version` is what the client must start echoing back.
	if v, _ := envelope.Data["version"].(float64); v != 1 {
		t.Errorf("version = %v, want 1", envelope.Data["version"])
	}

	// The path the doctor app currently uses does not exist. This is the
	// exact change the client needs.
	postReq := httptest.NewRequest(http.MethodPost, "/api/v1/clinical-notes", strings.NewReader(body))
	postReq.Header.Set("Content-Type", "application/json")
	postRec := httptest.NewRecorder()
	router.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusMethodNotAllowed && postRec.Code != http.StatusNotFound {
		t.Errorf("POST /clinical-notes returned %d; the client must switch to PUT /clinical-notes/{appointment_id}", postRec.Code)
	}
}

// TestICD10ResponseMatchesTheDoctorAppModel: Icd10Code.fromJson() casts
// json['code'] and json['description'] to String with no null guard, so both
// must be present on every element or the picker throws.
func TestICD10ResponseMatchesTheDoctorAppModel(t *testing.T) {
	f := newFixture(t)
	router := newTestRouter(NewHandler(f.svc), f.doctor())

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/icd10?q=dengue", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /icd10 returned %d: %s", rec.Code, rec.Body.String())
	}

	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(envelope.Data) == 0 {
		t.Fatal("expected results for \"dengue\"")
	}
	for i, item := range envelope.Data {
		code, codeOK := item["code"].(string)
		desc, descOK := item["description"].(string)
		if !codeOK || code == "" {
			t.Errorf("item %d has no string code: %v", i, item)
		}
		if !descOK || desc == "" {
			t.Errorf("item %d has no string description: %v", i, item)
		}
	}
	if envelope.Data[0]["code"] != "A90" {
		t.Errorf("top result = %v, want A90", envelope.Data[0]["code"])
	}
}

// TestHTTPConflictShapeIsActionable: the two 409s a client can receive are
// distinguishable by code, and the retryable one carries the version to
// retry with.
func TestHTTPConflictShapeIsActionable(t *testing.T) {
	f := newFixture(t)
	router := newTestRouter(NewHandler(f.svc), f.doctor())

	note := f.saveDraft(t, 0)

	// A stale save.
	stale := `{"subjective":"stale","version":` + itoa(note.Version-1) + `}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/clinical-notes/"+f.appointmentID.String(), strings.NewReader(stale))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale save returned %d: %s", rec.Code, rec.Body.String())
	}
	var apiErr struct {
		Code   string            `json:"code"`
		Fields map[string]string `json:"fields"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &apiErr); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if apiErr.Code != string(httpx.CodeConflict) {
		t.Errorf("code = %q, want CONFLICT", apiErr.Code)
	}
	if apiErr.Fields["version"] != itoa(note.Version) {
		t.Errorf("fields.version = %q, want %q so the client can retry without another GET",
			apiErr.Fields["version"], itoa(note.Version))
	}

	// Finalise, then a draft save: a DIFFERENT code, because retrying this
	// one can never succeed.
	if _, err := f.svc.Finalise(context.Background(), FinaliseInput{
		Principal: f.doctor(), AppointmentID: f.appointmentID, Version: note.Version,
	}); err != nil {
		t.Fatalf("finalise: %v", err)
	}
	body := `{"subjective":"after finalisation","version":` + itoa(note.Version+1) + `}`
	req = httptest.NewRequest(http.MethodPut, "/api/v1/clinical-notes/"+f.appointmentID.String(), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("post-finalisation save returned %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &apiErr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if apiErr.Code != string(httpx.CodeNoteFinalised) {
		t.Errorf("code = %q, want NOTE_FINALISED -- a client that sees CONFLICT here retries forever", apiErr.Code)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
