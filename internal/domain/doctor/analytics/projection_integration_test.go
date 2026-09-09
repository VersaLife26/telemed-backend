//go:build integration

package analytics

import (
	"context"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/repopath"
)

// The analytics projection's correctness claim is that it converges to the same
// numbers no matter what order JetStream delivers in, and no matter how many
// times. That claim rests on real Postgres semantics -- ON CONFLICT ... WHERE,
// FILTER aggregates, CTE snapshot visibility -- so it is tested against a real
// Postgres 17 and not a fake pool. A mock of those would be a mock of the thing
// under test.

func setupPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	ctr, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("telemed_doctor_test"),
		tcpostgres.WithUsername("telemed"),
		tcpostgres.WithPassword("telemed"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
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

	// Migrate into svc_doctor, not public: that is the shape production runs.
	connStr, err = database.EnsureSchema(ctx, connStr, "doctor")
	if err != nil {
		t.Fatalf("provision schema: %v", err)
	}
	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	t.Cleanup(pool.Close)

	applyAllMigrations(t, pool)
	return pool
}

// applyAllMigrations runs every *.up.sql in filename order. Globbing rather
// than naming files keeps the test schema in step with production
// automatically; a hardcoded list rots one migration at a time.
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

func colombo(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Colombo")
	if err != nil {
		t.Fatalf("load Asia/Colombo: %v", err)
	}
	return loc
}

// seedDoctor inserts the minimum viable doctors row. reviews has a FK to it, so
// the rating half of the summary needs one.
func seedDoctor(t *testing.T, pool *pgxpool.Pool, doctorID uuid.UUID) {
	t.Helper()
	const q = `
		INSERT INTO doctors (id, user_id, slmc_number, specialty, fee_cents, display_name, verification_status)
		VALUES ($1, gen_random_uuid(), $2, 'general_practice', 250000, 'Dr. Test', 'approved')`
	if _, err := pool.Exec(context.Background(), q, doctorID, "T"+doctorID.String()[:8]); err != nil {
		t.Fatalf("seed doctor: %v", err)
	}
}

func seedReview(t *testing.T, pool *pgxpool.Pool, doctorID uuid.UUID, rating int, at time.Time) {
	t.Helper()
	const q = `
		INSERT INTO reviews (doctor_id, patient_id, appointment_id, rating, is_published, created_at, updated_at)
		VALUES ($1, gen_random_uuid(), gen_random_uuid(), $2, TRUE, $3, $3)`
	if _, err := pool.Exec(context.Background(), q, doctorID, rating, at.UTC()); err != nil {
		t.Fatalf("seed review: %v", err)
	}
}

// envelope builds a delivery the consumer can handle, so the tests exercise the
// real decode path rather than calling the repository directly.
func envelope(t *testing.T, subject events.Subject, occurredAt time.Time, payload any) events.Envelope {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return events.Envelope{
		ID: uuid.New(), Subject: subject, Version: 1,
		OccurredAt: occurredAt.UTC(), Producer: "test", Payload: raw,
	}
}

func newTestConsumer(t *testing.T, pool *pgxpool.Pool) (*Consumer, *Service) {
	t.Helper()
	loc := colombo(t)
	return NewConsumer(pool, loc, zerolog.Nop()), NewService(pool, loc)
}

// deliver feeds envelopes through the consumer in the given order.
func deliver(t *testing.T, c *Consumer, envs ...events.Envelope) {
	t.Helper()
	for _, env := range envs {
		if err := c.handle(context.Background(), env); err != nil {
			t.Fatalf("handle %s: %v", env.Subject, err)
		}
	}
}

// TestProjectionIsIdempotentAndOrderIndependent is the central claim.
//
// JetStream is at-least-once with no cross-delivery ordering guarantee. The
// same six events are delivered in a random order, then EVERY one is delivered
// a second time, and the answer must be identical to a single clean pass. A
// counter-based projection fails this test on the first duplicate; an
// envelope-id dedupe fails it on the first out-of-order pair.
func TestProjectionIsIdempotentAndOrderIndependent(t *testing.T) {
	pool := setupPostgres(t)
	c, svc := newTestConsumer(t, pool)
	loc := colombo(t)

	doctorID := uuid.New()
	seedDoctor(t, pool, doctorID)

	day := time.Date(2026, 8, 10, 9, 0, 0, 0, loc) // Monday 09:00 Colombo
	apptA, apptB, apptC := uuid.New(), uuid.New(), uuid.New()
	consultA := uuid.New()
	paymentA := uuid.New()

	envs := []events.Envelope{
		envelope(t, events.SubjectAppointmentCompleted, day.Add(time.Hour), events.AppointmentTerminal{
			AppointmentID: apptA, PatientID: uuid.New(), DoctorID: doctorID,
			StartAt: day, OccurredAt: day.Add(time.Hour),
		}),
		envelope(t, events.SubjectAppointmentNoShow, day.Add(2*time.Hour), events.AppointmentTerminal{
			AppointmentID: apptB, PatientID: uuid.New(), DoctorID: doctorID,
			StartAt: day.Add(time.Hour), OccurredAt: day.Add(2 * time.Hour),
		}),
		envelope(t, events.SubjectAppointmentCancelled, day.Add(-24*time.Hour), events.AppointmentCancelled{
			AppointmentID: apptC, PatientID: uuid.New(), DoctorID: doctorID,
			StartAt: day.Add(2 * time.Hour), CancelledBy: "patient",
			RefundPolicy: "FULL", RefundPercent: 100, CancelledAt: day.Add(-24 * time.Hour),
		}),
		envelope(t, events.SubjectConsultationEnded, day.Add(90*time.Minute), events.ConsultationEnded{
			ConsultationID: consultA, AppointmentID: apptA, PatientID: uuid.New(), DoctorID: doctorID,
			DurationSeconds: 1200, EndReason: "completed", EndedAt: day.Add(90 * time.Minute),
		}),
		envelope(t, events.SubjectPaymentSucceeded, day.Add(3*time.Hour), events.PaymentSucceeded{
			PaymentID: paymentA, AppointmentID: apptA, PatientID: uuid.New(), DoctorID: doctorID,
			AmountCents: 250000, Currency: "LKR", CommissionCents: 37500, PayoutCents: 212500,
			Provider: "payhere", SucceededAt: day.Add(3 * time.Hour),
		}),
		envelope(t, events.SubjectPayoutSent, day.Add(72*time.Hour), events.PayoutSent{
			PayoutID: uuid.New(), DoctorID: doctorID, AmountCents: 212500, Currency: "LKR",
			PeriodStart: "2026-08-01", PeriodEnd: "2026-08-15",
			TransferID: "tr_test", SentAt: day.Add(72 * time.Hour),
		}),
	}

	// Shuffle, deliver, shuffle again, redeliver every one.
	rng := rand.New(rand.NewSource(20260820)) //nolint:gosec // determinism beats cryptographic quality here
	shuffled := append([]events.Envelope(nil), envs...)
	rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	deliver(t, c, shuffled...)

	rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	deliver(t, c, shuffled...)

	ctx := context.Background()
	rangeAug, err := svc.ResolveRange("2026-08-01", "2026-08-31")
	if err != nil {
		t.Fatalf("range: %v", err)
	}

	summary, err := svc.Summarise(ctx, doctorID, rangeAug)
	if err != nil {
		t.Fatalf("summarise: %v", err)
	}
	if summary.CompletedCount != 1 || summary.NoShowCount != 1 || summary.CancelledCount != 1 {
		t.Fatalf("counts after two full deliveries = %d/%d/%d, want 1/1/1 -- the projection double-counted",
			summary.CompletedCount, summary.NoShowCount, summary.CancelledCount)
	}
	if summary.ConsultationCount != 1 || summary.ConsultationSeconds != 1200 {
		t.Errorf("consultations = %d calls / %ds, want 1 / 1200",
			summary.ConsultationCount, summary.ConsultationSeconds)
	}
	if got := summary.AverageConsultationSeconds(); got != 1200 {
		t.Errorf("average duration = %v, want 1200", got)
	}

	earnings, err := svc.Earn(ctx, doctorID, rangeAug)
	if err != nil {
		t.Fatalf("earn: %v", err)
	}
	primary, ok := earnings.Primary()
	if !ok {
		t.Fatal("no earnings after a settled payment")
	}
	if primary.GrossCents != 250000 || primary.CommissionCents != 37500 || primary.NetCents != 212500 {
		t.Errorf("earnings = %d/%d/%d cents, want 250000/37500/212500 -- money double-counted",
			primary.GrossCents, primary.CommissionCents, primary.NetCents)
	}
	if primary.Currency != "LKR" {
		t.Errorf("currency = %q, want LKR", primary.Currency)
	}
	// The payout covers 1-15 August and the payment landed on the 10th, so the
	// whole window is settled.
	if primary.PaidCents != 212500 || primary.Status() != PayoutStatusPaid {
		t.Errorf("payout = %d cents / %q, want 212500 / paid", primary.PaidCents, primary.Status())
	}

	peak, err := svc.PeakHours(ctx, doctorID)
	if err != nil {
		t.Fatalf("peak hours: %v", err)
	}
	if peak.Total != 3 {
		t.Errorf("peak-hours total = %d, want 3", peak.Total)
	}
	// Monday is time.Weekday 1. The three appointments started at 09:00, 10:00
	// and 11:00 Colombo.
	if got := peak.ByHourOfDay[9].BookingCount; got != 1 {
		t.Errorf("09:00 bucket = %d, want 1", got)
	}
	for _, b := range peak.Buckets {
		if b.DayOfWeek != 1 {
			t.Errorf("bucket on day %d; every appointment was a Monday", b.DayOfWeek)
		}
	}
}

// TestStaleRedeliveryDoesNotRollAnOutcomeBack.
//
// An appointment marked no-show and then completed (the doctor corrected it)
// must not revert when the older no-show event is redelivered late. This is
// exactly the case an envelope-id dedupe cannot handle: the id is new to the
// consumer, so it would apply it.
func TestStaleRedeliveryDoesNotRollAnOutcomeBack(t *testing.T) {
	pool := setupPostgres(t)
	c, svc := newTestConsumer(t, pool)
	loc := colombo(t)

	doctorID := uuid.New()
	seedDoctor(t, pool, doctorID)
	apptID := uuid.New()
	start := time.Date(2026, 8, 11, 14, 0, 0, 0, loc)

	older := envelope(t, events.SubjectAppointmentNoShow, start.Add(time.Hour), events.AppointmentTerminal{
		AppointmentID: apptID, PatientID: uuid.New(), DoctorID: doctorID,
		StartAt: start, OccurredAt: start.Add(time.Hour),
	})
	newer := envelope(t, events.SubjectAppointmentCompleted, start.Add(2*time.Hour), events.AppointmentTerminal{
		AppointmentID: apptID, PatientID: uuid.New(), DoctorID: doctorID,
		StartAt: start, OccurredAt: start.Add(2 * time.Hour),
	})

	deliver(t, c, newer, older) // the correction first, the stale one after

	rng, err := svc.ResolveRange("2026-08-01", "2026-08-31")
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	summary, err := svc.Summarise(context.Background(), doctorID, rng)
	if err != nil {
		t.Fatalf("summarise: %v", err)
	}
	if summary.CompletedCount != 1 || summary.NoShowCount != 0 {
		t.Fatalf("outcome = %d completed / %d no-show, want 1/0: a stale redelivery rolled the correction back",
			summary.CompletedCount, summary.NoShowCount)
	}
	if summary.TotalAppointments() != 1 {
		t.Errorf("total = %d, want 1: the two events describe ONE appointment", summary.TotalAppointments())
	}
}

// TestAbandonedCallsDoNotDragTheAverageDown.
//
// A four-second failed connection is a real ConsultationEnded. Averaging it in
// would understate every doctor's consultation length -- a number they are
// shown as a quality metric and have no way to explain.
func TestAbandonedCallsDoNotDragTheAverageDown(t *testing.T) {
	pool := setupPostgres(t)
	c, svc := newTestConsumer(t, pool)
	loc := colombo(t)

	doctorID := uuid.New()
	seedDoctor(t, pool, doctorID)
	day := time.Date(2026, 8, 12, 10, 0, 0, 0, loc)

	deliver(t, c,
		envelope(t, events.SubjectConsultationEnded, day, events.ConsultationEnded{
			ConsultationID: uuid.New(), AppointmentID: uuid.New(), DoctorID: doctorID,
			DurationSeconds: 1200, EndReason: "completed", EndedAt: day,
		}),
		envelope(t, events.SubjectConsultationEnded, day.Add(time.Hour), events.ConsultationEnded{
			ConsultationID: uuid.New(), AppointmentID: uuid.New(), DoctorID: doctorID,
			DurationSeconds: 4, EndReason: "failed", EndedAt: day.Add(time.Hour),
		}),
		envelope(t, events.SubjectConsultationEnded, day.Add(2*time.Hour), events.ConsultationEnded{
			ConsultationID: uuid.New(), AppointmentID: uuid.New(), DoctorID: doctorID,
			DurationSeconds: 900, EndReason: "abandoned", EndedAt: day.Add(2 * time.Hour),
		}),
	)

	rng, err := svc.ResolveRange("2026-08-01", "2026-08-31")
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	summary, err := svc.Summarise(context.Background(), doctorID, rng)
	if err != nil {
		t.Fatalf("summarise: %v", err)
	}
	if summary.ConsultationCount != 1 || summary.ConsultationSeconds != 1200 {
		t.Fatalf("consultations = %d / %ds, want 1 / 1200: only completed calls count",
			summary.ConsultationCount, summary.ConsultationSeconds)
	}
}

// TestCancellationFlaggedNoShowCountsAsNoShow.
//
// scheduling-service can publish appointment.cancelled with no_show set: the
// patient did not turn up and the appointment was cancelled as a consequence.
// Counting that as an ordinary cancellation understates the exact metric this
// screen exists to show.
func TestCancellationFlaggedNoShowCountsAsNoShow(t *testing.T) {
	pool := setupPostgres(t)
	c, svc := newTestConsumer(t, pool)
	loc := colombo(t)

	doctorID := uuid.New()
	seedDoctor(t, pool, doctorID)
	start := time.Date(2026, 8, 13, 11, 0, 0, 0, loc)

	deliver(t, c, envelope(t, events.SubjectAppointmentCancelled, start, events.AppointmentCancelled{
		AppointmentID: uuid.New(), PatientID: uuid.New(), DoctorID: doctorID,
		StartAt: start, CancelledBy: "system", NoShow: true,
		RefundPolicy: "NONE", CancelledAt: start,
	}))

	rng, err := svc.ResolveRange("2026-08-01", "2026-08-31")
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	summary, err := svc.Summarise(context.Background(), doctorID, rng)
	if err != nil {
		t.Fatalf("summarise: %v", err)
	}
	if summary.NoShowCount != 1 || summary.CancelledCount != 0 {
		t.Errorf("counts = %d no-show / %d cancelled, want 1/0",
			summary.NoShowCount, summary.CancelledCount)
	}
}

// TestWindowRatingIsScopedToTheWindow, and the lifetime figure is not.
func TestWindowRatingIsScopedToTheWindow(t *testing.T) {
	pool := setupPostgres(t)
	_, svc := newTestConsumer(t, pool)
	loc := colombo(t)

	doctorID := uuid.New()
	seedDoctor(t, pool, doctorID)

	seedReview(t, pool, doctorID, 2, time.Date(2026, 6, 15, 12, 0, 0, 0, loc)) // outside
	seedReview(t, pool, doctorID, 5, time.Date(2026, 8, 5, 12, 0, 0, 0, loc))  // inside
	// 23:00 on the last day of the window: this is the review a half-open
	// conversion done in UTC would silently drop.
	seedReview(t, pool, doctorID, 5, time.Date(2026, 8, 31, 23, 0, 0, 0, loc))

	rng, err := svc.ResolveRange("2026-08-01", "2026-08-31")
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	summary, err := svc.Summarise(context.Background(), doctorID, rng)
	if err != nil {
		t.Fatalf("summarise: %v", err)
	}
	if summary.ReviewCount != 2 {
		t.Fatalf("window review count = %d, want 2 (the 23:00 review on the final day is inside)", summary.ReviewCount)
	}
	if summary.AverageRating != 5 {
		t.Errorf("window rating = %v, want 5", summary.AverageRating)
	}
	if summary.LifetimeReviewCount != 3 {
		t.Errorf("lifetime review count = %d, want 3", summary.LifetimeReviewCount)
	}
	if summary.LifetimeRating != 4 {
		t.Errorf("lifetime rating = %v, want 4 ((2+5+5)/3)", summary.LifetimeRating)
	}
}

// TestUnpaidEarningsAreReportedPending. A payout whose period does not cover
// the payment's date settles nothing.
func TestUnpaidEarningsAreReportedPending(t *testing.T) {
	pool := setupPostgres(t)
	c, svc := newTestConsumer(t, pool)
	loc := colombo(t)

	doctorID := uuid.New()
	seedDoctor(t, pool, doctorID)
	paid := time.Date(2026, 8, 20, 10, 0, 0, 0, loc)

	deliver(t, c,
		envelope(t, events.SubjectPaymentSucceeded, paid, events.PaymentSucceeded{
			PaymentID: uuid.New(), AppointmentID: uuid.New(), DoctorID: doctorID,
			AmountCents: 250000, Currency: "LKR", CommissionCents: 37500, PayoutCents: 212500,
			SucceededAt: paid,
		}),
		// Settles the FIRST half of August; the payment landed on the 20th.
		envelope(t, events.SubjectPayoutSent, paid, events.PayoutSent{
			PayoutID: uuid.New(), DoctorID: doctorID, AmountCents: 500000, Currency: "LKR",
			PeriodStart: "2026-08-01", PeriodEnd: "2026-08-15", SentAt: paid,
		}),
	)

	rng, err := svc.ResolveRange("2026-08-16", "2026-08-31")
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	earnings, err := svc.Earn(context.Background(), doctorID, rng)
	if err != nil {
		t.Fatalf("earn: %v", err)
	}
	primary, ok := earnings.Primary()
	if !ok {
		t.Fatal("no earnings in the second half of August")
	}
	if primary.PaidCents != 0 {
		t.Errorf("paid = %d cents, want 0: the payout covers 1-15 and the payment landed on the 20th", primary.PaidCents)
	}
	if primary.UnpaidCents() != 212500 || primary.Status() != PayoutStatusPending {
		t.Errorf("unpaid = %d / status %q, want 212500 / pending", primary.UnpaidCents(), primary.Status())
	}
}

// TestMalformedEventsAreDroppedNotRetriedForever.
//
// A payload missing the ids or the start time will never become valid on
// redelivery. Returning an error would park it in the redelivery loop and block
// every event behind it, so the consumer acknowledges and logs. The projection
// must be untouched.
func TestMalformedEventsAreDroppedNotRetriedForever(t *testing.T) {
	pool := setupPostgres(t)
	c, svc := newTestConsumer(t, pool)

	doctorID := uuid.New()
	seedDoctor(t, pool, doctorID)
	ctx := context.Background()

	bad := []events.Envelope{
		envelope(t, events.SubjectAppointmentCompleted, time.Now(), events.AppointmentTerminal{
			DoctorID: doctorID, // no appointment id
		}),
		envelope(t, events.SubjectAppointmentCompleted, time.Now(), events.AppointmentTerminal{
			AppointmentID: uuid.New(), DoctorID: doctorID, // no start_at
		}),
		envelope(t, events.SubjectConsultationEnded, time.Now(), events.ConsultationEnded{
			AppointmentID: uuid.New(), DoctorID: doctorID, // no consultation id
		}),
		envelope(t, events.SubjectPayoutSent, time.Now(), events.PayoutSent{
			PayoutID: uuid.New(), DoctorID: doctorID,
			PeriodStart: "not-a-date", PeriodEnd: "2026-08-15",
		}),
	}
	for _, env := range bad {
		if err := c.handle(ctx, env); err != nil {
			t.Errorf("%s returned %v; a permanently invalid payload must be acknowledged, not retried",
				env.Subject, err)
		}
	}

	rng, err := svc.ResolveRange("", "")
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	summary, err := svc.Summarise(ctx, doctorID, rng)
	if err != nil {
		t.Fatalf("summarise: %v", err)
	}
	if summary.TotalAppointments() != 0 || summary.ConsultationCount != 0 {
		t.Errorf("malformed events reached the projection: %d appointments, %d consultations",
			summary.TotalAppointments(), summary.ConsultationCount)
	}
}

// TestDoctorsAreIsolated. Every read is keyed on doctor_id, and one doctor
// seeing another's earnings would be the worst bug this surface could have.
func TestDoctorsAreIsolated(t *testing.T) {
	pool := setupPostgres(t)
	c, svc := newTestConsumer(t, pool)
	loc := colombo(t)

	mine, theirs := uuid.New(), uuid.New()
	seedDoctor(t, pool, mine)
	seedDoctor(t, pool, theirs)
	day := time.Date(2026, 8, 14, 9, 0, 0, 0, loc)

	deliver(t, c,
		envelope(t, events.SubjectAppointmentCompleted, day, events.AppointmentTerminal{
			AppointmentID: uuid.New(), DoctorID: theirs, StartAt: day, OccurredAt: day,
		}),
		envelope(t, events.SubjectPaymentSucceeded, day, events.PaymentSucceeded{
			PaymentID: uuid.New(), AppointmentID: uuid.New(), DoctorID: theirs,
			AmountCents: 999999, Currency: "LKR", PayoutCents: 888888, SucceededAt: day,
		}),
	)

	rng, err := svc.ResolveRange("2026-08-01", "2026-08-31")
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	ctx := context.Background()

	summary, err := svc.Summarise(ctx, mine, rng)
	if err != nil {
		t.Fatalf("summarise: %v", err)
	}
	if summary.TotalAppointments() != 0 {
		t.Errorf("I can see %d of another doctor's appointments", summary.TotalAppointments())
	}
	earnings, err := svc.Earn(ctx, mine, rng)
	if err != nil {
		t.Fatalf("earn: %v", err)
	}
	if _, ok := earnings.Primary(); ok {
		t.Error("I can see another doctor's earnings")
	}
	peak, err := svc.PeakHours(ctx, mine)
	if err != nil {
		t.Fatalf("peak hours: %v", err)
	}
	if peak.Total != 0 {
		t.Errorf("I can see %d of another doctor's bookings", peak.Total)
	}
}

// TestLocalDateBucketingUsesColomboNotUTC.
//
// An appointment at 08:00 Colombo on 1 September is 02:30 UTC on 1 September;
// one at 02:00 Colombo is 20:30 UTC on 31 August. Bucketing by the UTC date
// would move the second one to the previous month, and a doctor's September
// session count would be short by every early-morning consultation.
func TestLocalDateBucketingUsesColomboNotUTC(t *testing.T) {
	pool := setupPostgres(t)
	c, svc := newTestConsumer(t, pool)
	loc := colombo(t)

	doctorID := uuid.New()
	seedDoctor(t, pool, doctorID)

	early := time.Date(2026, 9, 1, 2, 0, 0, 0, loc) // 2026-08-31T20:30:00Z
	if got := early.UTC().Format(time.DateOnly); got != "2026-08-31" {
		t.Fatalf("fixture is wrong: 02:00 Colombo on 1 September is %s in UTC", got)
	}

	deliver(t, c, envelope(t, events.SubjectAppointmentCompleted, early, events.AppointmentTerminal{
		AppointmentID: uuid.New(), DoctorID: doctorID, StartAt: early, OccurredAt: early,
	}))

	september, err := svc.ResolveRange("2026-09-01", "2026-09-30")
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	summary, err := svc.Summarise(context.Background(), doctorID, september)
	if err != nil {
		t.Fatalf("summarise: %v", err)
	}
	if summary.CompletedCount != 1 {
		t.Errorf("September sessions = %d, want 1: the 02:00 consultation was bucketed by its UTC date",
			summary.CompletedCount)
	}

	peak, err := svc.PeakHours(context.Background(), doctorID)
	if err != nil {
		t.Fatalf("peak hours: %v", err)
	}
	if peak.ByHourOfDay[2].BookingCount != 1 {
		t.Errorf("the 02:00 bucket is empty; the hour was taken in UTC, not Colombo")
	}
}
