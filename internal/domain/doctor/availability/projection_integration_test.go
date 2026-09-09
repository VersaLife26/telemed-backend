//go:build integration

package availability

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"telemed/internal/platform/database"
	"telemed/internal/platform/repopath"
)

// setupPostgres starts a real Postgres 17 container (the pinned platform
// version), applies the two committed migrations against it, and returns a
// pool. Availability's correctness under out-of-order delivery depends on
// real Postgres semantics (ON CONFLICT ... WHERE, FILTER aggregates); no
// amount of mocking a driver interface would catch a mistake there, which is
// why this is an integration test and not a unit test with a fake pool.
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

// applyAllMigrations runs every *.up.sql in migrations/, in filename order.
// Globbing rather than naming files keeps this schema in step with production
// automatically; a hardcoded list silently rots one migration at a time.
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

// seedDoctor inserts the minimum viable doctors row a slot event's foreign
// key needs.
func seedDoctor(t *testing.T, pool *pgxpool.Pool, doctorID uuid.UUID) {
	t.Helper()
	const q = `
		INSERT INTO doctors (id, user_id, slmc_number, specialty, fee_cents, display_name, verification_status)
		VALUES ($1, gen_random_uuid(), $2, 'general_practice', 100000, 'Dr. Test', 'approved')`
	if _, err := pool.Exec(context.Background(), q, doctorID, "T"+doctorID.String()[:8]); err != nil {
		t.Fatalf("seed doctor: %v", err)
	}
}

func apply(t *testing.T, pool *pgxpool.Pool, repo *Repository, loc *time.Location,
	eventID uuid.UUID, occurredAt time.Time, status SlotStatus, payload SlotEventPayload,
) {
	t.Helper()
	err := database.InTx(context.Background(), pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return repo.ApplySlotEvent(context.Background(), tx, eventID, occurredAt, status, payload, loc)
	})
	if err != nil {
		t.Fatalf("apply slot event: %v", err)
	}
}

// TestApplySlotEvent_InOrder is the baseline: generated, then booked, then
// released, delivered in the order they logically happened. Final state must
// be AVAILABLE and the summary must count it.
func TestApplySlotEvent_InOrder(t *testing.T) {
	pool := setupPostgres(t)
	repo := NewRepository()
	loc := time.UTC

	doctorID := uuid.New()
	seedDoctor(t, pool, doctorID)
	slotID := uuid.New()
	start := time.Now().Add(2 * time.Hour).Truncate(time.Minute)
	payload := SlotEventPayload{SlotID: slotID, DoctorID: doctorID, StartAt: start, EndAt: start.Add(15 * time.Minute)}

	t0 := time.Now().Add(-3 * time.Hour)
	apply(t, pool, repo, loc, uuid.New(), t0, SlotAvailable, payload)
	assertSlotStatus(t, pool, slotID, SlotAvailable)

	t1 := t0.Add(1 * time.Minute)
	apply(t, pool, repo, loc, uuid.New(), t1, SlotBooked, payload)
	assertSlotStatus(t, pool, slotID, SlotBooked)
	assertSummary(t, pool, doctorID, start, 0, false)

	t2 := t1.Add(1 * time.Minute)
	apply(t, pool, repo, loc, uuid.New(), t2, SlotAvailable, payload)
	assertSlotStatus(t, pool, slotID, SlotAvailable)
	assertSummary(t, pool, doctorID, start, 1, true)
}

// TestApplySlotEvent_OutOfOrder_ReleasedBeforeBooked simulates the released
// event (logically last) arriving before the booked event (logically
// second) -- exactly the redelivery scenario JetStream's at-least-once,
// unordered guarantee allows. The final state must still be AVAILABLE
// (released is the true last event by occurred_at), not BOOKED (which
// arrival order alone would produce).
func TestApplySlotEvent_OutOfOrder_ReleasedBeforeBooked(t *testing.T) {
	pool := setupPostgres(t)
	repo := NewRepository()
	loc := time.UTC

	doctorID := uuid.New()
	seedDoctor(t, pool, doctorID)
	slotID := uuid.New()
	start := time.Now().Add(3 * time.Hour).Truncate(time.Minute)
	payload := SlotEventPayload{SlotID: slotID, DoctorID: doctorID, StartAt: start, EndAt: start.Add(15 * time.Minute)}

	base := time.Now().Add(-1 * time.Hour)
	generatedAt := base
	bookedAt := base.Add(1 * time.Minute)
	releasedAt := base.Add(2 * time.Minute)

	// Delivery order: generated, released, booked -- booked arrives LAST
	// despite happening chronologically BEFORE released.
	apply(t, pool, repo, loc, uuid.New(), generatedAt, SlotAvailable, payload)
	apply(t, pool, repo, loc, uuid.New(), releasedAt, SlotAvailable, payload)
	apply(t, pool, repo, loc, uuid.New(), bookedAt, SlotBooked, payload)

	// The booked event is stale relative to what's already applied (released,
	// which has a later occurred_at) and must be ignored.
	assertSlotStatus(t, pool, slotID, SlotAvailable)
	assertSummary(t, pool, doctorID, start, 1, true)
}

// TestApplySlotEvent_OutOfOrder_GeneratedArrivesLast simulates the
// slot.generated event -- logically first -- being redelivered (or simply
// delayed) after slot.booked has already been applied. The booked state,
// being logically later, must win.
func TestApplySlotEvent_OutOfOrder_GeneratedArrivesLast(t *testing.T) {
	pool := setupPostgres(t)
	repo := NewRepository()
	loc := time.UTC

	doctorID := uuid.New()
	seedDoctor(t, pool, doctorID)
	slotID := uuid.New()
	start := time.Now().Add(4 * time.Hour).Truncate(time.Minute)
	payload := SlotEventPayload{SlotID: slotID, DoctorID: doctorID, StartAt: start, EndAt: start.Add(15 * time.Minute)}

	base := time.Now().Add(-1 * time.Hour)
	generatedAt := base
	bookedAt := base.Add(1 * time.Minute)

	// Delivery order: booked first (e.g. this consumer replica was behind),
	// then the generated event catches up.
	apply(t, pool, repo, loc, uuid.New(), bookedAt, SlotBooked, payload)
	apply(t, pool, repo, loc, uuid.New(), generatedAt, SlotAvailable, payload)

	assertSlotStatus(t, pool, slotID, SlotBooked)
	assertSummary(t, pool, doctorID, start, 0, false)
}

// TestApplySlotEvent_DuplicateDelivery replays the exact same event (same
// event id) twice, as JetStream's at-least-once guarantee allows on an
// unacked redelivery. The second application must be a complete no-op.
func TestApplySlotEvent_DuplicateDelivery(t *testing.T) {
	pool := setupPostgres(t)
	repo := NewRepository()
	loc := time.UTC

	doctorID := uuid.New()
	seedDoctor(t, pool, doctorID)
	slotID := uuid.New()
	start := time.Now().Add(5 * time.Hour).Truncate(time.Minute)
	payload := SlotEventPayload{SlotID: slotID, DoctorID: doctorID, StartAt: start, EndAt: start.Add(15 * time.Minute)}

	eventID := uuid.New()
	occurredAt := time.Now().Add(-30 * time.Minute)

	apply(t, pool, repo, loc, eventID, occurredAt, SlotBooked, payload)
	assertSlotStatus(t, pool, slotID, SlotBooked)

	// Redeliver the identical event. If this were mistakenly treated as a
	// fresh "booked" transition it would still show BOOKED here, so this
	// test alone would not catch a broken guard; TestApplySlotEvent_DuplicateDelivery_DoesNotResurrectAvailability
	// below is the one that actually exercises the failure mode.
	apply(t, pool, repo, loc, eventID, occurredAt, SlotBooked, payload)
	assertSlotStatus(t, pool, slotID, SlotBooked)
}

// TestApplySlotEvent_DuplicateDelivery_DoesNotResurrectAvailability is the
// sharper version: after booked -> released, redeliver the (now stale)
// booked event. A broken idempotency guard that dedupes only on exact
// timestamp equality, rather than "not newer than what's stored", would
// incorrectly flip the slot back to booked, or at least fail to reject it
// cleanly. It must have zero effect.
func TestApplySlotEvent_DuplicateDelivery_DoesNotResurrectAvailability(t *testing.T) {
	pool := setupPostgres(t)
	repo := NewRepository()
	loc := time.UTC

	doctorID := uuid.New()
	seedDoctor(t, pool, doctorID)
	slotID := uuid.New()
	start := time.Now().Add(6 * time.Hour).Truncate(time.Minute)
	payload := SlotEventPayload{SlotID: slotID, DoctorID: doctorID, StartAt: start, EndAt: start.Add(15 * time.Minute)}

	base := time.Now().Add(-2 * time.Hour)
	bookedID, bookedAt := uuid.New(), base
	releasedAt := base.Add(1 * time.Minute)

	apply(t, pool, repo, loc, bookedID, bookedAt, SlotBooked, payload)
	apply(t, pool, repo, loc, uuid.New(), releasedAt, SlotAvailable, payload)
	assertSlotStatus(t, pool, slotID, SlotAvailable)

	// Redeliver the stale booked event (older occurred_at than what's stored).
	apply(t, pool, repo, loc, bookedID, bookedAt, SlotBooked, payload)
	assertSlotStatus(t, pool, slotID, SlotAvailable)
	assertSummary(t, pool, doctorID, start, 1, true)
}

// TestApplySlotEvent_SummaryAggregatesMultipleSlots checks that the daily
// summary is a real aggregate over every slot that day, not just the last
// one touched.
func TestApplySlotEvent_SummaryAggregatesMultipleSlots(t *testing.T) {
	pool := setupPostgres(t)
	repo := NewRepository()
	loc := time.UTC

	doctorID := uuid.New()
	seedDoctor(t, pool, doctorID)
	day := time.Now().Add(24 * time.Hour).Truncate(24 * time.Hour).Add(9 * time.Hour)

	slots := make([]SlotEventPayload, 3)
	for i := range slots {
		slots[i] = SlotEventPayload{
			SlotID: uuid.New(), DoctorID: doctorID,
			StartAt: day.Add(time.Duration(i) * 20 * time.Minute),
			EndAt:   day.Add(time.Duration(i)*20*time.Minute + 15*time.Minute),
		}
	}

	base := time.Now().Add(-1 * time.Hour)
	for i, s := range slots {
		apply(t, pool, repo, loc, uuid.New(), base.Add(time.Duration(i)*time.Second), SlotAvailable, s)
	}
	assertSummary(t, pool, doctorID, slots[0].StartAt, 3, true)

	// Book the earliest slot; next_available_at must move to the second.
	apply(t, pool, repo, loc, uuid.New(), base.Add(10*time.Second), SlotBooked, slots[0])

	sum, ok, err := repo.SummaryFor(context.Background(), mustTx(t, pool), doctorID, dateOnly(slots[0].StartAt, loc))
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if !ok {
		t.Fatal("expected a summary row")
	}
	if sum.AvailableSlotCount != 2 {
		t.Errorf("available_slot_count = %d, want 2", sum.AvailableSlotCount)
	}
	if sum.NextAvailableAt == nil || !sum.NextAvailableAt.Equal(slots[1].StartAt) {
		t.Errorf("next_available_at = %v, want %v", sum.NextAvailableAt, slots[1].StartAt)
	}
}

func mustTx(t *testing.T, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func dateOnly(ts time.Time, loc *time.Location) time.Time {
	local := ts.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
}

func assertSlotStatus(t *testing.T, pool *pgxpool.Pool, slotID uuid.UUID, want SlotStatus) {
	t.Helper()
	var got string
	err := pool.QueryRow(context.Background(), `SELECT status FROM doctor_slot_state WHERE slot_id = $1`, slotID).Scan(&got)
	if err != nil {
		t.Fatalf("query slot status: %v", err)
	}
	if SlotStatus(got) != want {
		t.Errorf("slot %s status = %s, want %s", slotID, got, want)
	}
}

func assertSummary(t *testing.T, pool *pgxpool.Pool, doctorID uuid.UUID, slotStart time.Time, wantCount int, wantNextAvailable bool) {
	t.Helper()
	date := dateOnly(slotStart, time.UTC)
	var count int
	var nextAvail *time.Time
	err := pool.QueryRow(context.Background(),
		`SELECT available_slot_count, next_available_at FROM doctor_availability_summary WHERE doctor_id = $1 AND date = $2`,
		doctorID, date,
	).Scan(&count, &nextAvail)
	if err != nil {
		t.Fatalf("query summary: %v", err)
	}
	if count != wantCount {
		t.Errorf("available_slot_count = %d, want %d", count, wantCount)
	}
	if wantNextAvailable && nextAvail == nil {
		t.Error("expected next_available_at to be set, got nil")
	}
	if !wantNextAvailable && nextAvail != nil {
		t.Errorf("expected next_available_at to be nil, got %v", *nextAvail)
	}
}
