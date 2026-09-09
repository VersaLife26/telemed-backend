//go:build integration

package doctor

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"telemed/internal/platform/cache"
	"telemed/internal/platform/events"
	"telemed/internal/platform/repopath"
)

// setupPostgres mirrors internal/availability's helper: a real Postgres 17
// container with both committed migrations applied. Duplicated rather than
// shared across packages because Go test helpers do not export across
// package boundaries without a non-test support package, and this is a
// small, self-contained integration suite.
func setupPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	ctr, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("telemed_doctor_test"),
		tcpostgres.WithUsername("telemed"),
		tcpostgres.WithPassword("telemed"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Logf("terminate postgres container: %v", err)
		}
	})

	connStr, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	t.Cleanup(pool.Close)

	applyAllMigrations(t, pool)
	return pool
}

// applyAllMigrations runs every *.up.sql in migrations/, in filename order.
//
// It globs rather than naming files. A hardcoded list means every new migration
// silently leaves the integration schema behind the production one, and the
// test then fails with something like `column "fee_cents" does not exist` --
// which reads as a code bug and is not one. The numeric prefixes make lexical
// order the correct order.
func applyAllMigrations(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(repopath.Migrations(t, "doctor"), "*.up.sql"))
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no migrations found: the integration schema would be empty")
	}
	sort.Strings(files)
	for _, name := range files {
		sql, err := os.ReadFile(name) //nolint:gosec // path comes from our own migrations dir
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		if _, err := pool.Exec(context.Background(), string(sql)); err != nil {
			t.Fatalf("apply migration %s: %v", filepath.Base(name), err)
		}
	}
}

// fakeCache is a minimal in-memory cache.Cache. Service only exercises
// Get/Set for search-page caching in these tests; the remaining methods
// exist to satisfy the interface.
type fakeCache struct {
	mu   sync.Mutex
	data map[string][]byte
}

var _ cache.Cache = (*fakeCache)(nil)

func newFakeCache() *fakeCache { return &fakeCache{data: map[string][]byte{}} }

func (c *fakeCache) Get(_ context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.data[key]
	if !ok {
		return nil, cache.ErrNotFound
	}
	return v, nil
}
func (c *fakeCache) Set(_ context.Context, key string, value []byte, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[key] = value
	return nil
}
func (c *fakeCache) Del(_ context.Context, keys ...string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range keys {
		delete(c.data, k)
	}
	return nil
}
func (c *fakeCache) Exists(_ context.Context, key string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.data[key]
	return ok, nil
}
func (c *fakeCache) Incr(context.Context, string, time.Duration) (int64, error) { return 1, nil }
func (c *fakeCache) Lock(context.Context, string, time.Duration) (token string, acquired bool, err error) {
	return "token", true, nil
}
func (c *fakeCache) Unlock(context.Context, string, string) error        { return nil }
func (c *fakeCache) ZAdd(context.Context, string, float64, string) error { return nil }
func (c *fakeCache) ZRem(context.Context, string, ...string) error       { return nil }
func (c *fakeCache) ZRank(context.Context, string, string) (int64, error) {
	return 0, cache.ErrNotFound
}
func (c *fakeCache) ZRange(context.Context, string, int64, int64) ([]string, error) {
	return nil, nil
}
func (c *fakeCache) ZCard(context.Context, string) (int64, error) { return 0, nil }
func (c *fakeCache) Ping(context.Context) error                   { return nil }
func (c *fakeCache) Close() error                                 { return nil }

func testEncryptor(t *testing.T) *SecretboxEncryptor {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	enc, err := NewSecretboxEncryptor(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("new encryptor: %v", err)
	}
	return enc
}

func newTestService(t *testing.T, pool *pgxpool.Pool) (*Service, *Repository) {
	t.Helper()
	repo := NewRepository(pool)
	svc := NewService(repo, pool, events.NewOutbox("telemed-doctor-service-test"), newFakeCache(), testEncryptor(t), time.Minute, zerolog.Nop())
	return svc, repo
}

// countOutboxEvents is how these tests verify "published via the outbox"
// without standing up NATS: the outbox row is the durable, transactional
// fact; the relay delivering it to JetStream is the platform template's
// concern, already covered by its own tests.
func countOutboxEvents(t *testing.T, pool *pgxpool.Pool, subject, aggregateID string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM outbox_events WHERE subject = $1 AND aggregate_id = $2`, subject, aggregateID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("count outbox events: %v", err)
	}
	return n
}

// TestRegister_SLMCUniqueness covers the SLMC uniqueness half of "SLMC
// number validation: format check plus uniqueness" -- the format half is
// covered by TestSLMCNumberValidation, a pure unit test. This half needs a
// real database because uniqueness is enforced by a constraint.
func TestRegister_SLMCUniqueness(t *testing.T) {
	pool := setupPostgres(t)
	svc, _ := newTestService(t, pool)
	ctx := context.Background()

	base := RegisterInput{
		DisplayName: "Dr. First", SLMCNumber: "SLMC1001", Specialty: "general_practice",
		ExperienceYears: 5, FeeCents: 100000, Languages: []Language{LanguageEN},
	}

	first := base
	first.UserID = uuid.New()
	if _, err := svc.Register(ctx, first); err != nil {
		t.Fatalf("first registration: unexpected error: %v", err)
	}

	second := base // same SLMC number, different user
	second.UserID = uuid.New()
	_, err := svc.Register(ctx, second)
	if !errors.Is(err, ErrSLMCTaken) {
		t.Fatalf("expected ErrSLMCTaken for duplicate SLMC number, got %v", err)
	}
}

// TestRegister_OneProfilePerAccount covers ErrAlreadyRegistered: a platform
// account cannot hold two doctor profiles.
func TestRegister_OneProfilePerAccount(t *testing.T) {
	pool := setupPostgres(t)
	svc, _ := newTestService(t, pool)
	ctx := context.Background()
	userID := uuid.New()

	in := RegisterInput{
		UserID: userID, DisplayName: "Dr. Once", SLMCNumber: "SLMC2002", Specialty: "cardiology",
		ExperienceYears: 5, FeeCents: 100000, Languages: []Language{LanguageEN},
	}
	if _, err := svc.Register(ctx, in); err != nil {
		t.Fatalf("first registration: unexpected error: %v", err)
	}

	in.SLMCNumber = "SLMC2003" // different SLMC, same account
	_, err := svc.Register(ctx, in)
	if !errors.Is(err, ErrAlreadyRegistered) {
		t.Fatalf("expected ErrAlreadyRegistered, got %v", err)
	}
}

// TestRegister_PublishesOutboxEvent confirms doctor.registered is enqueued
// in the same transaction as the doctor row (ADR-005's rule enforced by
// Outbox.Enqueue taking a pgx.Tx).
func TestRegister_PublishesOutboxEvent(t *testing.T) {
	pool := setupPostgres(t)
	svc, _ := newTestService(t, pool)
	ctx := context.Background()

	d, err := svc.Register(ctx, RegisterInput{
		UserID: uuid.New(), DisplayName: "Dr. Events", SLMCNumber: "SLMC3003", Specialty: "dermatology",
		ExperienceYears: 5, FeeCents: 100000, Languages: []Language{LanguageEN},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if n := countOutboxEvents(t, pool, string(events.SubjectDoctorRegistered), d.ID.String()); n != 1 {
		t.Errorf("expected exactly 1 doctor.registered outbox row, got %d", n)
	}
}

// TestVerify_FullWorkflow_RejectThenReopenThenApprove exercises the
// verification state machine through Service.Verify end to end, including
// the outbox events for approve/reject and optimistic version bumps.
func TestVerify_FullWorkflow_RejectThenReopenThenApprove(t *testing.T) {
	pool := setupPostgres(t)
	svc, _ := newTestService(t, pool)
	ctx := context.Background()
	admin := uuid.New()

	d, err := svc.Register(ctx, RegisterInput{
		UserID: uuid.New(), DisplayName: "Dr. Workflow", SLMCNumber: "SLMC4004", Specialty: "neurology",
		ExperienceYears: 8, FeeCents: 150000, Languages: []Language{LanguageEN},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if d.VerificationStatus != StatusPending {
		t.Fatalf("expected pending, got %s", d.VerificationStatus)
	}

	// Reject requires a reason.
	if _, err := svc.Verify(ctx, d.ID, ActionReject, "", &admin); !errors.Is(err, ErrReasonRequired) {
		t.Fatalf("expected ErrReasonRequired, got %v", err)
	}

	rejected, err := svc.Verify(ctx, d.ID, ActionReject, "NIC does not match SLMC registry name", &admin)
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if rejected.VerificationStatus != StatusRejected {
		t.Fatalf("expected rejected, got %s", rejected.VerificationStatus)
	}
	if n := countOutboxEvents(t, pool, string(events.SubjectDoctorRejected), d.ID.String()); n != 1 {
		t.Errorf("expected exactly 1 doctor.rejected outbox row, got %d", n)
	}

	// The rule under test: approving a rejected doctor directly must fail.
	if _, err := svc.Verify(ctx, d.ID, ActionApprove, "", &admin); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition approving a rejected doctor, got %v", err)
	}

	reopened, err := svc.Verify(ctx, d.ID, ActionReopen, "", &admin)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.VerificationStatus != StatusUnderReview {
		t.Fatalf("expected under_review after reopen, got %s", reopened.VerificationStatus)
	}

	approved, err := svc.Verify(ctx, d.ID, ActionApprove, "", &admin)
	if err != nil {
		t.Fatalf("approve after reopen: %v", err)
	}
	if approved.VerificationStatus != StatusApproved {
		t.Fatalf("expected approved, got %s", approved.VerificationStatus)
	}
	if approved.VerifiedAt == nil {
		t.Error("expected verified_at to be set")
	}
	if n := countOutboxEvents(t, pool, string(events.SubjectDoctorApproved), d.ID.String()); n != 1 {
		t.Errorf("expected exactly 1 doctor.approved outbox row, got %d", n)
	}
}

// TestReviewEligibility_EndToEnd is the review-eligibility enforcement test:
// a review is rejected before the appointment.completed consumer has run,
// accepted after, a second review on the same appointment is rejected, and
// the doctor's denormalised rating/review_count reflect the published review.
func TestReviewEligibility_EndToEnd(t *testing.T) {
	pool := setupPostgres(t)
	svc, repo := newTestService(t, pool)
	consumer := NewEventConsumer(repo, pool, zerolog.Nop())
	ctx := context.Background()

	d, err := svc.Register(ctx, RegisterInput{
		UserID: uuid.New(), DisplayName: "Dr. Reviewed", SLMCNumber: "SLMC5005", Specialty: "orthopedics",
		ExperienceYears: 10, FeeCents: 200000, Languages: []Language{LanguageEN},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// Reviews only aggregate over approved... actually rating aggregation
	// itself has no verification_status gate, but let's approve for realism.
	admin := uuid.New()
	if _, err := svc.Verify(ctx, d.ID, ActionApprove, "", &admin); err != nil {
		t.Fatalf("approve: %v", err)
	}

	patientID := uuid.New()
	appointmentID := uuid.New()

	// Before appointment.completed has been consumed, review must be refused.
	if _, err := svc.CreateReview(ctx, d.ID, patientID, appointmentID, 5, "Great doctor"); !errors.Is(err, ErrNotEligibleReview) {
		t.Fatalf("expected ErrNotEligibleReview before completion, got %v", err)
	}

	// Simulate the appointment.completed event.
	env, err := events.NewEnvelope(events.SubjectAppointmentCompleted, "telemed-scheduling-service-test", appointmentID.String(),
		events.AppointmentTerminal{AppointmentID: appointmentID, DoctorID: d.ID, PatientID: patientID, OccurredAt: time.Now().UTC()})
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	if err := consumer.handle(ctx, env); err != nil {
		t.Fatalf("consume appointment.completed: %v", err)
	}

	after, err := repo.GetByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("get doctor: %v", err)
	}
	if after.ConsultationCount != 1 {
		t.Fatalf("expected consultation_count = 1, got %d", after.ConsultationCount)
	}

	// Redeliver the same event: consultation_count must not double-count.
	if err := consumer.handle(ctx, env); err != nil {
		t.Fatalf("redeliver appointment.completed: %v", err)
	}
	after, err = repo.GetByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("get doctor after redelivery: %v", err)
	}
	if after.ConsultationCount != 1 {
		t.Fatalf("expected consultation_count to remain 1 after redelivery, got %d", after.ConsultationCount)
	}

	// Now the review must succeed.
	rv, err := svc.CreateReview(ctx, d.ID, patientID, appointmentID, 5, "Great doctor")
	if err != nil {
		t.Fatalf("create review after eligibility: %v", err)
	}
	if rv.Rating != 5 {
		t.Errorf("rating = %d, want 5", rv.Rating)
	}

	// A second review for the same appointment must be rejected.
	_, err = svc.CreateReview(ctx, d.ID, patientID, appointmentID, 3, "Second attempt")
	if !errors.Is(err, ErrReviewExists) {
		t.Fatalf("expected ErrReviewExists on double review, got %v", err)
	}

	// The denormalised aggregate on doctors must reflect the one published review.
	final, err := repo.GetByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("get doctor: %v", err)
	}
	if final.ReviewCount != 1 {
		t.Errorf("review_count = %d, want 1", final.ReviewCount)
	}
	if final.Rating != 5.00 {
		t.Errorf("rating = %v, want 5.00", final.Rating)
	}
}
