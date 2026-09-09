package scheduling_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"telemed/internal/domain/scheduling/scheduling"
	"telemed/internal/platform/events"
)

// testClock is a movable clock. It is mutex-guarded because the booking path
// reads it from whichever goroutine is serving the request, and -race is not
// optional on this package.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock { return &testClock{t: time.Now().UTC()} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestServiceWithClock(t *testing.T, pool *pgxpool.Pool, clock scheduling.Clock, queue scheduling.WaitlistQueue) *scheduling.Service {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Colombo")
	if err != nil {
		t.Fatalf("load Asia/Colombo: %v", err)
	}
	return scheduling.NewService(scheduling.Options{
		Pool:       pool,
		Repository: scheduling.NewRepository(),
		Locker:     scheduling.NoopLocker{},
		Queue:      queue,
		Outbox:     events.NewOutbox("telemed-scheduling-service"),
		Clock:      clock,
		Location:   loc,
		Logger:     zerolog.Nop(),
	})
}

func slotState(t *testing.T, pool *pgxpool.Pool, slotID uuid.UUID) (status string, reservedFor *uuid.UUID) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT status, reserved_for FROM slots WHERE id = $1`, slotID).Scan(&status, &reservedFor); err != nil {
		t.Fatalf("read slot: %v", err)
	}
	return status, reservedFor
}

func waitlistState(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) (status string, offerCount int) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT status, offer_count FROM waitlists WHERE id = $1`, id).Scan(&status, &offerCount); err != nil {
		t.Fatalf("read waitlist entry: %v", err)
	}
	return status, offerCount
}

// TestWaitlistPromotionOrdering: a cancellation goes to the longest-waiting
// patient, and only to them.
func TestWaitlistPromotionOrdering(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	clock := newTestClock()
	queue := scheduling.RedisWaitlistQueue{Cache: newMemCache()}
	svc := newTestServiceWithClock(t, pool, clock, queue)

	loc := svc.Location()
	doctorID := uuid.New()
	// A slot tomorrow, so "today" arithmetic cannot make the date ambiguous.
	start := clock.Now().Add(26 * time.Hour)
	slotID := seedPricedSlot(t, pool, doctorID, start, 15*time.Minute)
	date := scheduling.DateIn(start, loc)

	// Three patients join, in order.
	patients := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	entries := make([]scheduling.WaitlistEntry, len(patients))
	for i, p := range patients {
		e, err := svc.JoinWaitlist(ctx, p, doctorID, date)
		if err != nil {
			t.Fatalf("join waitlist %d: %v", i, err)
		}
		entries[i] = e
		// queued_at has microsecond resolution; make the order unambiguous.
		time.Sleep(2 * time.Millisecond)
	}

	// A fourth patient books the slot, then cancels it.
	booker := uuid.New()
	appt, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: booker})
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	if _, err := svc.CancelAppointment(ctx, scheduling.CancelInput{
		AppointmentID: appt.ID, ActorID: booker, ActorRole: "patient", Reason: "test",
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// The first joiner holds the slot; nobody else does.
	status, reservedFor := slotState(t, pool, slotID)
	if status != string(scheduling.SlotBlocked) {
		t.Fatalf("slot status = %s, want BLOCKED while reserved", status)
	}
	if reservedFor == nil || *reservedFor != patients[0] {
		t.Fatalf("slot reserved for %v, want the first joiner %v", reservedFor, patients[0])
	}

	if s, n := waitlistState(t, pool, entries[0].ID); s != string(scheduling.WaitlistNotified) || n != 1 {
		t.Fatalf("first entry is %s with %d offers, want notified with 1", s, n)
	}
	for i := 1; i < len(entries); i++ {
		if s, _ := waitlistState(t, pool, entries[i].ID); s != string(scheduling.WaitlistWaiting) {
			t.Fatalf("entry %d is %s, want still waiting", i, s)
		}
	}

	// A reserved slot is invisible to everyone else's availability search...
	slots, err := svc.ListSlots(ctx, doctorID, date)
	if err != nil {
		t.Fatalf("list slots: %v", err)
	}
	for _, s := range slots {
		if s.ID == slotID {
			t.Fatal("a reserved slot is still being advertised as available")
		}
	}

	// ...and unbookable by them.
	_, err = svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: patients[2]})
	if !errors.Is(err, scheduling.ErrSlotReserved) {
		t.Fatalf("err = %v, want ErrSlotReserved for a patient who is not the holder", err)
	}

	// The holder can take it, and doing so closes their waitlist entry.
	if _, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: patients[0]}); err != nil {
		t.Fatalf("the reservation holder could not book: %v", err)
	}
	if s, _ := waitlistState(t, pool, entries[0].ID); s != string(scheduling.WaitlistBooked) {
		t.Fatalf("first entry is %s after booking, want booked", s)
	}

	// The offer must have been announced.
	var offers int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM outbox_events WHERE subject = 'waitlist.slot_offered'`).Scan(&offers); err != nil {
		t.Fatalf("count offers: %v", err)
	}
	if offers != 1 {
		t.Fatalf("%d waitlist.slot_offered events, want 1", offers)
	}
}

// TestWaitlistOfferExpiryFallsThrough: an ignored offer must not park the slot
// forever, and must not keep going back to the same unresponsive patient.
func TestWaitlistOfferExpiryFallsThrough(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	clock := newTestClock()
	svc := newTestServiceWithClock(t, pool, clock, scheduling.RedisWaitlistQueue{Cache: newMemCache()})
	loc := svc.Location()

	doctorID := uuid.New()
	start := clock.Now().Add(30 * time.Hour)
	slotID := seedPricedSlot(t, pool, doctorID, start, 15*time.Minute)
	date := scheduling.DateIn(start, loc)

	first, err := svc.JoinWaitlist(ctx, uuid.New(), doctorID, date)
	if err != nil {
		t.Fatalf("join first: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	second, err := svc.JoinWaitlist(ctx, uuid.New(), doctorID, date)
	if err != nil {
		t.Fatalf("join second: %v", err)
	}

	booker := uuid.New()
	appt, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: booker})
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	if _, err := svc.CancelAppointment(ctx, scheduling.CancelInput{
		AppointmentID: appt.ID, ActorID: booker, ActorRole: "patient",
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	if _, reservedFor := slotState(t, pool, slotID); reservedFor == nil || *reservedFor != first.PatientID {
		t.Fatalf("slot reserved for %v, want first joiner", reservedFor)
	}

	// Nothing happens before the five minutes are up.
	clock.Advance(scheduling.WaitlistOfferWindow - time.Minute)
	if _, err := svc.SweepExpiredOffers(ctx); err != nil {
		t.Fatalf("early sweep: %v", err)
	}
	if s, _ := waitlistState(t, pool, first.ID); s != string(scheduling.WaitlistNotified) {
		t.Fatalf("first entry is %s before expiry, want still notified", s)
	}

	// Past the window, the offer lapses and falls through to the next patient.
	clock.Advance(2 * time.Minute)
	if _, err := svc.SweepExpiredOffers(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if s, n := waitlistState(t, pool, first.ID); s != string(scheduling.WaitlistWaiting) || n != 1 {
		t.Fatalf("first entry is %s with %d offers after lapsing, want waiting with 1", s, n)
	}
	if s, n := waitlistState(t, pool, second.ID); s != string(scheduling.WaitlistNotified) || n != 1 {
		t.Fatalf("second entry is %s with %d offers, want notified with 1", s, n)
	}

	status, reservedFor := slotState(t, pool, slotID)
	if status != string(scheduling.SlotBlocked) {
		t.Fatalf("slot status = %s, want BLOCKED for the new holder", status)
	}
	if reservedFor == nil || *reservedFor != second.PatientID {
		t.Fatalf("slot reserved for %v, want the second joiner", reservedFor)
	}
}

// TestWaitlistRetiresAfterRepeatedLapses: three ignored offers and the entry is
// retired, so one dormant patient cannot starve the queue five minutes at a time.
func TestWaitlistRetiresAfterRepeatedLapses(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	clock := newTestClock()
	svc := newTestServiceWithClock(t, pool, clock, scheduling.NoopWaitlistQueue{})
	loc := svc.Location()

	doctorID := uuid.New()
	start := clock.Now().Add(40 * time.Hour)
	slotID := seedPricedSlot(t, pool, doctorID, start, 15*time.Minute)
	date := scheduling.DateIn(start, loc)

	only, err := svc.JoinWaitlist(ctx, uuid.New(), doctorID, date)
	if err != nil {
		t.Fatalf("join: %v", err)
	}

	// Free the slot to trigger the first offer.
	booker := uuid.New()
	appt, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: booker})
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	if _, err := svc.CancelAppointment(ctx, scheduling.CancelInput{
		AppointmentID: appt.ID, ActorID: booker, ActorRole: "patient",
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	for i := 1; i <= scheduling.WaitlistMaxOffers; i++ {
		clock.Advance(scheduling.WaitlistOfferWindow + time.Minute)
		if _, err := svc.SweepExpiredOffers(ctx); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}

	status, count := waitlistState(t, pool, only.ID)
	if status != string(scheduling.WaitlistExpired) {
		t.Fatalf("entry is %s after %d lapsed offers, want expired", status, scheduling.WaitlistMaxOffers)
	}
	if count != scheduling.WaitlistMaxOffers {
		t.Fatalf("offer_count = %d, want %d", count, scheduling.WaitlistMaxOffers)
	}

	slotStatus, _ := slotState(t, pool, slotID)
	if slotStatus != string(scheduling.SlotAvailable) {
		t.Fatalf("slot status = %s, want AVAILABLE once the queue is exhausted", slotStatus)
	}
}

// TestWaitlistJoinRules covers the ordinary validation.
func TestWaitlistJoinRules(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()

	t.Run("rejects a date in the past", func(t *testing.T) {
		resetTables(t, pool)
		svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
		yesterday := scheduling.DateIn(time.Now(), svc.Location()).AddDays(-1)
		_, err := svc.JoinWaitlist(ctx, uuid.New(), uuid.New(), yesterday)
		if !errors.Is(err, scheduling.ErrWaitlistDateInPast) {
			t.Fatalf("err = %v, want ErrWaitlistDateInPast", err)
		}
	})

	t.Run("rejects a duplicate join", func(t *testing.T) {
		resetTables(t, pool)
		svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
		patient, doctor := uuid.New(), uuid.New()
		date := scheduling.DateIn(time.Now(), svc.Location()).AddDays(3)

		if _, err := svc.JoinWaitlist(ctx, patient, doctor, date); err != nil {
			t.Fatalf("first join: %v", err)
		}
		_, err := svc.JoinWaitlist(ctx, patient, doctor, date)
		if !errors.Is(err, scheduling.ErrWaitlistDuplicate) {
			t.Fatalf("err = %v, want ErrWaitlistDuplicate", err)
		}
	})

	t.Run("leaving frees the patient to rejoin", func(t *testing.T) {
		resetTables(t, pool)
		svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
		patient, doctor := uuid.New(), uuid.New()
		date := scheduling.DateIn(time.Now(), svc.Location()).AddDays(3)

		entry, err := svc.JoinWaitlist(ctx, patient, doctor, date)
		if err != nil {
			t.Fatalf("join: %v", err)
		}
		if err := svc.LeaveWaitlist(ctx, entry.ID, patient, false); err != nil {
			t.Fatalf("leave: %v", err)
		}
		if _, err := svc.JoinWaitlist(ctx, patient, doctor, date); err != nil {
			t.Fatalf("rejoin after leaving: %v", err)
		}
	})

	t.Run("cannot leave somebody else's entry", func(t *testing.T) {
		resetTables(t, pool)
		svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
		date := scheduling.DateIn(time.Now(), svc.Location()).AddDays(3)
		entry, err := svc.JoinWaitlist(ctx, uuid.New(), uuid.New(), date)
		if err != nil {
			t.Fatalf("join: %v", err)
		}
		err = svc.LeaveWaitlist(ctx, entry.ID, uuid.New(), false)
		if !errors.Is(err, scheduling.ErrWaitlistNotFound) {
			t.Fatalf("err = %v, want ErrWaitlistNotFound", err)
		}
	})
}
