package scheduling_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"telemed/internal/domain/scheduling/scheduling"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
)

// newTestService builds a Service against the real database with the supplied
// locker. Everything else is production code: the real repository, the real
// outbox, the real transaction helper.
func newTestService(t *testing.T, pool *pgxpool.Pool, locker scheduling.SlotLocker, queue scheduling.WaitlistQueue) *scheduling.Service {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Colombo")
	if err != nil {
		t.Fatalf("load Asia/Colombo: %v", err)
	}
	if queue == nil {
		queue = scheduling.NoopWaitlistQueue{}
	}
	return scheduling.NewService(scheduling.Options{
		Pool:       pool,
		Repository: scheduling.NewRepository(),
		Locker:     locker,
		Queue:      queue,
		Outbox:     events.NewOutbox("telemed-scheduling-service"),
		Clock:      scheduling.SystemClock{},
		Location:   loc,
		Logger:     zerolog.Nop(),
	})
}

// seedSlot inserts one AVAILABLE slot an hour from now.
func seedSlot(t *testing.T, pool *pgxpool.Pool, doctorID uuid.UUID, startAt time.Time, duration time.Duration) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO slots (id, doctor_id, start_at, end_at, status) VALUES ($1, $2, $3, $4, 'AVAILABLE')`,
		id, doctorID, startAt.UTC(), startAt.Add(duration).UTC())
	if err != nil {
		t.Fatalf("seed slot: %v", err)
	}
	return id
}

// testFeeCents is LKR 2,000.00 -- the figure the design discussion uses for
// "a patient books at LKR 2,000 and pays LKR 2,000".
const testFeeCents = int64(200000)

// seedPricing gives a doctor a list price, which is now a PRECONDITION for
// booking rather than an optional extra.
//
// Every booking test seeds this. That is the change of contract this pass
// introduces and it is deliberate: a doctor with no price cannot be booked,
// because a booking that cannot be priced becomes an appointment that can never
// be paid for. TestBookingWithoutPricing asserts the other side of it.
func seedPricing(t *testing.T, pool *pgxpool.Pool, doctorID uuid.UUID) {
	t.Helper()
	seedPricingAt(t, pool, doctorID, testFeeCents, "LKR", "cardiology", time.Now().UTC())
}

func seedPricingAt(t *testing.T, pool *pgxpool.Pool, doctorID uuid.UUID,
	feeCents int64, currency, specialty string, at time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO doctor_pricing (doctor_id, specialty, fee_cents, currency, languages, status, last_event_at)
		VALUES ($1, $2, $3, $4, $5, 'approved', $6)
		ON CONFLICT (doctor_id) DO UPDATE SET
			specialty = EXCLUDED.specialty, fee_cents = EXCLUDED.fee_cents,
			currency = EXCLUDED.currency, last_event_at = EXCLUDED.last_event_at`,
		doctorID, specialty, feeCents, currency, []string{"en"}, at.UTC())
	if err != nil {
		t.Fatalf("seed pricing: %v", err)
	}
}

// seedPricedSlot is the common case: a doctor who has a price and a free slot.
func seedPricedSlot(t *testing.T, pool *pgxpool.Pool, doctorID uuid.UUID, startAt time.Time, duration time.Duration) uuid.UUID {
	t.Helper()
	seedPricing(t, pool, doctorID)
	return seedSlot(t, pool, doctorID, startAt, duration)
}

// TestConcurrentBooking is the single most important test in the platform.
//
// One hundred goroutines race for one slot. Exactly one must win; the other
// ninety-nine must be turned away with a clean domain error, never a 500 and
// never a second appointment. The post-conditions are queried out of Postgres
// afterwards, because the claim being tested is a claim about the database, not
// about Go.
//
// Run it with: go test -race -run TestConcurrent -count=10 ./...
func TestConcurrentBooking(t *testing.T) {
	pool := requireDB(t)
	svc := newTestService(t, pool, scheduling.RedisLocker{Cache: newMemCache()}, nil)
	runConcurrentBooking(t, pool, svc, true)
}

// TestConcurrentBookingWithoutRedisLock is ADR-007's proof.
//
// The same hundred-goroutine race, with the distributed lock removed entirely.
// If this test's outcome differed from TestConcurrentBooking's, the Redis lock
// would be load-bearing for correctness -- and a lock with a TTL, on a store
// that can lose a key to a failover, is not something correctness may rest on.
// It passes identically, which is the whole point: Redis is a throughput
// optimisation and Postgres is the truth.
func TestConcurrentBookingWithoutRedisLock(t *testing.T) {
	pool := requireDB(t)
	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	runConcurrentBooking(t, pool, svc, false)
}

// TestConcurrentBookingWithFailingLock covers the middle case: Redis is present
// but erroring. The booking path must fail open -- an outage in an optimisation
// must not stop the platform taking bookings.
func TestConcurrentBookingWithFailingLock(t *testing.T) {
	pool := requireDB(t)
	c := newMemCache()
	c.failLock = true
	svc := newTestService(t, pool, scheduling.RedisLocker{Cache: c}, nil)
	runConcurrentBooking(t, pool, svc, false)
}

func runConcurrentBooking(t *testing.T, pool *pgxpool.Pool, svc *scheduling.Service, lockEnabled bool) {
	t.Helper()
	resetTables(t, pool)

	const goroutines = 100
	ctx := context.Background()
	doctorID := uuid.New()
	slotID := seedPricedSlot(t, pool, doctorID, time.Now().Add(90*time.Minute), 15*time.Minute)

	var (
		successes atomic.Int64
		locked    atomic.Int64
		taken     atomic.Int64
		winnerMu  sync.Mutex
		winner    uuid.UUID
		unexpectd = make(chan error, goroutines)
	)

	// A shared start gate: without it the goroutines trickle in as they are
	// scheduled and the first one is usually finished before the hundredth
	// starts, which tests nothing.
	gate := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			patientID := uuid.New()
			<-gate

			appt, err := svc.BookSlot(ctx, scheduling.BookSlotInput{
				SlotID:    slotID,
				PatientID: patientID,
				Intake:    []byte(`{"symptoms":"redacted"}`),
			})
			switch {
			case err == nil:
				successes.Add(1)
				winnerMu.Lock()
				winner = appt.ID
				winnerMu.Unlock()
			case errors.Is(err, scheduling.ErrSlotUnavailable), errors.Is(err, scheduling.ErrVersionConflict):
				taken.Add(1)
			case errors.Is(err, scheduling.ErrSlotLocked):
				locked.Add(1)
			default:
				unexpectd <- err
			}
		}()
	}

	close(gate)
	wg.Wait()
	close(unexpectd)

	for err := range unexpectd {
		t.Errorf("booking failed with an error that is neither success nor a clean rejection: %v", err)
	}

	// --- the headline assertions ---------------------------------------
	if got := successes.Load(); got != 1 {
		t.Fatalf("expected exactly 1 winner, got %d", got)
	}
	if got := locked.Load() + taken.Load(); got != goroutines-1 {
		t.Fatalf("expected %d clean rejections, got %d (locked=%d taken=%d)",
			goroutines-1, got, locked.Load(), taken.Load())
	}
	if !lockEnabled && locked.Load() != 0 {
		t.Fatalf("no distributed lock is configured, yet %d callers were rejected as locked", locked.Load())
	}

	// --- post-conditions, queried out of Postgres -----------------------
	repo := scheduling.NewRepository()

	doubled, err := repo.CountDoubleBookedSlots(ctx, pool)
	if err != nil {
		t.Fatalf("double-booking check: %v", err)
	}
	if doubled != 0 {
		t.Fatalf("DOUBLE BOOKING: %d slot(s) hold more than one live appointment", doubled)
	}

	inconsistent, err := repo.CountInconsistentSlots(ctx, pool)
	if err != nil {
		t.Fatalf("consistency check: %v", err)
	}
	if inconsistent != 0 {
		t.Fatalf("%d slot(s) disagree with the appointments table", inconsistent)
	}

	var apptCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM appointments WHERE slot_id = $1`, slotID).
		Scan(&apptCount); err != nil {
		t.Fatalf("count appointments: %v", err)
	}
	if apptCount != 1 {
		t.Fatalf("expected exactly 1 appointment row for the slot, got %d", apptCount)
	}

	var (
		status    string
		version   int
		apptOnRow *uuid.UUID
	)
	if err := pool.QueryRow(ctx,
		`SELECT status, version, appointment_id FROM slots WHERE id = $1`, slotID).
		Scan(&status, &version, &apptOnRow); err != nil {
		t.Fatalf("read slot: %v", err)
	}
	if status != string(scheduling.SlotBooked) {
		t.Fatalf("slot status = %s, want BOOKED", status)
	}
	if version != 1 {
		t.Fatalf("slot version = %d, want exactly 1: a second increment means a second write won", version)
	}
	winnerMu.Lock()
	defer winnerMu.Unlock()
	if apptOnRow == nil || *apptOnRow != winner {
		t.Fatalf("slot points at %v, winner was %v", apptOnRow, winner)
	}

	// The winning appointment must carry the quote. A booking with no amount is
	// the defect this whole pass exists to remove: it holds a slot, looks
	// confirmed to the patient, and payment-service refuses it as unpayable.
	var (
		gotAmount    *int64
		gotCurrency  *string
		gotSpecialty *string
	)
	if err := pool.QueryRow(ctx,
		`SELECT amount_cents, currency, specialty FROM appointments WHERE slot_id = $1`, slotID).
		Scan(&gotAmount, &gotCurrency, &gotSpecialty); err != nil {
		t.Fatalf("read appointment quote: %v", err)
	}
	if gotAmount == nil || *gotAmount != testFeeCents {
		t.Fatalf("appointment amount_cents = %v, want %d", gotAmount, testFeeCents)
	}
	if gotCurrency == nil || strings.TrimSpace(*gotCurrency) != "LKR" {
		t.Fatalf("appointment currency = %v, want LKR", gotCurrency)
	}
	if gotSpecialty == nil || *gotSpecialty != "cardiology" {
		t.Fatalf("appointment specialty = %v, want cardiology", gotSpecialty)
	}

	// The outbox must carry the booking events, in the same transaction that
	// booked it (ADR-005). Two events: appointment.created and slot.booked.
	var outboxCount int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM outbox_events WHERE subject IN ('appointment.created', 'slot.booked')`).
		Scan(&outboxCount); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if outboxCount != 2 {
		t.Fatalf("expected 2 outbox rows for the winning booking, got %d", outboxCount)
	}
}

// TestConcurrentBookingAcrossManySlots is the throughput-shaped variant: many
// slots, many patients, everyone should get exactly what they asked for and no
// slot may end up doubly booked.
func TestConcurrentBookingAcrossManySlots(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)

	svc := newTestService(t, pool, scheduling.RedisLocker{Cache: newMemCache()}, nil)
	ctx := context.Background()
	doctorID := uuid.New()

	const slots = 20
	const contenders = 5

	seedPricing(t, pool, doctorID)
	slotIDs := make([]uuid.UUID, slots)
	base := time.Now().Add(2 * time.Hour)
	for i := range slotIDs {
		slotIDs[i] = seedSlot(t, pool, doctorID, base.Add(time.Duration(i)*20*time.Minute), 15*time.Minute)
	}

	var successes atomic.Int64
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for _, id := range slotIDs {
		for j := 0; j < contenders; j++ {
			wg.Add(1)
			go func(slotID uuid.UUID) {
				defer wg.Done()
				<-gate
				if _, err := svc.BookSlot(ctx, scheduling.BookSlotInput{
					SlotID: slotID, PatientID: uuid.New(),
				}); err == nil {
					successes.Add(1)
				}
			}(id)
		}
	}
	close(gate)
	wg.Wait()

	if got := successes.Load(); got != slots {
		t.Fatalf("expected %d bookings (one per slot), got %d", slots, got)
	}

	repo := scheduling.NewRepository()
	doubled, err := repo.CountDoubleBookedSlots(ctx, pool)
	if err != nil {
		t.Fatalf("double-booking check: %v", err)
	}
	if doubled != 0 {
		t.Fatalf("DOUBLE BOOKING: %d slots hold more than one live appointment", doubled)
	}
}

// TestBookingRejectsPastSlot and friends: the ordinary single-threaded rules.
func TestBookingRules(t *testing.T) {
	pool := requireDB(t)
	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	ctx := context.Background()

	t.Run("rejects a slot that has already started", func(t *testing.T) {
		resetTables(t, pool)
		slotID := seedPricedSlot(t, pool, uuid.New(), time.Now().Add(-30*time.Minute), 15*time.Minute)
		_, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: uuid.New()})
		if !errors.Is(err, scheduling.ErrSlotInPast) {
			t.Fatalf("err = %v, want ErrSlotInPast", err)
		}
	})

	t.Run("rejects an unknown slot", func(t *testing.T) {
		resetTables(t, pool)
		_, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: uuid.New(), PatientID: uuid.New()})
		if !errors.Is(err, scheduling.ErrSlotNotFound) {
			t.Fatalf("err = %v, want ErrSlotNotFound", err)
		}
	})

	t.Run("rejects a doctor id that does not own the slot", func(t *testing.T) {
		resetTables(t, pool)
		slotID := seedPricedSlot(t, pool, uuid.New(), time.Now().Add(time.Hour), 15*time.Minute)
		other := uuid.New()
		_, err := svc.BookSlot(ctx, scheduling.BookSlotInput{
			SlotID: slotID, PatientID: uuid.New(), DoctorID: &other,
		})
		if !errors.Is(err, scheduling.ErrSlotNotFound) {
			t.Fatalf("err = %v, want ErrSlotNotFound", err)
		}
	})

	t.Run("rejects one patient booking two overlapping consultations", func(t *testing.T) {
		resetTables(t, pool)
		patient := uuid.New()
		start := time.Now().Add(3 * time.Hour)
		first := seedPricedSlot(t, pool, uuid.New(), start, 30*time.Minute)
		// A different doctor, overlapping window: the per-slot unique index
		// cannot see this one, so the service has to.
		second := seedPricedSlot(t, pool, uuid.New(), start.Add(10*time.Minute), 30*time.Minute)

		if _, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: first, PatientID: patient}); err != nil {
			t.Fatalf("first booking: %v", err)
		}
		_, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: second, PatientID: patient})
		if !errors.Is(err, scheduling.ErrDuplicateBooking) {
			t.Fatalf("err = %v, want ErrDuplicateBooking", err)
		}
	})

	t.Run("a cancelled slot can be booked again", func(t *testing.T) {
		// The documentation's `slot_id UNIQUE` would make this impossible: the
		// cancelled appointment would keep the slot locked out forever.
		resetTables(t, pool)
		slotID := seedPricedSlot(t, pool, uuid.New(), time.Now().Add(4*time.Hour), 15*time.Minute)
		patientA, patientB := uuid.New(), uuid.New()

		appt, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: patientA})
		if err != nil {
			t.Fatalf("first booking: %v", err)
		}
		if _, err := svc.CancelAppointment(ctx, scheduling.CancelInput{
			AppointmentID: appt.ID, ActorID: patientA, ActorRole: "patient", Reason: "changed my mind",
		}); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if _, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: patientB}); err != nil {
			t.Fatalf("rebooking a cancelled slot: %v", err)
		}
	})

	t.Run("prepayment is required above the no-show threshold", func(t *testing.T) {
		resetTables(t, pool)
		patient := uuid.New()
		// 2 no-shows out of 5 is 40%, over the 30% threshold and past the
		// 3-appointment minimum history.
		if err := database.InTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO no_show_stats (patient_id, total_appointments, no_show_count) VALUES ($1, 5, 2)`,
				patient)
			return err
		}); err != nil {
			t.Fatalf("seed stats: %v", err)
		}

		slotID := seedPricedSlot(t, pool, uuid.New(), time.Now().Add(5*time.Hour), 15*time.Minute)
		appt, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: patient})
		if err != nil {
			t.Fatalf("booking: %v", err)
		}
		if !appt.PrepaymentRequired {
			t.Fatal("expected prepayment_required for a patient with a 40% no-show rate")
		}
	})
}

// TestCancellationRefundPolicy pins the >2h / <2h rule to the wire contract.
func TestCancellationRefundPolicy(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()

	cases := []struct {
		name      string
		startsIn  time.Duration
		role      string
		wantPolic scheduling.RefundPolicy
		wantPct   int
	}{
		{"patient cancels well ahead", 6 * time.Hour, "patient", scheduling.RefundFull, 100},
		{"patient cancels just inside the window", 90 * time.Minute, "patient", scheduling.RefundPartial, 50},
		{"doctor cancels late", 30 * time.Minute, "doctor", scheduling.RefundFull, 100},
		{"admin cancels late", 30 * time.Minute, "admin", scheduling.RefundFull, 100},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetTables(t, pool)
			svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)

			doctorID, patientID := uuid.New(), uuid.New()
			slotID := seedPricedSlot(t, pool, doctorID, time.Now().Add(tc.startsIn), 15*time.Minute)
			appt, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: patientID})
			if err != nil {
				t.Fatalf("booking: %v", err)
			}

			actor := patientID
			switch tc.role {
			case "doctor":
				actor = doctorID
			case "admin":
				actor = uuid.New()
			}

			cancelled, err := svc.CancelAppointment(ctx, scheduling.CancelInput{
				AppointmentID: appt.ID, ActorID: actor, ActorRole: tc.role, Reason: "test",
			})
			if err != nil {
				t.Fatalf("cancel: %v", err)
			}
			if cancelled.RefundPolicy == nil || *cancelled.RefundPolicy != tc.wantPolic {
				t.Fatalf("refund policy = %v, want %s", cancelled.RefundPolicy, tc.wantPolic)
			}
			if got := cancelled.RefundPolicy.Percent(); got != tc.wantPct {
				t.Fatalf("refund percent = %d, want %d", got, tc.wantPct)
			}

			// The slot must be back on the market.
			var status string
			if err := pool.QueryRow(ctx, `SELECT status FROM slots WHERE id = $1`, slotID).Scan(&status); err != nil {
				t.Fatalf("read slot: %v", err)
			}
			if status != string(scheduling.SlotAvailable) {
				t.Fatalf("slot status = %s, want AVAILABLE after cancellation", status)
			}

			// And the policy must be on the wire, not merely in the row: the
			// payment service reads this and must not re-derive the window.
			var raw []byte
			if err := pool.QueryRow(ctx,
				`SELECT payload FROM outbox_events WHERE subject = 'appointment.cancelled' LIMIT 1`).
				Scan(&raw); err != nil {
				t.Fatalf("read outbox: %v", err)
			}
			var env struct {
				Payload events.AppointmentCancelled `json:"payload"`
			}
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("decode outbox envelope: %v", err)
			}
			if scheduling.RefundPolicy(env.Payload.RefundPolicy) != tc.wantPolic {
				t.Fatalf("event refund_policy = %s, want %s", env.Payload.RefundPolicy, tc.wantPolic)
			}
			if env.Payload.RefundPercent != tc.wantPct {
				t.Fatalf("event refund_percent = %d, want %d", env.Payload.RefundPercent, tc.wantPct)
			}
			// Instants on the wire are UTC, always.
			if got := env.Payload.StartAt.Location(); got != time.UTC {
				t.Fatalf("event start_at is in %v, want UTC", got)
			}
		})
	}
}
