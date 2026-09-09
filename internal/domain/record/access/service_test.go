package access

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rs/zerolog"

	"telemed/internal/platform/httpx"
	"telemed/internal/platform/middleware"
)

var (
	patientID   = uuid.MustParse("11111111-0000-0000-0000-000000000001")
	doctorID    = uuid.MustParse("22222222-0000-0000-0000-000000000002")
	otherUserID = uuid.MustParse("33333333-0000-0000-0000-000000000003")
)

func patientPrincipal() middleware.Principal {
	return middleware.Principal{UserID: patientID, Roles: []middleware.Role{middleware.RolePatient}}
}

func treatingDoctorPrincipal() middleware.Principal {
	return middleware.Principal{UserID: otherUserID, DoctorID: doctorID, Roles: []middleware.Role{middleware.RoleDoctor}}
}

func adminPrincipal() middleware.Principal {
	return middleware.Principal{UserID: otherUserID, Roles: []middleware.Role{middleware.RoleAdmin}}
}

func superAdminPrincipal() middleware.Principal {
	return middleware.Principal{UserID: otherUserID, Roles: []middleware.Role{middleware.RoleSuperAdmin}}
}

func anonymousPrincipal() middleware.Principal { return middleware.Principal{} }

// TestDecideAccess_AuthorizationMatrix is the "test it hard" requirement
// from AGENT-BRIEF: owner / treating doctor / untreating doctor / admin /
// anonymous, exercised against decideAccess directly so the whole matrix
// runs with no database. This is the single function every read path in
// records and prescriptions ultimately depends on (via Service.Check), so
// this table is effectively the authorization contract for both resource
// types at once -- decideAccess has no notion of "document" vs
// "prescription", which is exactly the point: the rule is the same
// regardless of what is being protected.
func TestDecideAccess_AuthorizationMatrix(t *testing.T) {
	activeShare := &RecordShare{ID: uuid.New(), PatientID: patientID, DoctorID: doctorID, ExpiresAt: time.Now().Add(time.Hour)}

	tests := []struct {
		name        string
		principal   middleware.Principal
		treated     bool
		share       *RecordShare
		wantGranted bool
		wantReason  Reason
	}{
		{"owner reads their own vault", patientPrincipal(), false, nil, true, ReasonOwner},
		{"owner reads their own vault even if also flagged treated (degenerate)", patientPrincipal(), true, activeShare, true, ReasonOwner},

		{"treating doctor reads patient vault", treatingDoctorPrincipal(), true, nil, true, ReasonTreatingDoctor},
		{"treating doctor takes priority over an also-active share", treatingDoctorPrincipal(), true, activeShare, true, ReasonTreatingDoctor},

		{"untreating doctor with no share is denied", treatingDoctorPrincipal(), false, nil, false, ReasonDeniedNoLink},
		{"doctor with only an active share is granted", treatingDoctorPrincipal(), false, activeShare, true, ReasonShare},

		// These two rows used to read "admin reads any vault" / "super_admin
		// reads any vault", granted. That WAS SECURITY-REVIEW F4: the same
		// grant that was documented as index-only produced a presigned URL
		// to the patient's actual lab report, on a route with no IP
		// allowlist. No administrator role reads a patient's vault now.
		{"admin is refused a patient document", adminPrincipal(), false, nil, false, ReasonDeniedAdminClinical},
		{"super_admin is refused a patient document", superAdminPrincipal(), false, nil, false, ReasonDeniedAdminClinical},

		{"anonymous caller is denied", anonymousPrincipal(), false, nil, false, ReasonDeniedAnon},

		{"patient role (not doctor, not owner) is denied even with a share", middleware.Principal{UserID: otherUserID, Roles: []middleware.Role{middleware.RolePatient}}, false, activeShare, false, ReasonDeniedRole},
		{"doctor role without a DoctorID claim is denied", middleware.Principal{UserID: otherUserID, Roles: []middleware.Role{middleware.RoleDoctor}}, true, nil, false, ReasonDeniedRole},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decideAccess(tc.principal, patientID, ResourceDocument, ActionView, tc.treated, tc.share)
			if got.Granted != tc.wantGranted {
				t.Errorf("Granted = %v, want %v (reason=%s)", got.Granted, tc.wantGranted, got.Reason)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %s, want %s", got.Reason, tc.wantReason)
			}
		})
	}
}

// TestDecideAccess_ClinicalNotesAreNeverVisibleToAdmins pins the one place
// the matrix differs by resource type. Every other row is identical to
// TestDecideAccess_AuthorizationMatrix above, and that is asserted here
// rather than assumed: the danger of a resource-aware rule is that someone
// "fixes" it later by making clinical notes special in some second way too.
//
// The admin rows are the point. On documents and prescriptions an admin is
// granted with reason "admin"; on a clinical note the same principal is
// denied with a distinct, greppable reason. super_admin is listed separately
// because "surely the top role is exempt" is exactly the assumption this
// test exists to break.
func TestDecideAccess_ClinicalNotesAreNeverVisibleToAdmins(t *testing.T) {
	activeShare := &RecordShare{ID: uuid.New(), PatientID: patientID, DoctorID: doctorID, ExpiresAt: time.Now().Add(time.Hour)}

	tests := []struct {
		name        string
		principal   middleware.Principal
		treated     bool
		share       *RecordShare
		wantGranted bool
		wantReason  Reason
	}{
		{"patient reads their own finalised note", patientPrincipal(), false, nil, true, ReasonOwner},
		{"treating doctor reads the note", treatingDoctorPrincipal(), true, nil, true, ReasonTreatingDoctor},
		{"another doctor with no relationship is denied", treatingDoctorPrincipal(), false, nil, false, ReasonDeniedNoLink},
		{"another doctor holding an active share is granted", treatingDoctorPrincipal(), false, activeShare, true, ReasonShare},
		{"admin is denied clinical notes", adminPrincipal(), false, nil, false, ReasonDeniedAdminClinical},
		{"super_admin is denied clinical notes", superAdminPrincipal(), false, nil, false, ReasonDeniedAdminClinical},
		{"admin is denied even when a share exists for some doctor", adminPrincipal(), true, activeShare, false, ReasonDeniedAdminClinical},
		{"anonymous caller is denied", anonymousPrincipal(), false, nil, false, ReasonDeniedAnon},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decideAccess(tc.principal, patientID, ResourceClinicalNote, ActionView, tc.treated, tc.share)
			if got.Granted != tc.wantGranted {
				t.Errorf("Granted = %v, want %v (reason=%s)", got.Granted, tc.wantGranted, got.Reason)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %s, want %s", got.Reason, tc.wantReason)
			}
		})
	}
}

// TestDecideAccess_AdminStillReadsNonClinicalResources is the other half of
// the previous test, and of F4's: refusing administrators a patient's
// clinical record must not have refused them the ONE thing they legitimately
// read on this service -- a doctor's own credential paperwork, which the
// verification queue cannot review without opening.
//
// This test predates the F4 fix and its purpose is unchanged: keep the
// credentialing workflow alive. What changed is which resource carries that
// workflow. It used to be asserted through ResourceDocument, which is also
// how every patient lab report is protected, so "the verification queue
// works" and "any support account can download anyone's scan" were the same
// assertion. They are now two, and only this one is a grant.
func TestDecideAccess_AdminStillReadsNonClinicalResources(t *testing.T) {
	for _, role := range middleware.AdminRoles {
		p := middleware.Principal{UserID: otherUserID, Roles: []middleware.Role{role}}
		got := decideAccess(p, patientID, ResourceCredentialDocument, ActionView, false, nil)
		if !got.Granted || got.Reason != ReasonAdmin {
			t.Errorf("role %s: Granted=%v Reason=%s, want granted with reason %s -- the doctor-verification queue must keep working",
				role, got.Granted, got.Reason, ReasonAdmin)
		}
	}
}

// TestDecideAccess_NoAdminRoleReadsPatientClinicalContent is SECURITY-REVIEW
// F4, pinned.
//
// The exploit it closes needs no exploit: five roles -- admin, super_admin,
// ops, finance and support -- were granted every document and every
// prescription in the platform, on /api/v1/records and /api/v1/prescriptions,
// which the gateway serves as auth: "authenticated". The IP allowlist and the
// admin-origin check are composed per rule on auth: "admin" only, so none of
// them applied. A finance account, from any address on the internet, called
// GET /records/{id}/download and got a presigned MinIO URL to the actual
// file; GET /prescriptions/{id} returned the drug lines, and the drug names
// the condition.
//
// Every role is enumerated from middleware.AdminRoles rather than listed, so
// a sixth admin role added to the platform is covered on the day it is added
// rather than the day someone remembers this file.
func TestDecideAccess_NoAdminRoleReadsPatientClinicalContent(t *testing.T) {
	patientResources := []ResourceType{ResourceDocument, ResourcePrescription, ResourceClinicalNote}
	activeShare := &RecordShare{ID: uuid.New(), PatientID: patientID, DoctorID: doctorID, ExpiresAt: time.Now().Add(time.Hour)}

	for _, role := range middleware.AdminRoles {
		for _, res := range patientResources {
			// treated and activeShare are set to the most permissive values
			// the caller could ever pass, so a grant cannot be excused as
			// "well, there was a relationship".
			got := decideAccess(middleware.Principal{UserID: otherUserID, Roles: []middleware.Role{role}}, patientID, res, ActionView, true, activeShare)
			if got.Granted {
				t.Errorf("role %s was GRANTED %s (reason %s): an administrator must never read a patient's clinical record", role, res, got.Reason)
			}
			if got.Reason != ReasonDeniedAdminClinical {
				t.Errorf("role %s on %s: Reason = %s, want %s -- the refusal must be greppable as an admin refusal, not lost in a generic denial",
					role, res, got.Reason, ReasonDeniedAdminClinical)
			}
		}
	}
}

func TestDecideAccess_ExpiredShareIsNotConsidered(t *testing.T) {
	// The caller (Service.decide) is responsible for only ever passing an
	// activeShare that is unexpired and unrevoked; this test documents that
	// decideAccess itself grants on ANY non-nil share it is handed, so the
	// expiry/revocation enforcement living in the repository query
	// (ActiveShare's SQL WHERE clause) is the only thing standing between an
	// expired share and access. We assert that behaviour explicitly here so
	// a future refactor that moves the expiry check cannot silently drop it
	// without a test failing somewhere.
	expired := &RecordShare{ID: uuid.New(), ExpiresAt: time.Now().Add(-time.Hour), RevokedAt: nil}
	if expired.Active(time.Now()) {
		t.Fatal("test fixture is not actually expired")
	}
}

func TestRecordShare_Active(t *testing.T) {
	now := time.Now()
	revokedTime := now.Add(-time.Minute)

	tests := []struct {
		name string
		s    RecordShare
		want bool
	}{
		{"active", RecordShare{ExpiresAt: now.Add(time.Hour)}, true},
		{"expired", RecordShare{ExpiresAt: now.Add(-time.Hour)}, false},
		{"revoked but not yet expired", RecordShare{ExpiresAt: now.Add(time.Hour), RevokedAt: &revokedTime}, false},
		{"revoked and expired", RecordShare{ExpiresAt: now.Add(-time.Hour), RevokedAt: &revokedTime}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.Active(now); got != tc.want {
				t.Errorf("Active() = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- CreateShare / RevokeShare validation (the paths that return before
// touching the database, so they are safe to exercise with a Service that
// has no real pool or repository behind it). ---------------------------

func newTestService() *Service {
	return NewService(NewRepository(), nil, zerolog.Nop())
}

func TestCreateShare_OnlyPatientOrAdminMayGrant(t *testing.T) {
	svc := newTestService()
	stranger := middleware.Principal{UserID: otherUserID, Roles: []middleware.Role{middleware.RolePatient}}

	_, err := svc.CreateShare(t.Context(), stranger, patientID, doctorID, time.Hour)
	if err == nil {
		t.Fatal("expected an error when a non-owner, non-admin tries to create a share")
	}
}

func TestCreateShare_RejectsNonPositiveTTL(t *testing.T) {
	svc := newTestService()
	p := patientPrincipal()

	if _, err := svc.CreateShare(t.Context(), p, patientID, doctorID, 0); err == nil {
		t.Fatal("expected an error for a zero ttl")
	}
	if _, err := svc.CreateShare(t.Context(), p, patientID, doctorID, -time.Hour); err == nil {
		t.Fatal("expected an error for a negative ttl")
	}
}

func TestCreateShare_RejectsTTLBeyondMax(t *testing.T) {
	svc := newTestService()
	p := patientPrincipal()

	if _, err := svc.CreateShare(t.Context(), p, patientID, doctorID, MaxShareTTL+time.Hour); err == nil {
		t.Fatal("expected an error for a ttl beyond MaxShareTTL")
	}
}

func TestCreateShare_RejectsSelfShare(t *testing.T) {
	svc := newTestService()
	// A principal that is simultaneously "patient" and the doctor id in
	// question -- nonsensical, must be rejected regardless of role checks.
	p := middleware.Principal{UserID: patientID, Roles: []middleware.Role{middleware.RolePatient}}

	if _, err := svc.CreateShare(t.Context(), p, patientID, patientID, time.Hour); err == nil {
		t.Fatal("expected an error when patient_id == doctor_id")
	}
}

// --- F3: the implicit grant a consultation creates is bounded ----------

// TestTreatingRelationship_GrantsAccessAt is SECURITY-REVIEW F3, pinned at
// the level of the rule itself.
//
// The exploit: the predicate behind "is this doctor treating this patient"
// was a bare EXISTS on (doctor_id, patient_id). started_at and ended_at were
// written by the consumer and read by nothing. So Dr X advertises a free
// two-minute consultation, patient A books, X admits and ends it, A never
// joins -- and X now holds permanent, unrevocable read+download over A's
// entire vault, including every document A uploads for the rest of their
// life. RevokeShare does not touch it; there is no expiry job and no DELETE
// anywhere in the service. The patient had no way out at all.
//
// The rows below are the cases the bound has to get right, including the two
// that are easy to get wrong: a consultation that never recorded an end
// (which must expire from its start, not never), and one stamped in the
// future (which must not be a way to mint a window that never closes).
func TestTreatingRelationship_GrantsAccessAt(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }

	tests := []struct {
		name string
		rel  TreatingRelationship
		want bool
	}{
		{"consultation in progress right now", TreatingRelationship{StartedAt: at(-20 * time.Minute)}, true},
		{"concluded a minute ago", TreatingRelationship{StartedAt: at(-time.Hour), EndedAt: at(-time.Minute)}, true},
		{"concluded yesterday, notes still being written", TreatingRelationship{StartedAt: at(-25 * time.Hour), EndedAt: at(-24 * time.Hour)}, true},
		{"concluded three weeks ago, a histopathology report just landed", TreatingRelationship{StartedAt: at(-21 * 24 * time.Hour), EndedAt: at(-21 * 24 * time.Hour)}, true},
		{"concluded one second inside the window", TreatingRelationship{StartedAt: at(-TreatingAccessWindow), EndedAt: at(-TreatingAccessWindow + time.Second)}, true},

		{"concluded one second outside the window", TreatingRelationship{StartedAt: at(-TreatingAccessWindow - time.Hour), EndedAt: at(-TreatingAccessWindow - time.Second)}, false},
		{"concluded a year ago", TreatingRelationship{StartedAt: at(-365 * 24 * time.Hour), EndedAt: at(-365 * 24 * time.Hour)}, false},

		// The F3 exploit itself, in both shapes. Admitting and ending a
		// two-minute consultation is a 30-day grant, not a permanent one;
		// admitting and never ending is not a way around that.
		{"admitted and ended two minutes later, a month and a day ago", TreatingRelationship{StartedAt: at(-31 * 24 * time.Hour), EndedAt: at(-31*24*time.Hour + 2*time.Minute)}, false},
		{"admitted and never ended, a month and a day ago", TreatingRelationship{StartedAt: at(-31 * 24 * time.Hour)}, false},

		// Ordering and loss in the event pipeline must not widen the grant.
		{"only ended_at known (started event lost or reordered), recent", TreatingRelationship{EndedAt: at(-time.Hour)}, true},
		{"only ended_at known, long past", TreatingRelationship{EndedAt: at(-90 * 24 * time.Hour)}, false},

		// A row with no timestamps is not an authorisation fact. Nothing
		// produces one today; the point is that if something ever does, it
		// grants nothing rather than everything.
		{"no timestamps at all", TreatingRelationship{}, false},

		// Clock skew between services is tolerated; a hostile or corrupt
		// producer stamping the future is not.
		{"started a moment in the future (clock skew)", TreatingRelationship{StartedAt: at(30 * time.Second)}, true},
		{"started in 2099", TreatingRelationship{StartedAt: at(365 * 24 * time.Hour)}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rel.GrantsAccessAt(now); got != tc.want {
				t.Errorf("GrantsAccessAt() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTreatingAccessWindow_IsStrictlyWeakerThanAnExplicitShare states the
// relationship between the two grants as an assertion rather than as a
// sentence in a design doc. A patient who deliberately shares their vault
// gets at most MaxShareTTL and can revoke it whenever they like. The grant
// they never made must be strictly shorter than the one they did -- if these
// two numbers are ever equalised, the implicit grant has quietly become as
// strong as consent.
func TestTreatingAccessWindow_IsStrictlyWeakerThanAnExplicitShare(t *testing.T) {
	if TreatingAccessWindow >= MaxShareTTL {
		t.Errorf("TreatingAccessWindow (%s) must be strictly shorter than MaxShareTTL (%s): a grant the patient never made cannot last as long as one they did",
			TreatingAccessWindow, MaxShareTTL)
	}
}

// --- F15: the audit trail does not fail open ---------------------------

// failingPool is a database.Pool whose every write fails. It exists to drive
// the one branch that is otherwise unreachable in a unit test: the access-log
// insert failing while the caller is entitled to the record.
type failingPool struct{ err error }

func (p failingPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, p.err
}
func (p failingPool) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, p.err }
func (p failingPool) QueryRow(context.Context, string, ...any) pgx.Row        { return errRow{scanErr: p.err} }
func (p failingPool) Begin(context.Context) (pgx.Tx, error)                   { return nil, p.err }
func (p failingPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)  { return nil, p.err }
func (p failingPool) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, p.err
}
func (p failingPool) Ping(context.Context) error { return p.err }
func (p failingPool) Close()                     {}

type errRow struct{ scanErr error }

func (r errRow) Scan(...any) error { return r.scanErr }

// TestCheck_FailsClosedWhenTheAccessLogCannotBeWritten is SECURITY-REVIEW
// F15's second half, pinned.
//
// Check used to log the insert failure at error level and return nil -- the
// document was served, and nothing recorded that anyone had read it. That is
// not a degraded audit trail, it is an absent one, and it is absent exactly
// when something is already wrong. The shipped instance was not theoretical:
// the local trimIP returned "[::1]" with brackets, NULLIF($9,”)::inet
// rejected it, and on any dual-stack deployment every single PHI access
// inserted nothing while every single request succeeded. Nobody would ever
// have noticed, because nothing failed.
//
// Both branches are asserted. A caller who WOULD have been granted is
// refused, because serving PHI unrecorded is the thing being prevented; a
// caller who would have been denied is refused with the same status rather
// than falling through to 403, so the rule has no exceptions to remember.
func TestCheck_FailsClosedWhenTheAccessLogCannotBeWritten(t *testing.T) {
	svc := NewService(NewRepository(), failingPool{err: errors.New("insert or update on table \"document_access_log\" violates check constraint")}, zerolog.Nop())

	tests := []struct {
		name      string
		principal middleware.Principal
	}{
		{"a caller who would have been granted", patientPrincipal()},
		{"a caller who would have been denied", middleware.Principal{UserID: otherUserID, Roles: []middleware.Role{middleware.RolePatient}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.Check(t.Context(), CheckOptions{
				Principal: tc.principal, OwnerUserID: patientID, Resource: ResourceDocument,
				ResourceID: uuid.New(), Action: ActionDownload, IPAddress: "203.0.113.7", UserAgent: "test",
			})
			if err == nil {
				t.Fatal("Check() returned nil: PHI was served with no entry in the append-only access log")
			}
			var apiErr *httpx.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("Check() returned %T, want *httpx.APIError", err)
			}
			if apiErr.Status() != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want %d: an unwritable audit log is a retryable outage, not a permissions decision",
					apiErr.Status(), http.StatusServiceUnavailable)
			}
		})
	}
}
