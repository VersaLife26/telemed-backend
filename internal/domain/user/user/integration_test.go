//go:build integration

// Integration tests exercise the parts of this service that a fake cannot
// meaningfully stand in for: real Postgres transactions, row locks
// (FOR UPDATE), and the optimistic-lock/unique-constraint behaviour the
// repository depends on Postgres itself to enforce.
//
// Redis is not containerised here: cache.Cache is a narrow interface and
// fakeCache (cache_fake_test.go) already reproduces the one behaviour these
// tests need from it -- Cache.Incr's fixed-window semantics -- so spinning
// up a second container would add Docker/disk pressure for no additional
// coverage. See README "Testing" for the full rationale.
package user

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/repopath"
)

// setupTestPool starts a disposable postgres:17-alpine container, applies
// this service's migrations, and returns a ready pool. The container and
// pool are torn down via t.Cleanup, so a test run leaves nothing behind on a
// machine where disk is already tight.
func setupTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:        "postgres:17-alpine",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     "telemed",
			"POSTGRES_PASSWORD": "telemed",
			"POSTGRES_DB":       "telemed_user_test",
		},
		// The official postgres image restarts once after initdb completes
		// (init on the temporary startup instance, then a real restart for
		// the durable one). Waiting only for the port to listen races that
		// restart -- the port accepts a connection during the *temporary*
		// instance, which then closes it, producing a flaky
		// "the database system is starting up" error on whichever caller
		// connects in that window. Waiting for the ready log line twice is
		// the documented fix.
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(90 * time.Second),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("terminate postgres container: %v", err)
		}
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	mappedPort, err := container.MappedPort(ctx, "5432")
	if err != nil {
		t.Fatalf("container port: %v", err)
	}
	dsn := fmt.Sprintf("postgres://telemed:telemed@%s:%s/telemed_user_test?sslmode=disable", host, mappedPort.Port())

	// Migrate into svc_user, not public: that is the shape production runs.
	dsn, err = database.EnsureSchema(ctx, dsn, "user")
	if err != nil {
		t.Fatalf("provision schema: %v", err)
	}

	applyAllMigrations(t, ctx, dsn)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping pool: %v", err)
	}
	return pool
}

// applyAllMigrations applies every .up.sql in migrations/ in filename order.
//
// It globs rather than naming the files, because the previous version listed
// 000001 and 000002 explicitly and a third migration was therefore invisible
// to every integration test on the day it was added -- including the tests
// written to prove that migration works. A schema constraint the suite never
// applies is a constraint the suite cannot check.
//
// Filename order is the correct order: migrations are numbered sequentially
// from 000001 by convention, and lexical sort on a zero-padded prefix is
// numeric sort.
func applyAllMigrations(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(repopath.Migrations(t, "user"), "*.up.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no migrations found: the suite would run against an empty schema and pass for the wrong reason")
	}
	sort.Strings(paths)

	for _, p := range paths {
		applyMigration(t, ctx, dsn, p)
	}
}

// applyMigration runs one .up.sql file over a plain connection using the
// simple query protocol, which -- unlike pgx's default extended protocol --
// supports a file containing several semicolon-separated statements in one
// round trip, exactly like psql would.
func applyMigration(t *testing.T, ctx context.Context, dsn, path string) {
	t.Helper()
	sqlBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration %s: %v", path, err)
	}

	connCfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	connCfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	// Belt-and-suspenders on top of the log-based wait strategy above: on a
	// machine shared with other agents, container scheduling itself can be
	// slow enough that the very first connection attempt still loses the
	// race. A handful of short retries costs nothing when the server is
	// already up (the loop exits on the first success) and makes the test
	// robust to exactly the kind of transient contention this environment
	// has.
	var conn *pgx.Conn
	for attempt := 1; attempt <= 10; attempt++ {
		conn, err = pgx.ConnectConfig(ctx, connCfg)
		if err == nil {
			break
		}
		time.Sleep(time.Duration(attempt) * 300 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("connect for migration %s after retries: %v", path, err)
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, string(sqlBytes)); err != nil {
		t.Fatalf("apply migration %s: %v", path, err)
	}
}

// captureSMS is a test-only SMSProvider that records every message sent,
// standing in for DevSMSProvider (which only ever prints to stdout) so a
// test can recover the OTP without parsing captured console output across
// goroutines.
type captureSMS struct {
	mu   sync.Mutex
	last string
}

func (c *captureSMS) Send(_ context.Context, _, body string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = body
	return "captured", nil
}

func (c *captureSMS) lastCode(t *testing.T) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	m := sixDigits.FindString(c.last)
	if m == "" {
		t.Fatalf("no 6-digit code found in captured SMS body: %q", c.last)
	}
	return m
}

// captureEmail is a test-only EmailSender. OTP delivery moved to email when
// this deployment lost its SMS rail, so this -- not captureSMS -- is where a
// test recovers the code from.
type captureEmail struct {
	mu      sync.Mutex
	last    string
	lastTo  string
	lastSub string
}

func (c *captureEmail) Send(_ context.Context, to, subject, body string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastTo, c.lastSub, c.last = to, subject, body
	return "captured", nil
}

func (c *captureEmail) lastCode(t *testing.T) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	m := sixDigits.FindString(c.last)
	if m == "" {
		t.Fatalf("no 6-digit code found in captured email body: %q", c.last)
	}
	return m
}

func (c *captureEmail) recipient() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastTo
}

var sixDigits = regexp.MustCompile(`\d{6}`)

func newTestService(t *testing.T, pool *pgxpool.Pool) (*Service, *captureEmail) {
	t.Helper()
	repo := NewRepository(pool)
	outbox := events.NewOutbox("telemed-user-service-test")
	sms := &captureSMS{}
	kc := NewDegradedKeycloakClient(zerolog.Nop())
	tokens := newTestIssuer(t)
	nic, err := NewNICHasher(testNICPepper)
	if err != nil {
		t.Fatalf("NewNICHasher: %v", err)
	}
	svc := NewService(repo, newFakeCache(), outbox, sms, kc, tokens, nic, zerolog.Nop())
	mail := &captureEmail{}
	svc.SetEmailSender(mail)
	return svc, mail
}

func mustCreateUser(t *testing.T, ctx context.Context, repo *Repository, phone string) *User {
	t.Helper()
	// An email address, because OTP delivery is by email: a phone identity is
	// resolved to the address on its account, so a test user without one
	// cannot be sent a code at all (ErrNoDeliveryAddress).
	email := "user" + strings.TrimPrefix(phone, "+") + "@example.test"
	u := &User{Phone: phone, Email: &email, Name: "Test User", Language: LanguageEnglish, Role: RolePatient, Status: StatusActive}
	if err := repo.CreateUser(ctx, repo.Pool(), u); err != nil {
		t.Fatalf("create user %s: %v", phone, err)
	}
	return u
}

// TestIntegration_RefreshRotation_ReuseDetectionRevokesFamily is the test
// AGENT-BRIEF calls for: "refresh rotation including the reuse-detection
// path". A stolen refresh token replayed after it has already been rotated
// must not just fail itself -- it must burn the entire session lineage,
// including the token that legitimately replaced it, which is what makes
// this the standard defence against refresh-token theft rather than a
// half-measure.
func TestIntegration_RefreshRotation_ReuseDetectionRevokesFamily(t *testing.T) {
	ctx := context.Background()
	pool := setupTestPool(t)
	svc, _ := newTestService(t, pool)

	u := mustCreateUser(t, ctx, svc.repo, "+94771111111")

	session1, err := svc.issueSession(ctx, *u, "device-A", uuid.New())
	if err != nil {
		t.Fatalf("issueSession: %v", err)
	}

	// Legitimate rotation: token 1 -> token 2.
	session2, err := svc.Refresh(ctx, session1.RefreshToken, "device-A")
	if err != nil {
		t.Fatalf("first refresh (legitimate rotation): %v", err)
	}
	if session2.RefreshToken == session1.RefreshToken {
		t.Fatal("rotation returned the same refresh token")
	}
	if session2.User.ID != u.ID {
		t.Fatalf("rotated session user = %v, want %v", session2.User.ID, u.ID)
	}

	// Replay attack: token 1 is presented again after it was already
	// rotated away. This must be detected as reuse, not merely rejected as
	// "already used".
	_, err = svc.Refresh(ctx, session1.RefreshToken, "device-A")
	if !errors.Is(err, ErrRefreshReused) {
		t.Fatalf("replaying token 1: got err=%v, want ErrRefreshReused", err)
	}

	// The legitimate, currently-active token from the same family must also
	// now be dead: reuse detection revokes the whole lineage, not just the
	// replayed token.
	_, err = svc.Refresh(ctx, session2.RefreshToken, "device-A")
	if !errors.Is(err, ErrRefreshReused) && !errors.Is(err, ErrRefreshInvalid) {
		t.Fatalf("token 2 (legitimate descendant of the compromised family) after reuse detection: got err=%v, want it revoked", err)
	}
}

// TestIntegration_RefreshRotation_UnknownTokenRejected ensures a token that
// was never issued (garbage, or from a different service instance's key
// material) is rejected as invalid rather than crashing or being silently
// accepted.
func TestIntegration_RefreshRotation_UnknownTokenRejected(t *testing.T) {
	ctx := context.Background()
	pool := setupTestPool(t)
	svc, _ := newTestService(t, pool)

	_, err := svc.Refresh(ctx, "this-was-never-issued", "device-X")
	if !errors.Is(err, ErrRefreshInvalid) {
		t.Fatalf("got err=%v, want ErrRefreshInvalid", err)
	}
}

// TestIntegration_FamilyMemberAuthorization is the test AGENT-BRIEF calls
// for: "family-member authorisation (user A must not read user B's
// family)". Booking for a child or elderly parent is a core Sri Lankan
// usage pattern, and a leak here would expose one patient's dependants
// (names, dates of birth) to another patient.
func TestIntegration_FamilyMemberAuthorization(t *testing.T) {
	ctx := context.Background()
	pool := setupTestPool(t)
	svc, _ := newTestService(t, pool)

	userA := mustCreateUser(t, ctx, svc.repo, "+94772222222")
	userB := mustCreateUser(t, ctx, svc.repo, "+94773333333")

	bChild, err := svc.AddFamilyMember(ctx, userB.ID, FamilyMemberInput{
		Name: "B's Child", DOB: time.Date(2018, 5, 1, 0, 0, 0, 0, time.UTC), Relation: RelationChild,
	})
	if err != nil {
		t.Fatalf("AddFamilyMember for user B: %v", err)
	}

	// A's own family list must not include B's dependant.
	aFamily, err := svc.ListFamilyMembers(ctx, userA.ID)
	if err != nil {
		t.Fatalf("ListFamilyMembers for user A: %v", err)
	}
	for _, m := range aFamily {
		if m.ID == bChild.ID {
			t.Fatal("user A's family list leaked user B's family member")
		}
	}

	// A must not be able to update B's family member.
	_, err = svc.UpdateFamilyMember(ctx, userA.ID, bChild.ID, FamilyMemberInput{
		Name: "Renamed by attacker", DOB: bChild.DOB, Relation: RelationChild,
	}, bChild.Version)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("user A updating user B's family member: got err=%v, want ErrForbidden", err)
	}

	// A must not be able to delete B's family member.
	err = svc.DeleteFamilyMember(ctx, userA.ID, bChild.ID)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("user A deleting user B's family member: got err=%v, want ErrForbidden", err)
	}

	// Positive control: B can still manage their own family member, proving
	// the block above is about ownership, not a blanket failure.
	updated, err := svc.UpdateFamilyMember(ctx, userB.ID, bChild.ID, FamilyMemberInput{
		Name: "B's Child (updated)", DOB: bChild.DOB, Relation: RelationChild,
	}, bChild.Version)
	if err != nil {
		t.Fatalf("user B updating their own family member: %v", err)
	}
	if updated.Name != "B's Child (updated)" {
		t.Fatalf("update did not apply: got name %q", updated.Name)
	}
	if err := svc.DeleteFamilyMember(ctx, userB.ID, bChild.ID); err != nil {
		t.Fatalf("user B deleting their own family member: %v", err)
	}
}

// TestIntegration_OTPRegistrationFlow_CreatesUserAndOutboxEvent exercises
// the full VerifyOTP path end to end against a real database: find-or-create
// on first verification, a user.registered row landing in the transactional
// outbox in the same transaction as the user row (so a crash between the two
// is impossible by construction -- see DECISIONS.md ADR-005), and a working
// session.
func TestIntegration_OTPRegistrationFlow_CreatesUserAndOutboxEvent(t *testing.T) {
	ctx := context.Background()
	pool := setupTestPool(t)
	svc, mail := newTestService(t, pool)

	// Registration is by EMAIL. A brand-new phone number has no account and
	// therefore no address to receive a code at, which is the deliberate
	// limit of email-only OTP -- asserted directly in
	// TestIntegration_OTPRegistrationByPhoneHasNowhereToSend below.
	const email = "newpatient@example.test"
	sendRes, err := svc.SendOTP(ctx, OTPIdentity{Email: email}, PurposeRegister, LanguageEnglish, "127.0.0.1")
	if err != nil {
		t.Fatalf("SendOTP: %v", err)
	}
	if sendRes.AttemptsRemaining != otpSendLimitPerPhone-1 {
		t.Errorf("AttemptsRemaining = %d, want %d", sendRes.AttemptsRemaining, otpSendLimitPerPhone-1)
	}
	if got := mail.recipient(); got != email {
		t.Errorf("code delivered to %q, want %q", got, email)
	}

	code := mail.lastCode(t)

	auth, err := svc.VerifyOTP(ctx, OTPIdentity{Email: email}, code, "device-1", PurposeRegister, "127.0.0.1")
	if err != nil {
		t.Fatalf("VerifyOTP: %v", err)
	}
	if auth.User.Email == nil || *auth.User.Email != email {
		t.Errorf("created user email = %v, want %q", auth.User.Email, email)
	}
	// Migration 000005 dropped NOT NULL on phone precisely so an email-only
	// account is representable.
	if auth.User.Phone != "" {
		t.Errorf("email registration set a phone = %q, want empty", auth.User.Phone)
	}
	if auth.AccessToken == "" || auth.RefreshToken == "" {
		t.Fatal("VerifyOTP did not return both tokens")
	}

	principal, err := svc.tokens.Verify(auth.AccessToken)
	if err != nil {
		t.Fatalf("issued access token does not verify: %v", err)
	}
	if principal.UserID != auth.User.ID {
		t.Errorf("access token subject = %v, want %v", principal.UserID, auth.User.ID)
	}

	// The outbox row must exist in the SAME transaction as the user row --
	// we can only observe the outcome here, but its presence at all (with no
	// separate publish step run) confirms Enqueue happened inside VerifyOTP's
	// database.InTx block rather than after it.
	var count int
	err = pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE subject = $1 AND aggregate_id = $2`,
		string(events.SubjectUserRegistered), auth.User.ID.String(),
	).Scan(&count)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	if count != 1 {
		t.Errorf("outbox_events rows for user.registered/%s = %d, want 1", auth.User.ID, count)
	}

	// The OTP is single-use: verifying again with the same (now consumed)
	// code must fail rather than silently succeeding a second time.
	if _, err := svc.VerifyOTP(ctx, OTPIdentity{Email: email}, code, "device-1", PurposeLogin, "127.0.0.1"); err == nil {
		t.Error("VerifyOTP succeeded twice with the same code")
	}
}

// TestIntegration_OTPVerify_WrongCodeThenLockout drives the verify-attempt
// cap against a real send/verify cycle: five wrong guesses are rejected
// individually, the sixth invalidates the code outright, and the correct
// code no longer works afterwards even though it was never actually
// guessed correctly by the attacker.
func TestIntegration_OTPVerify_WrongCodeThenLockout(t *testing.T) {
	ctx := context.Background()
	pool := setupTestPool(t)
	svc, mail := newTestService(t, pool)

	const email = "lockout@example.test"
	ident := OTPIdentity{Email: email}
	if _, err := svc.SendOTP(ctx, ident, PurposeRegister, LanguageEnglish, "127.0.0.1"); err != nil {
		t.Fatalf("SendOTP: %v", err)
	}
	realCode := mail.lastCode(t)
	wrongCode := "000000"
	if wrongCode == realCode {
		wrongCode = "111111"
	}

	for i := 1; i <= otpVerifyMaxAttempts; i++ {
		_, err := svc.VerifyOTP(ctx, ident, wrongCode, "device-1", PurposeRegister, "127.0.0.1")
		if !errors.Is(err, ErrOTPInvalid) {
			t.Fatalf("attempt %d: got err=%v, want ErrOTPInvalid", i, err)
		}
	}

	// One more wrong attempt crosses the cap and invalidates the code.
	_, err := svc.VerifyOTP(ctx, ident, wrongCode, "device-1", PurposeRegister, "127.0.0.1")
	if !errors.Is(err, ErrOTPLocked) {
		t.Fatalf("attempt %d: got err=%v, want ErrOTPLocked", otpVerifyMaxAttempts+1, err)
	}

	// The real code must no longer work: the attempt counter is already over
	// the cap for the rest of this window, so every further call -- even
	// with the correct code -- is reported as locked rather than merely
	// "wrong" or "expired". The OTP hash itself was also deleted on
	// lockout, so even a fresh attempt counter would find no code to check.
	if _, err := svc.VerifyOTP(ctx, ident, realCode, "device-1", PurposeRegister, "127.0.0.1"); !errors.Is(err, ErrOTPLocked) {
		t.Fatalf("using the real code after lockout: got err=%v, want ErrOTPLocked", err)
	}
}

type stubGoogle struct {
	id  GoogleIdentity
	err error
}

func (s stubGoogle) Verify(context.Context, string) (GoogleIdentity, error) {
	return s.id, s.err
}

func TestIntegration_EmailPasswordRegisterAndLogin(t *testing.T) {
	ctx := context.Background()
	pool := setupTestPool(t)
	svc, _ := newTestService(t, pool)

	const email = "patient@example.lk"
	const password = "s3cret-pass"

	reg, err := svc.RegisterEmail(ctx, "Patient@Example.LK", password, "Nimal", "device-mail")
	if err != nil {
		t.Fatalf("RegisterEmail: %v", err)
	}
	if reg.User.Email == nil || *reg.User.Email != email {
		t.Fatalf("registered email = %v, want %s", reg.User.Email, email)
	}
	if reg.User.Phone != "" {
		t.Fatalf("email-only account stored phone %q", reg.User.Phone)
	}
	if reg.AccessToken == "" {
		t.Fatal("RegisterEmail returned no access token")
	}

	if _, err := svc.RegisterEmail(ctx, email, password, "Nimal", ""); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("duplicate register: got %v, want ErrEmailTaken", err)
	}

	login, err := svc.LoginEmail(ctx, email, password, "device-mail")
	if err != nil {
		t.Fatalf("LoginEmail: %v", err)
	}
	if login.User.ID != reg.User.ID {
		t.Fatalf("login user %s != registered user %s", login.User.ID, reg.User.ID)
	}

	if _, err := svc.LoginEmail(ctx, email, "wrong-password", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password: got %v, want ErrInvalidCredentials", err)
	}
	if _, err := svc.LoginEmail(ctx, "nobody@example.lk", password, ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown email: got %v, want ErrInvalidCredentials", err)
	}
}

func TestIntegration_GoogleFindOrCreateAndLinkEmail(t *testing.T) {
	ctx := context.Background()
	pool := setupTestPool(t)
	svc, _ := newTestService(t, pool)

	google := stubGoogle{id: GoogleIdentity{
		Sub: "google-sub-42", Email: "doc@example.lk", EmailVerified: true, Name: "Dr Example",
	}}
	svc.SetGoogle(google)

	first, err := svc.LoginGoogle(ctx, "id-token", "device-g", true)
	if err != nil {
		t.Fatalf("first LoginGoogle: %v", err)
	}
	if first.User.GoogleSub == nil || *first.User.GoogleSub != "google-sub-42" {
		t.Fatalf("google_sub = %v", first.User.GoogleSub)
	}

	again, err := svc.LoginGoogle(ctx, "id-token", "device-g", true)
	if err != nil {
		t.Fatalf("second LoginGoogle: %v", err)
	}
	if again.User.ID != first.User.ID {
		t.Fatalf("second Google login created a new user")
	}

	// An email/password account with the same address is linked, not duplicated.
	svc2, _ := newTestService(t, pool)
	reg, err := svc2.RegisterEmail(ctx, "linked@example.lk", "s3cret-pass", "Linked", "")
	if err != nil {
		t.Fatalf("RegisterEmail: %v", err)
	}
	svc2.SetGoogle(stubGoogle{id: GoogleIdentity{
		Sub: "google-sub-link", Email: "linked@example.lk", EmailVerified: true, Name: "Linked",
	}})
	linked, err := svc2.LoginGoogle(ctx, "id-token", "", true)
	if err != nil {
		t.Fatalf("LoginGoogle link: %v", err)
	}
	if linked.User.ID != reg.User.ID {
		t.Fatalf("Google login minted a second account instead of linking")
	}
	if linked.User.GoogleSub == nil || *linked.User.GoogleSub != "google-sub-link" {
		t.Fatalf("linked google_sub = %v", linked.User.GoogleSub)
	}

	svc2.SetGoogle(stubGoogle{id: GoogleIdentity{
		Sub: "google-unknown", Email: "unknown@example.lk", EmailVerified: true, Name: "Nobody",
	}})
	if _, err := svc2.LoginGoogle(ctx, "id-token", "", false); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("login-only Google for unknown email: got %v, want ErrUserNotFound", err)
	}
}

// TestIntegration_OTPRegistrationByPhoneHasNowhereToSend pins the deliberate
// limit of email-only OTP.
//
// Codes go out over SMTP because this deployment has no SMS rail. A phone
// identity is therefore resolved to the email on its account -- and a phone
// that has never registered has no account, so there is nowhere to send to.
// Every registration attempt by phone number lands here.
//
// It fails closed and says why. The alternative, falling back to the dev SMS
// provider, would "succeed" by printing the code to stdout, which in a
// container is the stream the log collector ships: a patient's login code in
// a log aggregator, and a caller told delivery worked when nothing arrived.
//
// When an SMS rail is configured this test should be replaced, not deleted:
// the branch it guards is in Service.otpAddress.
func TestIntegration_OTPRegistrationByPhoneHasNowhereToSend(t *testing.T) {
	ctx := context.Background()
	pool := setupTestPool(t)
	svc, mail := newTestService(t, pool)

	const unregistered = "+94770000999"
	_, err := svc.SendOTP(ctx, OTPIdentity{Phone: unregistered}, PurposeRegister, LanguageEnglish, "127.0.0.1")
	if !errors.Is(err, ErrNoDeliveryAddress) {
		t.Fatalf("SendOTP to an unregistered phone: got %v, want ErrNoDeliveryAddress", err)
	}
	if got := mail.recipient(); got != "" {
		t.Errorf("a message was sent to %q; nothing should have been delivered", got)
	}

	// An existing account WITH an email is the case that does work, so the
	// failure above is about the missing address and not about phone
	// identities in general.
	u := mustCreateUser(t, ctx, NewRepository(pool), "+94770000998")
	if _, err := svc.SendOTP(ctx, OTPIdentity{Phone: u.Phone}, PurposeLogin, LanguageEnglish, "127.0.0.1"); err != nil {
		t.Fatalf("SendOTP to a registered phone with an email: %v", err)
	}
	if got, want := mail.recipient(), *u.Email; got != want {
		t.Errorf("code delivered to %q, want the account's email %q", got, want)
	}
}

// TestOTPIdentity_ExactlyOneOfPhoneOrEmail covers the front-door rule without
// touching a database.
func TestOTPIdentity_ExactlyOneOfPhoneOrEmail(t *testing.T) {
	cases := []struct {
		name, phone, email string
		wantErr            error
	}{
		{name: "phone only", phone: "+94771234567", wantErr: nil},
		{name: "email only", email: "Person@Example.TEST", wantErr: nil},
		{name: "neither", wantErr: ErrOTPIdentityRequired},
		{name: "both", phone: "+94771234567", email: "a@b.test", wantErr: ErrOTPIdentityAmbiguous},
		{name: "malformed email", email: "not-an-email", wantErr: ErrInvalidEmail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ident, err := NewOTPIdentity(tc.phone, tc.email)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("NewOTPIdentity(%q, %q) err = %v, want %v", tc.phone, tc.email, err, tc.wantErr)
			}
			if tc.wantErr != nil {
				return
			}
			// The key namespaces cannot collide: a phone always carries "+"
			// and an email always carries "@", so a code sent to one can
			// never be redeemed against the other.
			if tc.email != "" && ident.key() != NormalizeEmail(tc.email) {
				t.Errorf("email identity key = %q, want the normalised address", ident.key())
			}
			if tc.phone != "" && ident.key() != tc.phone {
				t.Errorf("phone identity key = %q, want %q", ident.key(), tc.phone)
			}
		})
	}
}
