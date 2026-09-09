//go:build integration

// Package scheduling_test's integration tier stands up the real dependencies:
// PostgreSQL 17 and Redis 7.4 in containers, the real cache.RedisCache, and the
// real distributed lock.
//
// The unit tier already proves the booking invariant against a real Postgres
// (see main_test.go) with an in-process cache. What this tier adds is the parts
// that only a real Redis can exercise: the Lua compare-and-delete in Unlock, the
// SET NX TTL, and the sorted-set ordering the waitlist index depends on.
//
// Run with: make test-integration  (needs a working Docker daemon)
package scheduling_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"telemed/internal/domain/scheduling/scheduling"
	"telemed/internal/platform/cache"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/repopath"
)

type stack struct {
	pool  *pgxpool.Pool
	redis *cache.RedisCache
	stop  func()
}

// startStack brings up Postgres 17 and Redis 7.4 and tears them down on
// t.Cleanup, so an aborted run never leaves a container behind.
func startStack(t *testing.T) *stack {
	t.Helper()
	ctx := context.Background()

	pgReq := testcontainers.ContainerRequest{
		Image:        "postgres:17-alpine",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     "telemed",
			"POSTGRES_PASSWORD": "telemed",
			"POSTGRES_DB":       "telemed_scheduling",
			// The container is ephemeral; durability buys nothing and costs
			// most of the test's runtime.
			"POSTGRES_INITDB_ARGS": "--nosync",
		},
		Cmd: []string{"postgres", "-c", "fsync=off", "-c", "synchronous_commit=off", "-c", "max_connections=300"},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(120 * time.Second),
	}
	pgC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: pgReq, Started: true,
	})
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}

	redisReq := testcontainers.ContainerRequest{
		Image:        "redis:7.4-alpine",
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(60 * time.Second),
	}
	redisC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: redisReq, Started: true,
	})
	if err != nil {
		_ = pgC.Terminate(ctx)
		t.Fatalf("start redis container: %v", err)
	}

	stop := func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := redisC.Terminate(shutdown); err != nil {
			t.Logf("terminate redis: %v", err)
		}
		if err := pgC.Terminate(shutdown); err != nil {
			t.Logf("terminate postgres: %v", err)
		}
	}
	t.Cleanup(stop)

	pgHost, err := pgC.Host(ctx)
	if err != nil {
		t.Fatalf("postgres host: %v", err)
	}
	pgPort, err := pgC.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("postgres port: %v", err)
	}
	dsn := fmt.Sprintf("postgres://telemed:telemed@%s:%s/telemed_scheduling?sslmode=disable",
		pgHost, pgPort.Port())

	pool, err := database.Connect(ctx, database.Config{
		URL: dsn, MaxConns: 60, MinConns: 2, AppName: "telemed-scheduling-integration",
	}, zerolog.Nop())
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := applyMigrations(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	redisHost, err := redisC.Host(ctx)
	if err != nil {
		t.Fatalf("redis host: %v", err)
	}
	redisPort, err := redisC.MappedPort(ctx, "6379/tcp")
	if err != nil {
		t.Fatalf("redis port: %v", err)
	}
	rc, err := cache.NewRedis(ctx, cache.Options{
		URL: fmt.Sprintf("redis://%s:%s", redisHost, redisPort.Port()),
	})
	if err != nil {
		t.Fatalf("connect redis: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })

	return &stack{pool: pool, redis: rc, stop: stop}
}

func applyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	files, err := filepath.Glob(filepath.Join(repopath.Migrations(t, "scheduling"), "*.up.sql"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		sql, err := os.ReadFile(f) //nolint:gosec // path comes from a glob of our own repo
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
	}
	return nil
}

func integrationService(t *testing.T, s *stack) *scheduling.Service {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Colombo")
	if err != nil {
		t.Fatalf("tzdata: %v", err)
	}
	return scheduling.NewService(scheduling.Options{
		Pool:       s.pool,
		Repository: scheduling.NewRepository(),
		Locker:     scheduling.RedisLocker{Cache: s.redis},
		Queue:      scheduling.RedisWaitlistQueue{Cache: s.redis},
		Outbox:     events.NewOutbox("telemed-scheduling-service"),
		Clock:      scheduling.SystemClock{},
		Location:   loc,
		Logger:     zerolog.Nop(),
	})
}

// TestIntegrationConcurrentBooking is TestConcurrentBooking against Postgres 17
// and a real Redis, with the real Lua-scripted lock release.
func TestIntegrationConcurrentBooking(t *testing.T) {
	s := startStack(t)
	svc := integrationService(t, s)
	ctx := context.Background()

	const goroutines = 100
	doctorID := uuid.New()
	seedIntegrationPricing(t, s, doctorID)

	slotID := uuid.New()
	start := time.Now().Add(90 * time.Minute).UTC()
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO slots (id, doctor_id, start_at, end_at, status) VALUES ($1,$2,$3,$4,'AVAILABLE')`,
		slotID, doctorID, start, start.Add(15*time.Minute)); err != nil {
		t.Fatalf("seed slot: %v", err)
	}

	var successes, rejections atomic.Int64
	var wg sync.WaitGroup
	gate := make(chan struct{})
	errs := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			_, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: uuid.New()})
			switch {
			case err == nil:
				successes.Add(1)
			case errors.Is(err, scheduling.ErrSlotUnavailable),
				errors.Is(err, scheduling.ErrVersionConflict),
				errors.Is(err, scheduling.ErrSlotLocked):
				rejections.Add(1)
			default:
				errs <- err
			}
		}()
	}
	close(gate)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("unexpected error: %v", err)
	}
	if successes.Load() != 1 {
		t.Fatalf("%d winners, want 1", successes.Load())
	}
	if rejections.Load() != goroutines-1 {
		t.Fatalf("%d clean rejections, want %d", rejections.Load(), goroutines-1)
	}

	repo := scheduling.NewRepository()
	doubled, err := repo.CountDoubleBookedSlots(ctx, s.pool)
	if err != nil {
		t.Fatalf("post-condition: %v", err)
	}
	if doubled != 0 {
		t.Fatalf("DOUBLE BOOKING: %d", doubled)
	}

	// The Redis lock must have been released, not left to expire: a leaked lock
	// would stall the slot for its whole TTL after a burst.
	exists, err := s.redis.Exists(ctx, scheduling.SlotLockKey(slotID))
	if err != nil {
		t.Fatalf("check lock: %v", err)
	}
	if exists {
		t.Fatal("the booking lock was not released after the winner committed")
	}
}

// TestIntegrationTokenGuardedUnlock is ADR-006 against the real Lua script: a
// caller whose TTL expired must not be able to delete the lock somebody else
// now holds.
func TestIntegrationTokenGuardedUnlock(t *testing.T) {
	s := startStack(t)
	ctx := context.Background()
	key := "lock:slot:" + uuid.NewString()

	first, ok, err := s.redis.Lock(ctx, key, 300*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("first lock: ok=%v err=%v", ok, err)
	}

	// Let the TTL lapse, then let somebody else take the lock.
	time.Sleep(500 * time.Millisecond)
	second, ok, err := s.redis.Lock(ctx, key, 5*time.Second)
	if err != nil || !ok {
		t.Fatalf("second lock: ok=%v err=%v", ok, err)
	}

	// The first holder's release must be refused.
	if err := s.redis.Unlock(ctx, key, first); !errors.Is(err, cache.ErrLockNotHeld) {
		t.Fatalf("stale unlock returned %v, want ErrLockNotHeld", err)
	}
	if exists, _ := s.redis.Exists(ctx, key); !exists {
		t.Fatal("a stale holder deleted the current holder's lock")
	}
	if err := s.redis.Unlock(ctx, key, second); err != nil {
		t.Fatalf("current holder could not release: %v", err)
	}
}

// TestIntegrationWaitlistSortedSet exercises the Redis sorted set as the queue
// index, including the fall-through to Postgres when the index is cold.
func TestIntegrationWaitlistSortedSet(t *testing.T) {
	s := startStack(t)
	svc := integrationService(t, s)
	ctx := context.Background()
	loc := svc.Location()

	doctorID := uuid.New()
	seedIntegrationPricing(t, s, doctorID)
	start := time.Now().Add(26 * time.Hour)
	slotID := uuid.New()
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO slots (id, doctor_id, start_at, end_at, status) VALUES ($1,$2,$3,$4,'AVAILABLE')`,
		slotID, doctorID, start.UTC(), start.Add(15*time.Minute).UTC()); err != nil {
		t.Fatalf("seed slot: %v", err)
	}
	date := scheduling.DateIn(start, loc)

	var entries []scheduling.WaitlistEntry
	for i := 0; i < 3; i++ {
		e, err := svc.JoinWaitlist(ctx, uuid.New(), doctorID, date)
		if err != nil {
			t.Fatalf("join %d: %v", i, err)
		}
		entries = append(entries, e)
		time.Sleep(3 * time.Millisecond)
	}

	// The sorted set holds all three, in join order.
	queue := scheduling.RedisWaitlistQueue{Cache: s.redis}
	head, err := queue.Head(ctx, doctorID, date, 10)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if len(head) != 3 || head[0] != entries[0].ID {
		t.Fatalf("queue head = %v, want the three entries in join order starting with %v", head, entries[0].ID)
	}

	// Flushing the index must not cost anybody their place: Postgres is the
	// ledger and promotion falls back to it.
	if err := s.redis.Del(ctx, scheduling.WaitlistQueueKey(doctorID, date)); err != nil {
		t.Fatalf("flush index: %v", err)
	}
	if err := svc.PromoteWaitlist(ctx, doctorID, slotID); err != nil {
		t.Fatalf("promote with a cold index: %v", err)
	}

	var reservedFor *uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT reserved_for FROM slots WHERE id = $1`, slotID).
		Scan(&reservedFor); err != nil {
		t.Fatalf("read slot: %v", err)
	}
	if reservedFor == nil || *reservedFor != entries[0].PatientID {
		t.Fatalf("promoted %v, want the first joiner %v", reservedFor, entries[0].PatientID)
	}
}

// TestIntegrationFullBookingFlow walks a doctor from approval to a completed,
// paid consultation, the way the platform actually runs.
func TestIntegrationFullBookingFlow(t *testing.T) {
	s := startStack(t)
	svc := integrationService(t, s)
	consumers := scheduling.NewConsumers(svc, nil, nil, zerolog.Nop())
	ctx := context.Background()

	doctorID := uuid.New()
	hours := make([]scheduling.WorkingHourPayload, 0, 7)
	for d := 0; d < 7; d++ {
		hours = append(hours, scheduling.WorkingHourPayload{DayOfWeek: d, StartTime: "08:00", EndTime: "20:00"})
	}
	// The canonical payload and the optional scheduling hints, merged the way a
	// producer that carries both would put them on the wire. FeeCents is what
	// makes the booking further down payable at all.
	approvedPayload := map[string]any{
		"doctor_id":   doctorID,
		"user_id":     uuid.New(),
		"doctor_name": "Dr Integration",
		"specialty":   "general",
		"fee_cents":   200000,
		"currency":    "LKR",
		"approved_at": time.Now().UTC(),

		"timezone":              "Asia/Colombo",
		"slot_duration_minutes": 15,
		"max_per_day":           40,
		"advance_days":          7,
		"working_hours":         hours,
	}
	approved, err := events.NewEnvelope(events.SubjectDoctorApproved, "doctor-service", doctorID.String(),
		approvedPayload)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if err := consumers.Handle(ctx, approved); err != nil {
		t.Fatalf("doctor.approved: %v", err)
	}

	// Find something bookable tomorrow.
	tomorrow := scheduling.DateIn(time.Now(), svc.Location()).AddDays(1)
	slots, err := svc.ListSlots(ctx, doctorID, tomorrow)
	if err != nil {
		t.Fatalf("list slots: %v", err)
	}
	if len(slots) == 0 {
		t.Fatal("approval produced no bookable slots for tomorrow")
	}

	patientID := uuid.New()
	appt, err := svc.BookSlot(ctx, scheduling.BookSlotInput{
		SlotID: slots[0].ID, PatientID: patientID, Intake: []byte(`{"symptoms":"cough"}`),
	})
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	// The end-to-end assertion this whole reconciliation is for: the booking
	// carries a payable amount.
	if appt.AmountCents != 200000 || appt.Currency != "LKR" {
		t.Fatalf("appointment quote = %d %s, want 200000 LKR", appt.AmountCents, appt.Currency)
	}

	paymentID := uuid.New()
	paid, err := events.NewEnvelope(events.SubjectPaymentSucceeded, "payment-service", appt.ID.String(),
		scheduling.PaymentResultPayload{PaymentID: &paymentID, AppointmentID: appt.ID, Amount: 200000, Currency: "LKR"})
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if err := consumers.Handle(ctx, paid); err != nil {
		t.Fatalf("payment.succeeded: %v", err)
	}

	confirmed, err := svc.GetAppointment(ctx, appt.ID, patientID, "patient")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if confirmed.Status != scheduling.AppointmentConfirmed {
		t.Fatalf("status = %s, want confirmed", confirmed.Status)
	}

	done, err := svc.MarkTerminal(ctx, appt.ID, doctorID, "doctor", scheduling.AppointmentCompleted)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if done.Status != scheduling.AppointmentCompleted {
		t.Fatalf("status = %s, want completed", done.Status)
	}

	if err := svc.CheckInvariants(ctx); err != nil {
		t.Fatalf("invariants: %v", err)
	}
}

// seedIntegrationPricing gives a doctor a list price. Booking now requires one:
// a doctor with no price cannot be quoted, and a booking that cannot be quoted
// is refused rather than becoming an appointment nobody can pay for.
func seedIntegrationPricing(t *testing.T, s *stack, doctorID uuid.UUID) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `
		INSERT INTO doctor_pricing (doctor_id, specialty, fee_cents, currency, languages, status, last_event_at)
		VALUES ($1, 'general', 200000, 'LKR', ARRAY['en'], 'approved', NOW())
		ON CONFLICT (doctor_id) DO UPDATE SET last_event_at = EXCLUDED.last_event_at`,
		doctorID); err != nil {
		t.Fatalf("seed doctor pricing: %v", err)
	}
}
