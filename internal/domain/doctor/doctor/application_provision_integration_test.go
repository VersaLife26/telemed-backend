//go:build integration

package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"telemed/internal/platform/events"
)

// fakeProvisioner stands in for user-service. It records what doctor-service
// asked it to create so the test can assert on the hash that crosses the
// mesh, and can fail once to exercise the approve-again retry.
type fakeProvisioner struct {
	calls []DoctorAccount

	userID uuid.UUID
	// passwordApplied is what user-service reports back: false means the
	// account already existed and kept its own password.
	passwordApplied bool

	failed  bool
	failOn  int   // 1 = fail the first call, 0 = never fail
	failErr error // defaults to a transient error
}

func (f *fakeProvisioner) ProvisionDoctor(_ context.Context, in DoctorAccount) (ProvisionResult, error) {
	f.calls = append(f.calls, in)
	if f.failOn == len(f.calls) {
		f.failed = true
		if f.failErr != nil {
			return ProvisionResult{}, f.failErr
		}
		return ProvisionResult{}, errors.New("user-service unreachable")
	}
	return ProvisionResult{UserID: f.userID, PasswordApplied: f.passwordApplied}, nil
}

// approvalPayload reads back the doctor.application_approved the approve path
// queued. The instruction in the applicant's email is derived entirely from
// this payload, so it is the thing worth asserting on.
func approvalPayload(t *testing.T, pool *pgxpool.Pool, applicationID uuid.UUID) events.DoctorApplicationApproved {
	t.Helper()
	var raw []byte
	err := pool.QueryRow(context.Background(),
		`SELECT payload FROM outbox_events WHERE subject = $1 AND aggregate_id = $2`,
		string(events.SubjectDoctorApplicationApproved), applicationID.String(),
	).Scan(&raw)
	if err != nil {
		t.Fatalf("read approval payload: %v", err)
	}
	// outbox_events.payload holds the whole envelope, not the bare payload --
	// unmarshalling the row straight into the event type silently yields a
	// zero value, which is a very convincing way to assert nothing at all.
	var env events.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode outbox envelope: %v", err)
	}
	var p events.DoctorApplicationApproved
	if err := env.Decode(&p); err != nil {
		t.Fatalf("decode approval payload: %v", err)
	}
	return p
}

const applyTestPassword = "correct-horse-battery"

func applyFixture(phone, email, slmc string) ApplyInput {
	return ApplyInput{
		Phone:               phone,
		Email:               email,
		FirstName:           "Amila",
		LastName:            "Perera",
		SLMCNumber:          slmc,
		Specialty:           "general_practice",
		Languages:           []Language{LanguageEN},
		ExperienceYears:     6,
		FeeCents:            250000,
		RequiredFeeCents:    200000,
		MedicalSchool:       "University of Colombo",
		QualificationsText:  "MBBS, MD",
		AvailabilityNotes:   "Weekdays 09:00-17:00",
		PracticingLocations: []string{"Nawaloka Hospital"},
		TermsAccepted:       true,
		Bank: &BankDetails{
			BankName:      "Commercial Bank",
			BranchName:    "Colombo 07",
			AccountNumber: "1234567890",
			AccountName:   "Amila Perera",
		},
		Password: applyTestPassword,
	}
}

// TestApproveApplication_ProvisionsTheLoginFromTheApplyPassword is the
// end-to-end claim of the apply-time password: an admin approving a public
// application is what creates the doctor's login, and the credential it is
// created with is the one the applicant chose on the form. Before this,
// approval only flipped a status and the applicant could not sign in at all
// until they completed phone OTP.
func TestApproveApplication_ProvisionsTheLoginFromTheApplyPassword(t *testing.T) {
	pool := setupPostgres(t)
	svc, repo := newTestService(t, pool)
	ctx := context.Background()
	admin := uuid.New()

	prov := &fakeProvisioner{userID: uuid.New(), passwordApplied: true}
	svc.WithAccountProvisioner(prov)

	app, err := svc.Apply(ctx, applyFixture("+94771234567", "Amila.Perera@example.lk", "SLMC7001"))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if app.Status != ApplicationPending {
		t.Fatalf("expected pending after apply, got %s", app.Status)
	}
	// The plaintext never reaches the database.
	if app.PasswordHash == applyTestPassword {
		t.Fatal("apply stored the password in plaintext")
	}

	decided, err := svc.VerifyApplication(ctx, app.ID, true, "", &admin)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}

	// Approval runs all the way to activation, not just to "approved".
	if decided.Status != ApplicationActivated {
		t.Fatalf("expected activated after approve, got %s", decided.Status)
	}

	if len(prov.calls) != 1 {
		t.Fatalf("expected exactly 1 provision call, got %d", len(prov.calls))
	}
	got := prov.calls[0]
	if got.Email != "amila.perera@example.lk" {
		t.Errorf("email = %q, want the normalised apply email", got.Email)
	}
	if got.Phone != "+94771234567" {
		t.Errorf("phone = %q", got.Phone)
	}
	if got.Name != "Amila Perera" {
		t.Errorf("name = %q", got.Name)
	}
	// The point of the whole change: the hash handed to user-service is the
	// one the applicant's password verifies against, so LoginEmail works.
	if err := bcrypt.CompareHashAndPassword([]byte(got.PasswordHash), []byte(applyTestPassword)); err != nil {
		t.Errorf("provisioned hash does not match the apply password: %v", err)
	}

	// The doctors row exists and is tied to the user-service account.
	d, err := repo.GetByUserID(ctx, prov.userID)
	if err != nil {
		t.Fatalf("doctors row for provisioned user: %v", err)
	}
	if d.ID != app.ID {
		t.Errorf("doctor id = %s, want the application id %s", d.ID, app.ID)
	}
	if d.VerificationStatus != StatusApproved {
		t.Errorf("verification status = %s, want approved", d.VerificationStatus)
	}

	// The hash is not kept in two places once the account owns it.
	stored, err := repo.GetApplication(ctx, app.ID)
	if err != nil {
		t.Fatalf("reload application: %v", err)
	}
	if stored.PasswordHash != "" {
		t.Error("password hash still on the application after activation")
	}
	if stored.ActivatedUserID == nil || *stored.ActivatedUserID != prov.userID {
		t.Errorf("activated_user_id = %v, want %s", stored.ActivatedUserID, prov.userID)
	}

	if n := countOutboxEvents(t, pool, string(events.SubjectDoctorApplicationApproved), app.ID.String()); n != 1 {
		t.Errorf("expected 1 doctor.application_approved outbox row, got %d", n)
	}
	if n := countOutboxEvents(t, pool, string(events.SubjectDoctorApproved), app.ID.String()); n != 1 {
		t.Errorf("expected 1 doctor.approved outbox row, got %d", n)
	}
	// The email must tell them to use the password they chose.
	notice := approvalPayload(t, pool, app.ID)
	if !notice.LoginReady || !notice.PasswordApplied {
		t.Errorf("approval notice = {login_ready:%v password_applied:%v}, want both true",
			notice.LoginReady, notice.PasswordApplied)
	}
}

// TestApproveApplication_ExistingAccountKeepsItsOwnPassword covers the
// applicant who was already a VersaLife patient on the same email. Their
// account is promoted, but user-service refuses to overwrite the password it
// already has -- overwriting would turn the public apply form into a password
// reset for any address someone cares to type. The approval email has to say
// so, which means the flag has to survive the trip into the outbox.
func TestApproveApplication_ExistingAccountKeepsItsOwnPassword(t *testing.T) {
	pool := setupPostgres(t)
	svc, _ := newTestService(t, pool)
	ctx := context.Background()
	admin := uuid.New()

	// passwordApplied:false is user-service saying "this account already had
	// a password and I left it alone".
	prov := &fakeProvisioner{userID: uuid.New(), passwordApplied: false}
	svc.WithAccountProvisioner(prov)

	app, err := svc.Apply(ctx, applyFixture("+94712223344", "already@example.lk", "SLMC7004"))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	decided, err := svc.VerifyApplication(ctx, app.ID, true, "", &admin)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if decided.Status != ApplicationActivated {
		t.Fatalf("status = %s, want activated", decided.Status)
	}

	notice := approvalPayload(t, pool, app.ID)
	if !notice.LoginReady {
		t.Error("login_ready = false, but an account was provisioned")
	}
	if notice.PasswordApplied {
		t.Error("password_applied = true; the email would tell them to use a password their account does not have")
	}
}

// TestApproveApplication_ConflictIsNotReportedAsRetryable is the second half
// of the same story: when the email and phone on an application belong to two
// different accounts, user-service refuses permanently. Reporting that as
// ErrAccountProvision would put "retry the approval" in front of an admin for
// something that can only ever fail the same way.
func TestApproveApplication_ConflictIsNotReportedAsRetryable(t *testing.T) {
	pool := setupPostgres(t)
	svc, repo := newTestService(t, pool)
	ctx := context.Background()
	admin := uuid.New()

	prov := &fakeProvisioner{
		userID:  uuid.New(),
		failOn:  1,
		failErr: ErrAccountConflict,
	}
	svc.WithAccountProvisioner(prov)

	app, err := svc.Apply(ctx, applyFixture("+94713334455", "clash@example.lk", "SLMC7005"))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	_, err = svc.VerifyApplication(ctx, app.ID, true, "", &admin)
	if !errors.Is(err, ErrAccountConflict) {
		t.Fatalf("got %v, want ErrAccountConflict", err)
	}
	if errors.Is(err, ErrAccountProvision) {
		t.Error("a permanent conflict must not also report as the retryable failure")
	}

	// The decision stands -- the credentials were reviewed and approved; only
	// the login could not be built. And no email went out promising a sign-in
	// that does not exist.
	stored, err := repo.GetApplication(ctx, app.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if stored.Status != ApplicationApproved {
		t.Errorf("status = %s, want approved", stored.Status)
	}
	if n := countOutboxEvents(t, pool, string(events.SubjectDoctorApplicationApproved), app.ID.String()); n != 0 {
		t.Errorf("expected no approval email for a failed provision, got %d", n)
	}
}

// TestApproveApplication_RetriesAfterAProvisionFailure covers the failure
// mode the split introduces: the decision is committed before user-service is
// called, so a user-service outage leaves an approved application with no
// login. Approving again must finish the job rather than be refused as a
// duplicate -- and the hash must survive the first failure, or the retry
// would silently produce an OTP-only account.
func TestApproveApplication_RetriesAfterAProvisionFailure(t *testing.T) {
	pool := setupPostgres(t)
	svc, repo := newTestService(t, pool)
	ctx := context.Background()
	admin := uuid.New()

	prov := &fakeProvisioner{userID: uuid.New(), failOn: 1}
	svc.WithAccountProvisioner(prov)

	app, err := svc.Apply(ctx, applyFixture("+94777654321", "nimal@example.lk", "SLMC7002"))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	if _, err := svc.VerifyApplication(ctx, app.ID, true, "", &admin); !errors.Is(err, ErrAccountProvision) {
		t.Fatalf("first approve: got %v, want ErrAccountProvision", err)
	}
	if !prov.failed {
		t.Fatal("provisioner was never called")
	}

	mid, err := repo.GetApplication(ctx, app.ID)
	if err != nil {
		t.Fatalf("reload after failure: %v", err)
	}
	if mid.Status != ApplicationApproved {
		t.Fatalf("status after failed provision = %s, want approved (retryable)", mid.Status)
	}
	if mid.PasswordHash == "" {
		t.Fatal("password hash cleared by a failed provision; the retry could not set a password")
	}
	// No "you can sign in now" email for a login that does not exist yet.
	if n := countOutboxEvents(t, pool, string(events.SubjectDoctorApplicationApproved), app.ID.String()); n != 0 {
		t.Errorf("expected no approval email after a failed provision, got %d", n)
	}

	done, err := svc.VerifyApplication(ctx, app.ID, true, "", &admin)
	if err != nil {
		t.Fatalf("retry approve: %v", err)
	}
	if done.Status != ApplicationActivated {
		t.Fatalf("status after retry = %s, want activated", done.Status)
	}
	if len(prov.calls) != 2 {
		t.Fatalf("expected 2 provision calls, got %d", len(prov.calls))
	}
	if err := bcrypt.CompareHashAndPassword([]byte(prov.calls[1].PasswordHash), []byte(applyTestPassword)); err != nil {
		t.Errorf("retry provisioned a hash that does not match the apply password: %v", err)
	}
	if _, err := repo.GetByUserID(ctx, prov.userID); err != nil {
		t.Fatalf("doctors row after retry: %v", err)
	}

	// The applicant is emailed once, however many times provisioning ran.
	if n := countOutboxEvents(t, pool, string(events.SubjectDoctorApplicationApproved), app.ID.String()); n != 1 {
		t.Errorf("expected 1 doctor.application_approved outbox row, got %d", n)
	}
}

// TestApproveApplication_WithoutAProvisionerFallsBackToOTP pins the degraded
// path: a deployment with no USER_SERVICE_URL still approves, and the
// applicant reaches their account through phone OTP as before. Silently
// failing the approval instead would block admins on a config gap.
func TestApproveApplication_WithoutAProvisionerFallsBackToOTP(t *testing.T) {
	pool := setupPostgres(t)
	svc, repo := newTestService(t, pool)
	ctx := context.Background()
	admin := uuid.New()

	app, err := svc.Apply(ctx, applyFixture("+94770001122", "sunil@example.lk", "SLMC7003"))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	decided, err := svc.VerifyApplication(ctx, app.ID, true, "", &admin)
	if err != nil {
		t.Fatalf("approve without provisioner: %v", err)
	}
	if decided.Status != ApplicationApproved {
		t.Fatalf("status = %s, want approved (awaiting OTP)", decided.Status)
	}

	// The applicant is still told what to do -- with the OTP wording, since
	// there is no login to sign in to.
	notice := approvalPayload(t, pool, app.ID)
	if notice.LoginReady || notice.PasswordApplied {
		t.Errorf("approval notice = {login_ready:%v password_applied:%v}, want both false",
			notice.LoginReady, notice.PasswordApplied)
	}

	// Re-approving must not send a second copy of it.
	if _, err := svc.VerifyApplication(ctx, app.ID, true, "", &admin); err != nil {
		t.Fatalf("re-approve: %v", err)
	}
	if n := countOutboxEvents(t, pool, string(events.SubjectDoctorApplicationApproved), app.ID.String()); n != 1 {
		t.Errorf("expected 1 approval email after re-approving, got %d", n)
	}

	// The OTP path can still complete it, and the hash is still there to be
	// copied across when it does.
	stored, err := repo.GetApplication(ctx, app.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if stored.PasswordHash == "" {
		t.Error("password hash lost; an OTP activation would leave the doctor with no password")
	}

	userID := uuid.New()
	if _, err := svc.Attach(ctx, AttachInput{ApplicationID: app.ID, UserID: userID, Email: stored.Email}); err != nil {
		t.Fatalf("otp attach: %v", err)
	}
	if _, err := repo.GetByUserID(ctx, userID); err != nil {
		t.Fatalf("doctors row after otp attach: %v", err)
	}
}
