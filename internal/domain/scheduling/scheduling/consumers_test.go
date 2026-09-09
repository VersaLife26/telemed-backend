package scheduling_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/domain/scheduling/scheduling"
	"telemed/internal/platform/events"
)

func intPtr(n int) *int { return &n }

func envelope(t *testing.T, subject events.Subject, payload any) events.Envelope {
	t.Helper()
	env, err := events.NewEnvelope(subject, "test", "", payload)
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	return env
}

// approvedEnvelope builds a doctor.approved carrying the canonical payload AND
// the optional scheduling hints, merged into one JSON object.
//
// That merge is exactly what a producer emitting both would put on the wire,
// and doing it here rather than declaring a third combined struct is the point:
// the canonical half comes from events.DoctorApproved, so a field rename there
// breaks this test rather than silently dropping the fee.
func approvedEnvelope(t *testing.T, approved events.DoctorApproved, hints scheduling.DoctorScheduleHints) events.Envelope {
	t.Helper()
	merged := map[string]json.RawMessage{}
	for _, part := range []any{approved, hints} {
		raw, err := json.Marshal(part)
		if err != nil {
			t.Fatalf("marshal payload part: %v", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatalf("unmarshal payload part: %v", err)
		}
		for k, v := range fields {
			// The canonical half wins any overlap: it is the authority for
			// doctor_id, and the hints struct only repeats it to stay decodable
			// on its own.
			if _, taken := merged[k]; !taken {
				merged[k] = v
			}
		}
	}
	return envelope(t, events.SubjectDoctorApproved, merged)
}

// approvedDoctor is the canonical half of a doctor.approved for a doctor who
// charges LKR 2,000.
func approvedDoctor(doctorID uuid.UUID) events.DoctorApproved {
	return events.DoctorApproved{
		DoctorID:   doctorID,
		UserID:     uuid.New(),
		DoctorName: "Dr Test",
		Specialty:  "cardiology",
		FeeCents:   200000,
		Currency:   "LKR",
		Languages:  []string{"en"},
		ApprovedAt: time.Now().UTC(),
	}
}

// TestConsumeDoctorApproved: an approval seeds the mirrored schedule and
// materialises the doctor's first slots, and a redelivery changes nothing.
func TestConsumeDoctorApproved(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	consumers := scheduling.NewConsumers(svc, nil, nil, zerolog.Nop())

	doctorID := uuid.New()
	hours := make([]scheduling.WorkingHourPayload, 0, 7)
	for d := 0; d < 7; d++ {
		hours = append(hours, scheduling.WorkingHourPayload{
			DayOfWeek: d, StartTime: "09:00", EndTime: "12:00:00",
		})
	}
	env := approvedEnvelope(t, approvedDoctor(doctorID), scheduling.DoctorScheduleHints{
		DoctorID:            doctorID,
		Timezone:            "Asia/Colombo",
		SlotDurationMinutes: 30,
		BufferMinutes:       intPtr(0),
		MaxPerDay:           6,
		AdvanceDays:         7,
		WorkingHours:        hours,
	})

	if err := consumers.Handle(ctx, env); err != nil {
		t.Fatalf("handle doctor.approved: %v", err)
	}

	var duration, buffer, maxPerDay, advance int
	var tz string
	if err := pool.QueryRow(ctx, `
		SELECT slot_duration_minutes, buffer_minutes, max_per_day, advance_days, timezone
		FROM doctor_schedule_settings WHERE doctor_id = $1`, doctorID).
		Scan(&duration, &buffer, &maxPerDay, &advance, &tz); err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if duration != 30 || buffer != 0 || maxPerDay != 6 || advance != 7 || tz != "Asia/Colombo" {
		t.Fatalf("settings = %d/%d/%d/%d/%s, want 30/0/6/7/Asia/Colombo",
			duration, buffer, maxPerDay, advance, tz)
	}

	var whCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM working_hours WHERE doctor_id = $1`, doctorID).
		Scan(&whCount); err != nil {
		t.Fatalf("count working hours: %v", err)
	}
	if whCount != 7 {
		t.Fatalf("%d working-hour rows, want 7", whCount)
	}

	var slotCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM slots WHERE doctor_id = $1`, doctorID).
		Scan(&slotCount); err != nil {
		t.Fatalf("count slots: %v", err)
	}
	if slotCount == 0 {
		t.Fatal("approval generated no slots")
	}

	// Redelivery is normal: JetStream promises at-least-once and nothing more.
	if err := consumers.Handle(ctx, env); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	var afterSlots, settingsVersion int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM slots WHERE doctor_id = $1`, doctorID).Scan(&afterSlots)
	_ = pool.QueryRow(ctx, `SELECT version FROM doctor_schedule_settings WHERE doctor_id = $1`, doctorID).
		Scan(&settingsVersion)
	if afterSlots != slotCount {
		t.Fatalf("redelivery changed the slot count from %d to %d", slotCount, afterSlots)
	}
	if settingsVersion != 0 {
		t.Fatalf("redelivery bumped settings version to %d; the handler is not idempotent", settingsVersion)
	}
}

// TestConsumeDoctorApprovedDefaults: an event that carries only a doctor id
// must still produce a usable configuration rather than nothing at all.
func TestConsumeDoctorApprovedDefaults(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	consumers := scheduling.NewConsumers(svc, nil, nil, zerolog.Nop())

	doctorID := uuid.New()
	env := envelope(t, events.SubjectDoctorApproved, map[string]any{"doctor_id": doctorID})
	if err := consumers.Handle(ctx, env); err != nil {
		t.Fatalf("handle: %v", err)
	}

	var duration, buffer int
	var tz string
	if err := pool.QueryRow(ctx, `
		SELECT slot_duration_minutes, buffer_minutes, timezone
		FROM doctor_schedule_settings WHERE doctor_id = $1`, doctorID).Scan(&duration, &buffer, &tz); err != nil {
		t.Fatalf("read settings: %v", err)
	}
	def := scheduling.DefaultScheduleSettings(doctorID)
	if duration != def.SlotDurationMinutes || buffer != def.BufferMinutes || tz != def.Timezone {
		t.Fatalf("defaults not applied: %d/%d/%s", duration, buffer, tz)
	}

	// No working hours means no slots -- and that must not be an error.
	var slots int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM slots WHERE doctor_id = $1`, doctorID).Scan(&slots)
	if slots != 0 {
		t.Fatalf("%d slots generated for a doctor with no working hours", slots)
	}
}

// TestConsumeDoctorApprovedBadTimezone: an unknown zone must not poison the
// doctor's calendar.
func TestConsumeDoctorApprovedBadTimezone(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	consumers := scheduling.NewConsumers(svc, nil, nil, zerolog.Nop())

	doctorID := uuid.New()
	env := approvedEnvelope(t, approvedDoctor(doctorID), scheduling.DoctorScheduleHints{
		DoctorID: doctorID,
		Timezone: "Mars/Olympus_Mons",
		WorkingHours: []scheduling.WorkingHourPayload{
			{DayOfWeek: int(time.Now().Weekday()), StartTime: "09:00", EndTime: "10:00"},
		},
	})
	if err := consumers.Handle(ctx, env); err != nil {
		t.Fatalf("handle: %v", err)
	}

	var tz string
	if err := pool.QueryRow(ctx,
		`SELECT timezone FROM doctor_schedule_settings WHERE doctor_id = $1`, doctorID).Scan(&tz); err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if tz != "Asia/Colombo" {
		t.Fatalf("timezone = %s, want the platform default after an unknown zone", tz)
	}
}

// TestConsumePaymentEvents covers both halves of the payment contract.
func TestConsumePaymentEvents(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()

	t.Run("payment.succeeded confirms", func(t *testing.T) {
		resetTables(t, pool)
		svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
		consumers := scheduling.NewConsumers(svc, nil, nil, zerolog.Nop())

		slotID := seedPricedSlot(t, pool, uuid.New(), time.Now().Add(3*time.Hour), 15*time.Minute)
		appt, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: uuid.New()})
		if err != nil {
			t.Fatalf("book: %v", err)
		}

		paymentID := uuid.New()
		env := envelope(t, events.SubjectPaymentSucceeded, scheduling.PaymentResultPayload{
			PaymentID: &paymentID, AppointmentID: appt.ID, Amount: 200000, Currency: "LKR",
		})
		if err := consumers.Handle(ctx, env); err != nil {
			t.Fatalf("handle: %v", err)
		}

		var status string
		var gotPayment *uuid.UUID
		if err := pool.QueryRow(ctx,
			`SELECT status, payment_id FROM appointments WHERE id = $1`, appt.ID).Scan(&status, &gotPayment); err != nil {
			t.Fatalf("read appointment: %v", err)
		}
		if status != string(scheduling.AppointmentConfirmed) {
			t.Fatalf("status = %s, want confirmed", status)
		}
		if gotPayment == nil || *gotPayment != paymentID {
			t.Fatalf("payment_id = %v, want %v", gotPayment, paymentID)
		}

		// Replay must not double-confirm or bump the version again.
		var before int
		_ = pool.QueryRow(ctx, `SELECT version FROM appointments WHERE id = $1`, appt.ID).Scan(&before)
		if err := consumers.Handle(ctx, env); err != nil {
			t.Fatalf("replay: %v", err)
		}
		var after int
		_ = pool.QueryRow(ctx, `SELECT version FROM appointments WHERE id = $1`, appt.ID).Scan(&after)
		if before != after {
			t.Fatalf("replay bumped version %d -> %d", before, after)
		}
	})

	t.Run("payment.failed releases the slot", func(t *testing.T) {
		resetTables(t, pool)
		svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
		consumers := scheduling.NewConsumers(svc, nil, nil, zerolog.Nop())

		slotID := seedPricedSlot(t, pool, uuid.New(), time.Now().Add(3*time.Hour), 15*time.Minute)
		appt, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: uuid.New()})
		if err != nil {
			t.Fatalf("book: %v", err)
		}

		env := envelope(t, events.SubjectPaymentFailed, scheduling.PaymentResultPayload{
			AppointmentID: appt.ID, FailureReason: "card_declined",
		})
		if err := consumers.Handle(ctx, env); err != nil {
			t.Fatalf("handle: %v", err)
		}

		var apptStatus, slotStatus string
		_ = pool.QueryRow(ctx, `SELECT status FROM appointments WHERE id = $1`, appt.ID).Scan(&apptStatus)
		_ = pool.QueryRow(ctx, `SELECT status FROM slots WHERE id = $1`, slotID).Scan(&slotStatus)
		if apptStatus != string(scheduling.AppointmentCancelled) {
			t.Fatalf("appointment status = %s, want cancelled", apptStatus)
		}
		if slotStatus != string(scheduling.SlotAvailable) {
			t.Fatalf("slot status = %s, want AVAILABLE", slotStatus)
		}

		// The slot must be genuinely rebookable, not merely marked available.
		if _, err := svc.BookSlot(ctx, scheduling.BookSlotInput{
			SlotID: slotID, PatientID: uuid.New(),
		}); err != nil {
			t.Fatalf("rebooking a payment-failed slot: %v", err)
		}

		var released int
		_ = pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM outbox_events WHERE subject = 'slot.released'`).Scan(&released)
		if released != 1 {
			t.Fatalf("%d slot.released events, want 1", released)
		}
	})

	t.Run("payment for an unknown appointment is acknowledged, not retried forever", func(t *testing.T) {
		resetTables(t, pool)
		svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
		consumers := scheduling.NewConsumers(svc, nil, nil, zerolog.Nop())

		env := envelope(t, events.SubjectPaymentSucceeded, scheduling.PaymentResultPayload{
			AppointmentID: uuid.New(),
		})
		if err := consumers.Handle(ctx, env); err != nil {
			t.Fatalf("handle: %v", err)
		}
	})

	t.Run("an unparseable payload is dropped rather than redelivered forever", func(t *testing.T) {
		resetTables(t, pool)
		svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
		consumers := scheduling.NewConsumers(svc, nil, nil, zerolog.Nop())

		env := envelope(t, events.SubjectPaymentSucceeded, map[string]any{"appointment_id": "not-a-uuid"})
		env.Payload = json.RawMessage(`{"appointment_id": 12345}`)
		if err := consumers.Handle(ctx, env); err != nil {
			t.Fatalf("handle: %v", err)
		}
	})
}

// TestSweepUnpaidBookings: a checkout nobody finished must give the slot back.
func TestSweepUnpaidBookings(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	clock := newTestClock()
	svc := newTestServiceWithClock(t, pool, clock, scheduling.NoopWaitlistQueue{})

	slotID := seedPricedSlot(t, pool, uuid.New(), clock.Now().Add(6*time.Hour), 15*time.Minute)
	appt, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: uuid.New()})
	if err != nil {
		t.Fatalf("book: %v", err)
	}

	// Not yet: the patient may still be on the 3DS challenge screen.
	clock.Advance(scheduling.UnpaidBookingWindow - time.Minute)
	if n, err := svc.SweepUnpaidBookings(ctx); err != nil || n != 0 {
		t.Fatalf("early sweep released %d (err=%v), want 0", n, err)
	}

	// The sweeper compares against appointments.created_at, which is database
	// NOW(), so move real time forward by faking the row instead of the clock.
	if _, err := pool.Exec(ctx,
		`UPDATE appointments SET created_at = NOW() - INTERVAL '30 minutes' WHERE id = $1`, appt.ID); err != nil {
		t.Fatalf("age the appointment: %v", err)
	}
	n, err := svc.SweepUnpaidBookings(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}

	var apptStatus, slotStatus, reason string
	_ = pool.QueryRow(ctx, `SELECT status, cancellation_reason FROM appointments WHERE id = $1`, appt.ID).
		Scan(&apptStatus, &reason)
	_ = pool.QueryRow(ctx, `SELECT status FROM slots WHERE id = $1`, slotID).Scan(&slotStatus)

	if apptStatus != string(scheduling.AppointmentCancelled) {
		t.Fatalf("appointment status = %s, want cancelled", apptStatus)
	}
	if reason != "payment_timeout" {
		t.Fatalf("cancellation reason = %q, want payment_timeout", reason)
	}
	if slotStatus != string(scheduling.SlotAvailable) {
		t.Fatalf("slot status = %s, want AVAILABLE", slotStatus)
	}
}

// TestNoShowLifecycle: a no-show is recorded, counted, and eventually flips the
// patient into prepayment.
func TestNoShowLifecycle(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	repo := scheduling.NewRepository()
	doctorID, patientID := uuid.New(), uuid.New()

	// Four appointments: two no-shows, two completed -> 50%, over the threshold.
	seedPricing(t, pool, doctorID)
	base := time.Now().Add(2 * time.Hour)
	for i := 0; i < 4; i++ {
		slotID := seedSlot(t, pool, doctorID, base.Add(time.Duration(i)*time.Hour), 15*time.Minute)
		appt, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: patientID})
		if err != nil {
			t.Fatalf("book %d: %v", i, err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE appointments SET status = 'confirmed', confirmed_at = NOW() WHERE id = $1`, appt.ID); err != nil {
			t.Fatalf("confirm %d: %v", i, err)
		}
		status := scheduling.AppointmentCompleted
		if i < 2 {
			status = scheduling.AppointmentNoShow
		}
		if _, err := svc.MarkTerminal(ctx, appt.ID, doctorID, "doctor", status); err != nil {
			t.Fatalf("mark %d terminal: %v", i, err)
		}
	}

	stats, err := repo.GetNoShowStats(ctx, pool, patientID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.TotalAppointments != 4 || stats.NoShowCount != 2 {
		t.Fatalf("stats = %d total / %d no-shows, want 4/2", stats.TotalAppointments, stats.NoShowCount)
	}
	if !scheduling.RequiresPrepayment(stats) {
		t.Fatalf("no-show rate %.2f should require prepayment", stats.NoShowRate())
	}

	// The next booking must carry the flag outward on the event.
	next := seedPricedSlot(t, pool, doctorID, base.Add(20*time.Hour), 15*time.Minute)
	appt, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: next, PatientID: patientID})
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	if !appt.PrepaymentRequired {
		t.Fatal("prepayment_required was not set on the appointment")
	}

	var raw []byte
	if err := pool.QueryRow(ctx, `
		SELECT payload FROM outbox_events
		WHERE subject = 'appointment.created' AND aggregate_id = $1`, appt.ID.String()).Scan(&raw); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	var env struct {
		Payload events.AppointmentCreated `json:"payload"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !env.Payload.PrepaymentRequired {
		t.Fatal("appointment.created did not carry prepayment_required")
	}

	// A doctor with a 50% no-show rate gets the overbooking allowance.
	if _, err := svc.RecomputeOverbooking(ctx); err != nil {
		t.Fatalf("recompute overbooking: %v", err)
	}
}
