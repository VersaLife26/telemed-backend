package scheduling_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"telemed/internal/domain/scheduling/scheduling"
)

// nextWorkingInstant returns an instant on the given local date at 10:00, which
// is inside every working-hours window these tests seed and comfortably in the
// future.
func holidayInstant(t *testing.T, loc *time.Location, date scheduling.Date, hour int) time.Time {
	t.Helper()
	return time.Date(date.Year, date.Month, date.Day, hour, 0, 0, 0, loc).UTC()
}

// countSlotsByStatus reads the raw slot statuses for one doctor on one local
// day. It goes straight to SQL on purpose: ListSlots filters to AVAILABLE and
// would hide exactly the transition under test.
func countSlotsByStatus(t *testing.T, pool *pgxpool.Pool, doctorID uuid.UUID,
	loc *time.Location, date scheduling.Date,
) map[string]int {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT status, COUNT(*) FROM slots
		WHERE doctor_id = $1 AND start_at >= $2 AND start_at < $3
		GROUP BY status`,
		doctorID, date.StartOfDay(loc).UTC(), date.EndOfDay(loc).UTC())
	if err != nil {
		t.Fatalf("count slots: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			t.Fatalf("scan slot count: %v", err)
		}
		out[status] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate slot counts: %v", err)
	}
	return out
}

func outboxSubjects(t *testing.T, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT subject, COUNT(*) FROM outbox_events GROUP BY subject`)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var subject string
		var n int
		if err := rows.Scan(&subject, &n); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		out[subject] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate outbox: %v", err)
	}
	return out
}

// TestHolidayWithdrawsSlotsAlreadyGenerated is the whole point of the feature.
//
// Registering leave for a day inside the generation window has to act on the
// slots that ALREADY EXIST. Excluding the day from tomorrow's generation run
// changes nothing about the thirty days of rows sitting in the table today, and
// a doctor whose leave leaves those rows bookable is not on leave.
func TestHolidayWithdrawsSlotsAlreadyGenerated(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	loc := svc.Location()
	doctorID := uuid.New()
	seedPricing(t, pool, doctorID)

	leaveDate := scheduling.DateIn(time.Now(), loc).AddDays(3)
	for _, hour := range []int{9, 10, 11} {
		seedSlot(t, pool, doctorID, holidayInstant(t, loc, leaveDate, hour), 30*time.Minute)
	}
	// A slot on the following day must be left completely alone.
	otherDate := leaveDate.AddDays(1)
	seedSlot(t, pool, doctorID, holidayInstant(t, loc, otherDate, 9), 30*time.Minute)

	effect, err := svc.AddHoliday(ctx, scheduling.AddHolidayInput{
		DoctorID:        &doctorID,
		Date:            leaveDate,
		Reason:          "conference",
		ApplyToExisting: true,
	})
	if err != nil {
		t.Fatalf("add holiday: %v", err)
	}
	if effect.SlotsWithdrawn != 3 {
		t.Errorf("withdrew %d slots, want 3", effect.SlotsWithdrawn)
	}
	if effect.AppointmentsCancelled != 0 {
		t.Errorf("cancelled %d appointments, want 0", effect.AppointmentsCancelled)
	}
	if effect.Holiday.ID == uuid.Nil {
		t.Error("holiday response carries no id, so the client cannot delete it")
	}

	byStatus := countSlotsByStatus(t, pool, doctorID, loc, leaveDate)
	if byStatus["CANCELLED"] != 3 || byStatus["AVAILABLE"] != 0 {
		t.Errorf("leave day statuses = %v, want 3 CANCELLED and 0 AVAILABLE", byStatus)
	}
	if other := countSlotsByStatus(t, pool, doctorID, loc, otherDate); other["AVAILABLE"] != 1 {
		t.Errorf("neighbouring day statuses = %v, want 1 AVAILABLE untouched", other)
	}

	// The day must also be invisible to a patient browsing availability.
	slots, err := svc.ListSlots(ctx, doctorID, leaveDate)
	if err != nil {
		t.Fatalf("list slots: %v", err)
	}
	if len(slots) != 0 {
		t.Errorf("%d slots still bookable on a day the doctor is on leave", len(slots))
	}

	// Every withdrawal is announced, or doctor-service's search projection goes
	// on advertising the doctor as available.
	if got := outboxSubjects(t, pool)["slot.withdrawn"]; got != 3 {
		t.Errorf("published %d slot.withdrawn events, want 3", got)
	}
}

// TestHolidayRefusesToSilentlyCancelBookings is the decision this feature turns
// on.
//
// A doctor tapping a date in a picker does not necessarily know four people are
// booked that morning. Registering the leave and leaving those bookings
// standing tells the doctor they are off while patients still expect them.
// Registering it and cancelling silently destroys four consultations with
// nobody deciding to. So the request is refused, the count is reported, and
// nothing at all is written.
func TestHolidayRefusesToSilentlyCancelBookings(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	loc := svc.Location()
	doctorID := uuid.New()
	seedPricing(t, pool, doctorID)

	leaveDate := scheduling.DateIn(time.Now(), loc).AddDays(4)
	booked := seedSlot(t, pool, doctorID, holidayInstant(t, loc, leaveDate, 9), 30*time.Minute)
	free := seedSlot(t, pool, doctorID, holidayInstant(t, loc, leaveDate, 10), 30*time.Minute)

	patientID := uuid.New()
	if _, err := svc.BookSlot(ctx, scheduling.BookSlotInput{
		SlotID: booked, PatientID: patientID,
	}); err != nil {
		t.Fatalf("seed booking: %v", err)
	}

	_, err := svc.AddHoliday(ctx, scheduling.AddHolidayInput{
		DoctorID:        &doctorID,
		Date:            leaveDate,
		Reason:          "family",
		ApplyToExisting: true,
	})
	if !errors.Is(err, scheduling.ErrHolidayHasBookings) {
		t.Fatalf("add holiday over a booking: %v, want ErrHolidayHasBookings", err)
	}

	// Nothing was written. Not the holiday, not the withdrawal of the free
	// slot beside the booked one -- a partial application would be the worst
	// outcome available, because the doctor would be told the save failed while
	// half their day had quietly gone.
	holidays, err := svc.ListDoctorHolidays(ctx, doctorID, leaveDate, leaveDate)
	if err != nil {
		t.Fatalf("list holidays: %v", err)
	}
	if len(holidays) != 0 {
		t.Errorf("%d holidays stored after a refused request, want 0", len(holidays))
	}
	byStatus := countSlotsByStatus(t, pool, doctorID, loc, leaveDate)
	if byStatus["AVAILABLE"] != 1 || byStatus["BOOKED"] != 1 {
		t.Errorf("statuses after a refused request = %v, want 1 AVAILABLE and 1 BOOKED", byStatus)
	}
	_ = free
}

// TestHolidayCancelsBookingsWhenAsked covers the explicit-consent path, and
// asserts the patient is actually told.
//
// The refund is FULL: the platform broke the appointment, so the patient is
// made whole regardless of how much notice there was. Charging a late
// cancellation fee because a doctor took leave would be indefensible.
func TestHolidayCancelsBookingsWhenAsked(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	loc := svc.Location()
	doctorID := uuid.New()
	seedPricing(t, pool, doctorID)

	leaveDate := scheduling.DateIn(time.Now(), loc).AddDays(2)
	bookedSlot := seedSlot(t, pool, doctorID, holidayInstant(t, loc, leaveDate, 9), 30*time.Minute)
	seedSlot(t, pool, doctorID, holidayInstant(t, loc, leaveDate, 10), 30*time.Minute)

	appt, err := svc.BookSlot(ctx, scheduling.BookSlotInput{
		SlotID: bookedSlot, PatientID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("seed booking: %v", err)
	}

	effect, err := svc.AddHoliday(ctx, scheduling.AddHolidayInput{
		DoctorID:        &doctorID,
		Date:            leaveDate,
		Reason:          "family",
		ApplyToExisting: true,
		CancelBooked:    true,
	})
	if err != nil {
		t.Fatalf("add holiday with consent: %v", err)
	}
	if effect.AppointmentsCancelled != 1 {
		t.Errorf("cancelled %d appointments, want 1", effect.AppointmentsCancelled)
	}
	if effect.SlotsWithdrawn != 2 {
		t.Errorf("withdrew %d slots, want 2 (the booked one goes too)", effect.SlotsWithdrawn)
	}

	got, err := svc.GetAppointment(ctx, appt.ID, doctorID, "doctor")
	if err != nil {
		t.Fatalf("reload appointment: %v", err)
	}
	if got.Status != scheduling.AppointmentCancelled {
		t.Errorf("appointment status = %s, want cancelled", got.Status)
	}
	if got.RefundPolicy == nil || *got.RefundPolicy != scheduling.RefundFull {
		t.Errorf("refund policy = %v, want FULL: a patient must not pay for their doctor's leave", got.RefundPolicy)
	}
	// The ROW carries the code and the doctor's own words, because the patient
	// whose appointment vanished is owed an explanation and this is the one
	// place scoped to them. The EVENT carries the code alone -- see
	// TestLeaveReasonStaysOffTheBus.
	if want := scheduling.HolidayLeaveReason + ": family"; got.CancellationReason != want {
		t.Errorf("cancellation reason = %q, want %q", got.CancellationReason, want)
	}

	// The patient has to be TOLD. An appointment that disappears from a phone
	// with no notification is the failure mode this whole path exists to avoid.
	subjects := outboxSubjects(t, pool)
	if subjects["appointment.cancelled"] != 1 {
		t.Errorf("published %d appointment.cancelled events, want 1", subjects["appointment.cancelled"])
	}
	// The withdrawn slot must NOT be announced as released: released means
	// bookable again, and putting it back on the market on the day the doctor
	// is away would undo the entire operation.
	if subjects["slot.released"] != 0 {
		t.Errorf("published %d slot.released events, want 0 -- a withdrawn slot is not released",
			subjects["slot.released"])
	}
	if subjects["slot.withdrawn"] != 2 {
		t.Errorf("published %d slot.withdrawn events, want 2", subjects["slot.withdrawn"])
	}
}

// TestHolidayIsIdempotent: re-registering the same leave must not re-publish an
// event per slot. The doctor app retries on a flaky connection, and a retry
// that fans out twenty-four notifications is a bug the doctor experiences as
// their patients being messaged twice.
func TestHolidayIsIdempotent(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	loc := svc.Location()
	doctorID := uuid.New()

	leaveDate := scheduling.DateIn(time.Now(), loc).AddDays(5)
	seedSlot(t, pool, doctorID, holidayInstant(t, loc, leaveDate, 9), 30*time.Minute)

	in := scheduling.AddHolidayInput{
		DoctorID: &doctorID, Date: leaveDate, Reason: "leave", ApplyToExisting: true,
	}
	if _, err := svc.AddHoliday(ctx, in); err != nil {
		t.Fatalf("first add: %v", err)
	}
	second, err := svc.AddHoliday(ctx, in)
	if err != nil {
		t.Fatalf("second add: %v", err)
	}
	if second.SlotsWithdrawn != 0 {
		t.Errorf("second add withdrew %d slots, want 0", second.SlotsWithdrawn)
	}
	if got := outboxSubjects(t, pool)["slot.withdrawn"]; got != 1 {
		t.Errorf("published %d slot.withdrawn events over two identical requests, want 1", got)
	}

	holidays, err := svc.ListDoctorHolidays(ctx, doctorID, leaveDate, leaveDate)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(holidays) != 1 {
		t.Fatalf("%d holiday rows after two identical requests, want 1", len(holidays))
	}
}

// TestRemoveHolidayRestoresTheDay covers lifting leave.
//
// It is not enough to delete the row and let the nightly generator catch up:
// CopySlots merges ON CONFLICT (doctor_id, start_at) DO NOTHING, so the
// CANCELLED rows would block re-generation of the very slots they occupy and
// the day would stay empty. The withdrawn slots are revived in place.
func TestRemoveHolidayRestoresTheDay(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	loc := svc.Location()
	doctorID := uuid.New()

	leaveDate := scheduling.DateIn(time.Now(), loc).AddDays(6)
	for _, hour := range []int{9, 10} {
		seedSlot(t, pool, doctorID, holidayInstant(t, loc, leaveDate, hour), 30*time.Minute)
	}

	effect, err := svc.AddHoliday(ctx, scheduling.AddHolidayInput{
		DoctorID: &doctorID, Date: leaveDate, Reason: "leave", ApplyToExisting: true,
	})
	if err != nil {
		t.Fatalf("add holiday: %v", err)
	}

	restored, err := svc.RemoveHoliday(ctx, doctorID, effect.Holiday.ID)
	if err != nil {
		t.Fatalf("remove holiday: %v", err)
	}
	if restored.SlotsRestored != 2 {
		t.Errorf("restored %d slots, want 2", restored.SlotsRestored)
	}

	slots, err := svc.ListSlots(ctx, doctorID, leaveDate)
	if err != nil {
		t.Fatalf("list slots: %v", err)
	}
	if len(slots) != 2 {
		t.Errorf("%d bookable slots after lifting leave, want 2", len(slots))
	}
	if got := outboxSubjects(t, pool)["slot.released"]; got != 2 {
		t.Errorf("published %d slot.released events on lift, want 2", got)
	}

	// Re-registering the same date afterwards must work: holidays_uq does not
	// exclude soft-deleted rows, so a plain INSERT would 23505 on a date the
	// doctor sees as free.
	if _, err := svc.AddHoliday(ctx, scheduling.AddHolidayInput{
		DoctorID: &doctorID, Date: leaveDate, Reason: "leave again", ApplyToExisting: true,
	}); err != nil {
		t.Fatalf("re-register lifted leave: %v", err)
	}
}

// TestHolidayOwnershipIsEnforced. A holiday id is a uuid, so guessing one is
// not the threat -- leaking one is. Every failure collapses into "not found".
func TestHolidayOwnershipIsEnforced(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	loc := svc.Location()
	mine, theirs := uuid.New(), uuid.New()

	date := scheduling.DateIn(time.Now(), loc).AddDays(7)
	effect, err := svc.AddHoliday(ctx, scheduling.AddHolidayInput{
		DoctorID: &mine, Date: date, Reason: "leave", ApplyToExisting: true,
	})
	if err != nil {
		t.Fatalf("add holiday: %v", err)
	}

	if _, err := svc.RemoveHoliday(ctx, theirs, effect.Holiday.ID); !errors.Is(err, scheduling.ErrHolidayNotFound) {
		t.Errorf("another doctor deleting my leave: %v, want ErrHolidayNotFound", err)
	}

	// Nor may a doctor delete a platform-wide closure.
	platform, err := svc.SetHoliday(ctx, nil, date.AddDays(1), "Poya", false, false)
	if err != nil {
		t.Fatalf("set platform holiday: %v", err)
	}
	if _, err := svc.RemoveHoliday(ctx, mine, platform.Holiday.ID); !errors.Is(err, scheduling.ErrHolidayNotFound) {
		t.Errorf("doctor deleting a platform-wide closure: %v, want ErrHolidayNotFound", err)
	}
}

// TestPlatformHolidayLeavesExistingSlotsAlone pins the deliberate asymmetry.
//
// A national holiday is loaded for every doctor at once. Withdrawing every
// calendar on the platform from a single POST is a mass cancellation that needs
// an operational procedure with a rollback plan, so the platform-wide path
// stays generation-only no matter what the request asks for.
func TestPlatformHolidayLeavesExistingSlotsAlone(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	loc := svc.Location()
	doctorID := uuid.New()

	date := scheduling.DateIn(time.Now(), loc).AddDays(2)
	seedSlot(t, pool, doctorID, holidayInstant(t, loc, date, 9), 30*time.Minute)

	// Even asking for it explicitly does not withdraw anything.
	if _, err := svc.SetHoliday(ctx, nil, date, "Poya", true, true); err != nil {
		t.Fatalf("set platform holiday: %v", err)
	}
	if got := countSlotsByStatus(t, pool, doctorID, loc, date); got["AVAILABLE"] != 1 {
		t.Errorf("statuses after a platform-wide holiday = %v, want the existing slot untouched", got)
	}

	// It still hides the day from every future generation run, which is the
	// behaviour this endpoint has always had.
	holidays, err := svc.ListDoctorHolidays(ctx, doctorID, date, date)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(holidays) != 1 || !holidays[0].PlatformWide() {
		t.Fatalf("holidays = %+v, want one platform-wide entry", holidays)
	}
}

// TestHolidayRejectsThePast. Leave for a day gone by changes nothing that can
// still be booked and only pollutes the generator's exclusion set.
func TestHolidayRejectsThePast(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	doctorID := uuid.New()
	yesterday := scheduling.DateIn(time.Now(), svc.Location()).AddDays(-1)

	_, err := svc.AddHoliday(ctx, scheduling.AddHolidayInput{
		DoctorID: &doctorID, Date: yesterday, ApplyToExisting: true,
	})
	if !errors.Is(err, scheduling.ErrHolidayDateInPast) {
		t.Fatalf("holiday in the past: %v, want ErrHolidayDateInPast", err)
	}
}

// TestHolidayRaceWithBooking is the concurrency claim.
//
// Twenty patients race one holiday registration for the same slot. The two
// outcomes are both acceptable; the third is not:
//
//   - the booking commits first, the holiday sees it under lock and refuses
//     (or cancels it, with consent); or
//   - the holiday commits first and the booking fails with a clean domain
//     error.
//
// What must never happen is a patient holding a confirmed appointment in a slot
// the database records as CANCELLED. That is the invariant asserted here, and
// it is the reason AddHoliday locks appointments before slots and re-reads the
// appointment set after the slot locks are granted.
func TestHolidayRaceWithBooking(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	loc := svc.Location()
	doctorID := uuid.New()
	seedPricing(t, pool, doctorID)

	leaveDate := scheduling.DateIn(time.Now(), loc).AddDays(3)
	slotID := seedSlot(t, pool, doctorID, holidayInstant(t, loc, leaveDate, 9), 30*time.Minute)

	const bookers = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(bookers + 1)

	var mu sync.Mutex
	var booked int

	for i := 0; i < bookers; i++ {
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.BookSlot(ctx, scheduling.BookSlotInput{
				SlotID: slotID, PatientID: uuid.New(),
			})
			if err == nil {
				mu.Lock()
				booked++
				mu.Unlock()
			}
		}()
	}

	go func() {
		defer wg.Done()
		<-start
		// Retried, because losing the re-read race is a legitimate abort that
		// converges: the second attempt sees the appointment that beat it.
		for attempt := 0; attempt < 10; attempt++ {
			_, err := svc.AddHoliday(ctx, scheduling.AddHolidayInput{
				DoctorID: &doctorID, Date: leaveDate, Reason: "leave",
				ApplyToExisting: true, CancelBooked: true,
			})
			if err == nil || errors.Is(err, scheduling.ErrHolidayHasBookings) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	close(start)
	wg.Wait()

	// The invariant: never a live appointment against a withdrawn slot.
	var orphans int
	err := pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM appointments a
		JOIN slots s ON s.id = a.slot_id
		WHERE a.status IN ('pending_payment', 'confirmed')
		  AND s.status = 'CANCELLED'`).Scan(&orphans)
	if err != nil {
		t.Fatalf("check orphans: %v", err)
	}
	if orphans != 0 {
		t.Fatalf("%d live appointments are held against a withdrawn slot", orphans)
	}

	// And the booking invariant the platform rests on still holds: at most one
	// of twenty racers got the slot.
	if booked > 1 {
		t.Fatalf("%d patients booked the same slot", booked)
	}
}

// TestCountLiveAppointmentsOn backs the preflight the client shows before it
// asks the doctor to confirm cancelling patients.
func TestCountLiveAppointmentsOn(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	loc := svc.Location()
	doctorID := uuid.New()
	seedPricing(t, pool, doctorID)

	date := scheduling.DateIn(time.Now(), loc).AddDays(2)
	for _, hour := range []int{9, 10} {
		id := seedSlot(t, pool, doctorID, holidayInstant(t, loc, date, hour), 30*time.Minute)
		if _, err := svc.BookSlot(ctx, scheduling.BookSlotInput{
			SlotID: id, PatientID: uuid.New(),
		}); err != nil {
			t.Fatalf("seed booking: %v", err)
		}
	}

	n, err := svc.CountLiveAppointmentsOn(ctx, doctorID, date)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
	if n, err = svc.CountLiveAppointmentsOn(ctx, doctorID, date.AddDays(1)); err != nil || n != 0 {
		t.Errorf("count on a free day = %d (%v), want 0", n, err)
	}
}
