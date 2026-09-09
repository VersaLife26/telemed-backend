//go:build integration

package access

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/domain/record/dbtest"
	"telemed/internal/platform/events"
	"telemed/internal/platform/middleware"
)

// TestCheck_FullAuthorizationMatrix_AgainstRealDatabase runs the same
// owner / treating-doctor / untreating-doctor / admin / anonymous matrix as
// service_test.go's TestDecideAccess_AuthorizationMatrix, but through the
// full Service.Check path against a real Postgres: treating_relationships
// and record_shares are real rows, and every call is verified to have
// written an append-only document_access_log entry (granted or denied).
func TestCheck_FullAuthorizationMatrix_AgainstRealDatabase(t *testing.T) {
	pool := dbtest.NewPostgres(t)
	repo := NewRepository()
	svc := NewService(repo, pool, zerolog.Nop())
	ctx := context.Background()

	patient := uuid.New()
	treating := uuid.New()   // doctor who treated this patient
	untreating := uuid.New() // doctor who never treated this patient
	admin := uuid.New()

	// Seed a concluded treating relationship for `treating`.
	appointmentID := uuid.New()
	started := time.Now().Add(-time.Hour)
	ended := time.Now().Add(-time.Minute)
	if err := repo.UpsertTreatingRelationship(ctx, pool, appointmentID, treating, patient, &started, &ended); err != nil {
		t.Fatalf("seed treating relationship: %v", err)
	}

	resourceID := uuid.New()

	tests := []struct {
		name        string
		principal   middleware.Principal
		wantGranted bool
	}{
		{"owner", middleware.Principal{UserID: patient, Roles: []middleware.Role{middleware.RolePatient}}, true},
		{"treating doctor", middleware.Principal{UserID: uuid.New(), DoctorID: treating, Roles: []middleware.Role{middleware.RoleDoctor}}, true},
		{"untreating doctor", middleware.Principal{UserID: uuid.New(), DoctorID: untreating, Roles: []middleware.Role{middleware.RoleDoctor}}, false},
		// Was true. That was SECURITY-REVIEW F4 -- see
		// TestNoAdminRoleReadsAPatientVault below.
		{"admin", middleware.Principal{UserID: admin, Roles: []middleware.Role{middleware.RoleAdmin}}, false},
		{"anonymous", middleware.Principal{}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.Check(ctx, CheckOptions{
				Principal: tc.principal, OwnerUserID: patient, Resource: ResourceDocument,
				ResourceID: resourceID, Action: ActionView, IPAddress: "203.0.113.1", UserAgent: "test-agent",
			})
			granted := err == nil
			if granted != tc.wantGranted {
				t.Errorf("Check() granted = %v, want %v (err=%v)", granted, tc.wantGranted, err)
			}
		})
	}

	// Every one of the five calls above must have left an audit trail,
	// granted or denied -- that is the whole point of centralising
	// authorization through Check.
	logEntries, err := repo.ListAccessLog(ctx, pool, ResourceDocument, resourceID, 10)
	if err != nil {
		t.Fatalf("ListAccessLog: %v", err)
	}
	if len(logEntries) != len(tests) {
		t.Fatalf("expected %d access log entries, got %d", len(tests), len(logEntries))
	}
}

// TestCheck_ExpiredShareDoesNotGrantAccess proves the SQL-level expiry
// filter (ActiveShare's WHERE clause) actually excludes an expired share,
// closing the gap TestDecideAccess_ExpiredShareIsNotConsidered documents at
// the pure-function level.
func TestCheck_ExpiredShareDoesNotGrantAccess(t *testing.T) {
	pool := dbtest.NewPostgres(t)
	repo := NewRepository()
	svc := NewService(repo, pool, zerolog.Nop())
	ctx := context.Background()

	patient := uuid.New()
	doctorID := uuid.New()

	// Directly insert an already-expired share (CreateShare would refuse
	// this via CheckOptions/Validate, so we go straight to SQL to construct
	// the fixture).
	_, err := pool.Exec(ctx, `
		INSERT INTO record_shares (id, patient_id, doctor_id, granted_by, expires_at, created_at)
		VALUES ($1, $2, $3, $2, NOW() - interval '1 hour', NOW() - interval '2 hours')`,
		uuid.New(), patient, doctorID)
	if err != nil {
		t.Fatalf("seed expired share: %v", err)
	}

	err = svc.Check(ctx, CheckOptions{
		Principal:   middleware.Principal{UserID: uuid.New(), DoctorID: doctorID, Roles: []middleware.Role{middleware.RoleDoctor}},
		OwnerUserID: patient, Resource: ResourceDocument, ResourceID: uuid.New(), Action: ActionView,
	})
	if err == nil {
		t.Fatal("expected an expired share to deny access")
	}
}

// TestCheck_RevokedShareDoesNotGrantAccess proves RevokeShare's effect is
// actually observed by the next Check call.
func TestCheck_RevokedShareDoesNotGrantAccess(t *testing.T) {
	pool := dbtest.NewPostgres(t)
	repo := NewRepository()
	svc := NewService(repo, pool, zerolog.Nop())
	ctx := context.Background()

	patient := uuid.New()
	doctorID := uuid.New()
	patientPrincipal := middleware.Principal{UserID: patient, Roles: []middleware.Role{middleware.RolePatient}}
	doctorPrincipal := middleware.Principal{UserID: uuid.New(), DoctorID: doctorID, Roles: []middleware.Role{middleware.RoleDoctor}}

	share, err := svc.CreateShare(ctx, patientPrincipal, patient, doctorID, time.Hour)
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}

	if err := svc.Check(ctx, CheckOptions{Principal: doctorPrincipal, OwnerUserID: patient, Resource: ResourceDocument, ResourceID: uuid.New(), Action: ActionView}); err != nil {
		t.Fatalf("expected access to be granted before revocation: %v", err)
	}

	if err := svc.RevokeShare(ctx, patientPrincipal, share.ID); err != nil {
		t.Fatalf("RevokeShare: %v", err)
	}

	if err := svc.Check(ctx, CheckOptions{Principal: doctorPrincipal, OwnerUserID: patient, Resource: ResourceDocument, ResourceID: uuid.New(), Action: ActionView}); err == nil {
		t.Fatal("expected access to be denied after revocation")
	}
}

// TestConsumer_IdempotentOnEventID delivers the same event.ID twice and
// asserts the second delivery is a no-op: exactly one treating_relationships
// row, exactly one consumed_events row. This is the concrete test for
// AGENT-BRIEF's "consumers must be idempotent on envelope.ID" -- at-least-
// once delivery from JetStream means a redelivery WILL happen in production.
func TestConsumer_IdempotentOnEventID(t *testing.T) {
	pool := dbtest.NewPostgres(t)
	repo := NewRepository()
	consumer := NewConsumer(repo, pool, zerolog.Nop())
	ctx := context.Background()

	appointmentID, doctorID, patientID := uuid.New(), uuid.New(), uuid.New()
	env, err := events.NewEnvelope(events.SubjectConsultationStarted, "test", appointmentID.String(), events.ConsultationStarted{
		AppointmentID: appointmentID, DoctorID: doctorID, PatientID: patientID, StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}

	if err := consumer.Handle(ctx, env); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if err := consumer.Handle(ctx, env); err != nil {
		t.Fatalf("redelivery of the same event.ID must not error: %v", err)
	}

	var relCount, eventCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM treating_relationships WHERE appointment_id = $1`, appointmentID).Scan(&relCount); err != nil {
		t.Fatalf("count relationships: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM consumed_events WHERE event_id = $1`, env.ID).Scan(&eventCount); err != nil {
		t.Fatalf("count consumed events: %v", err)
	}
	if relCount != 1 {
		t.Errorf("treating_relationships rows = %d, want 1", relCount)
	}
	if eventCount != 1 {
		t.Errorf("consumed_events rows = %d, want 1", eventCount)
	}
}

// TestConsumer_StartedThenEnded_BuildsAFullRelationship exercises the
// realistic sequence: consultation.started arrives first (grants "during"
// access), then consultation.ended arrives later (grants "after" access
// too), without the second event clobbering the first event's timestamp.
func TestConsumer_StartedThenEnded_BuildsAFullRelationship(t *testing.T) {
	pool := dbtest.NewPostgres(t)
	repo := NewRepository()
	consumer := NewConsumer(repo, pool, zerolog.Nop())
	ctx := context.Background()

	appointmentID, doctorID, patientID := uuid.New(), uuid.New(), uuid.New()
	startedAt := time.Now().Add(-10 * time.Minute)
	endedAt := time.Now()

	startEnv, _ := events.NewEnvelope(events.SubjectConsultationStarted, "test", appointmentID.String(), events.ConsultationStarted{
		AppointmentID: appointmentID, DoctorID: doctorID, PatientID: patientID, StartedAt: startedAt,
	})
	if err := consumer.Handle(ctx, startEnv); err != nil {
		t.Fatalf("handle started: %v", err)
	}

	rel, ok, err := repo.TreatingRelationshipByAppointment(ctx, pool, appointmentID)
	if err != nil || !ok {
		t.Fatalf("relationship not found after started event: ok=%v err=%v", ok, err)
	}
	if rel.EndedAt != nil {
		t.Fatal("ended_at must still be nil after only consultation.started")
	}

	endEnv, _ := events.NewEnvelope(events.SubjectConsultationEnded, "test", appointmentID.String(), events.ConsultationEnded{
		AppointmentID: appointmentID, DoctorID: doctorID, PatientID: patientID, EndedAt: endedAt,
	})
	if err := consumer.Handle(ctx, endEnv); err != nil {
		t.Fatalf("handle ended: %v", err)
	}

	rel, ok, err = repo.TreatingRelationshipByAppointment(ctx, pool, appointmentID)
	if err != nil || !ok {
		t.Fatalf("relationship not found after ended event: ok=%v err=%v", ok, err)
	}
	// Postgres TIMESTAMPTZ has microsecond precision; time.Now() has
	// nanosecond precision, so a round trip through the database loses up to
	// 999ns. That is not a bug in UpsertTreatingRelationship's COALESCE
	// logic, so the comparison tolerates it rather than requiring exact
	// nanosecond equality.
	if rel.StartedAt == nil || rel.StartedAt.Sub(startedAt).Abs() > time.Millisecond {
		t.Errorf("started_at should be preserved from the first event, got %v, want ~%v", rel.StartedAt, startedAt)
	}
	if rel.EndedAt == nil {
		t.Fatal("ended_at must be set after consultation.ended")
	}
}

// --- F3: the implicit grant expires, against a real database -----------

// TestCheck_TreatingAccessExpires is SECURITY-REVIEW F3 driven end to end
// through the real SQL predicate, because that is where the bug was. The
// pure-function test (TestTreatingRelationship_GrantsAccessAt) pins the
// rule; this pins that the query implements the same rule, including its
// NULL handling, which no Go test can prove.
//
// Three doctors, three consultations, one patient. The only difference
// between them is when the consultation happened.
func TestCheck_TreatingAccessExpires(t *testing.T) {
	pool := dbtest.NewPostgres(t)
	repo := NewRepository()
	svc := NewService(repo, pool, zerolog.Nop())
	ctx := context.Background()

	patient := uuid.New()
	now := time.Now().UTC()

	seed := func(doctorID uuid.UUID, startedAgo, endedAgo time.Duration) {
		t.Helper()
		started := now.Add(-startedAgo)
		ended := now.Add(-endedAgo)
		if err := repo.UpsertTreatingRelationship(ctx, pool, uuid.New(), doctorID, patient, &started, &ended); err != nil {
			t.Fatalf("seed relationship: %v", err)
		}
	}

	yesterday := uuid.New()
	seed(yesterday, 25*time.Hour, 24*time.Hour)

	justInside := uuid.New()
	seed(justInside, TreatingAccessWindow, TreatingAccessWindow-time.Hour)

	justOutside := uuid.New()
	seed(justOutside, TreatingAccessWindow+2*time.Hour, TreatingAccessWindow+time.Hour)

	longAgo := uuid.New()
	seed(longAgo, 400*24*time.Hour, 400*24*time.Hour)

	tests := []struct {
		name     string
		doctorID uuid.UUID
		want     bool
	}{
		{"saw the patient yesterday", yesterday, true},
		{"an hour inside the window", justInside, true},
		{"an hour outside the window", justOutside, false},
		{"saw the patient over a year ago", longAgo, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.Check(ctx, CheckOptions{
				Principal:   middleware.Principal{UserID: uuid.New(), DoctorID: tc.doctorID, Roles: []middleware.Role{middleware.RoleDoctor}},
				OwnerUserID: patient, Resource: ResourceDocument, ResourceID: uuid.New(), Action: ActionDownload,
				IPAddress: "203.0.113.10", UserAgent: "test",
			})
			if granted := err == nil; granted != tc.want {
				t.Errorf("Check() granted = %v, want %v (err=%v)", granted, tc.want, err)
			}
		})
	}
}

// TestCheck_AdmitAndEndIsNotAPermanentGrant reproduces the F3 exploit as
// written, against a real database, and asserts it now expires.
//
// Dr X advertises a free two-minute consultation. Patient A books. X calls
// admit then end; A never appears. Under the old predicate X held
// permanent, unrevocable read+download over A's whole vault -- including
// documents A had not uploaded yet, which is the part with no analogue in
// paper medicine. The patient could not revoke it: RevokeShare only touches
// record_shares, and this grant is not a share.
//
// The uploaded-later case is asserted explicitly because it is the one a
// window could plausibly get wrong: the grant is keyed on the consultation,
// not on the document, so a document created after the window closed must be
// unreachable for the same reason an older one is.
func TestCheck_AdmitAndEndIsNotAPermanentGrant(t *testing.T) {
	pool := dbtest.NewPostgres(t)
	repo := NewRepository()
	svc := NewService(repo, pool, zerolog.Nop())
	ctx := context.Background()

	patient := uuid.New()
	drX := uuid.New()
	principal := middleware.Principal{UserID: uuid.New(), DoctorID: drX, Roles: []middleware.Role{middleware.RoleDoctor}}

	// The two-minute consultation, as it looked the day it happened.
	started := time.Now().UTC().Add(-2 * time.Minute)
	ended := started.Add(2 * time.Minute)
	if err := repo.UpsertTreatingRelationship(ctx, pool, uuid.New(), drX, patient, &started, &ended); err != nil {
		t.Fatalf("seed: %v", err)
	}

	check := func() error {
		return svc.Check(ctx, CheckOptions{
			Principal: principal, OwnerUserID: patient, Resource: ResourceDocument,
			ResourceID: uuid.New(), Action: ActionDownload, IPAddress: "203.0.113.11", UserAgent: "test",
		})
	}

	if err := check(); err != nil {
		t.Fatalf("the doctor must have access immediately after the consultation: %v", err)
	}

	// Wind the same row back past the window. Rewriting the timestamps is
	// how the passage of time is simulated; nothing else about the row, the
	// patient or the documents changes.
	old := time.Now().UTC().Add(-TreatingAccessWindow - 24*time.Hour)
	if _, err := pool.Exec(ctx,
		`UPDATE treating_relationships SET started_at = $1, ended_at = $2 WHERE doctor_id = $3`,
		old, old.Add(2*time.Minute), drX); err != nil {
		t.Fatalf("age the relationship: %v", err)
	}

	if err := check(); err == nil {
		t.Fatal("a consultation older than the access window still grants read+download over the patient's vault: F3 is open")
	}

	// A document the patient uploads today is not reachable either -- the
	// grant is keyed on the consultation, and the consultation is over.
	if err := svc.Check(ctx, CheckOptions{
		Principal: principal, OwnerUserID: patient, Resource: ResourceDocument,
		ResourceID: uuid.New(), Action: ActionView, IPAddress: "203.0.113.11", UserAgent: "test",
	}); err == nil {
		t.Fatal("a lapsed consultation still reaches documents uploaded after it ended")
	}
}

// TestCheck_AdmittedButNeverEndedIsAlsoBounded closes the obvious way around
// a window keyed on ended_at: never end the consultation. If the bound only
// applied to rows with an ended_at, "admit and walk away" would be a
// permanent grant again -- the same exploit with one fewer API call.
func TestCheck_AdmittedButNeverEndedIsAlsoBounded(t *testing.T) {
	pool := dbtest.NewPostgres(t)
	repo := NewRepository()
	svc := NewService(repo, pool, zerolog.Nop())
	ctx := context.Background()

	patient := uuid.New()
	drX := uuid.New()
	principal := middleware.Principal{UserID: uuid.New(), DoctorID: drX, Roles: []middleware.Role{middleware.RoleDoctor}}

	// consultation.started only. This is also what a lost or long-delayed
	// consultation.ended looks like, which is why the in-progress case must
	// still grant access for a sensible period rather than not at all.
	started := time.Now().UTC().Add(-10 * time.Minute)
	if err := repo.UpsertTreatingRelationship(ctx, pool, uuid.New(), drX, patient, &started, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := svc.Check(ctx, CheckOptions{
		Principal: principal, OwnerUserID: patient, Resource: ResourceDocument,
		ResourceID: uuid.New(), Action: ActionView, IPAddress: "203.0.113.12", UserAgent: "test",
	}); err != nil {
		t.Fatalf("a doctor mid-consultation must be able to read the record they are being asked to look at: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE treating_relationships SET started_at = $1 WHERE doctor_id = $2`,
		time.Now().UTC().Add(-TreatingAccessWindow-24*time.Hour), drX); err != nil {
		t.Fatalf("age the relationship: %v", err)
	}

	if err := svc.Check(ctx, CheckOptions{
		Principal: principal, OwnerUserID: patient, Resource: ResourceDocument,
		ResourceID: uuid.New(), Action: ActionView, IPAddress: "203.0.113.12", UserAgent: "test",
	}); err == nil {
		t.Fatal("a consultation admitted a month ago and never ended still grants access: the window can be bypassed by not ending the call")
	}
}

// --- F4: no administrator role reads a patient's vault -----------------

// TestNoAdminRoleReadsAPatientVault is SECURITY-REVIEW F4 through the real
// Check path, including the audit trail the refusal has to leave.
//
// All five roles are exercised, not just "admin", because the grant was
// p.HasAnyRole(middleware.AdminRoles...) -- finance and support had exactly
// the same reach as super_admin over every patient document and every
// prescription on the platform, from any IP, on a route the gateway serves
// as auth: "authenticated" with no allowlist.
func TestNoAdminRoleReadsAPatientVault(t *testing.T) {
	pool := dbtest.NewPostgres(t)
	repo := NewRepository()
	svc := NewService(repo, pool, zerolog.Nop())
	ctx := context.Background()

	patient := uuid.New()
	resourceID := uuid.New()

	for _, role := range middleware.AdminRoles {
		for _, res := range []ResourceType{ResourceDocument, ResourcePrescription, ResourceClinicalNote} {
			t.Run(string(role)+"/"+string(res), func(t *testing.T) {
				err := svc.Check(ctx, CheckOptions{
					Principal:   middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{role}},
					OwnerUserID: patient, Resource: res, ResourceID: resourceID, Action: ActionDownload,
					IPAddress: "203.0.113.13", UserAgent: "test",
				})
				if err == nil {
					t.Fatalf("role %s was granted %s content: an administrator must never read a patient's clinical record", role, res)
				}
			})
		}
	}

	// The refusals must be visible to a compliance officer as refusals of
	// administrators, not buried in a generic denial reason. This query is
	// the one a §15 audit actually runs.
	var denials int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM document_access_log WHERE owner_user_id = $1 AND granted = FALSE AND reason = $2`,
		patient, string(ReasonDeniedAdminClinical)).Scan(&denials); err != nil {
		t.Fatalf("count denials: %v", err)
	}
	if want := len(middleware.AdminRoles) * 3; denials != want {
		t.Errorf("document_access_log holds %d admin refusals, want %d -- every refusal must be greppable", denials, want)
	}

	// And there must be no granted admin row over this patient at all.
	var grants int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM document_access_log WHERE owner_user_id = $1 AND granted = TRUE AND reason = $2`,
		patient, string(ReasonAdmin)).Scan(&grants); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if grants != 0 {
		t.Errorf("document_access_log holds %d granted admin reads over a patient vault, want 0", grants)
	}
}

// TestAdminStillReadsCredentialDocuments keeps the doctor-verification queue
// alive, against the real database and the real CHECK constraint that
// migration 000006 widened. Refusing administrators a patient's record must
// not have refused them the one thing they legitimately open: the SLMC
// certificate a doctor uploaded to prove their registration.
func TestAdminStillReadsCredentialDocuments(t *testing.T) {
	pool := dbtest.NewPostgres(t)
	svc := NewService(NewRepository(), pool, zerolog.Nop())
	ctx := context.Background()

	doctorUserID := uuid.New() // the doctor under review owns their own paperwork

	for _, role := range middleware.AdminRoles {
		t.Run(string(role), func(t *testing.T) {
			if err := svc.Check(ctx, CheckOptions{
				Principal:   middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{role}},
				OwnerUserID: doctorUserID, Resource: ResourceCredentialDocument, ResourceID: uuid.New(),
				Action: ActionDownload, IPAddress: "203.0.113.14", UserAgent: "test",
			}); err != nil {
				t.Fatalf("role %s cannot read a credential document: the doctor-verification queue is broken: %v", role, err)
			}
		})
	}
}
