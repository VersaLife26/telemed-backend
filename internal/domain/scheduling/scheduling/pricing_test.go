package scheduling_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"telemed/internal/domain/scheduling/scheduling"
	"telemed/internal/platform/events"
)

// This file is the proof for INTEGRATION-FIXES #10: no patient could pay.
//
// The defect was that doctor-service published doctor.approved without a fee,
// this service had no price column at all, and it therefore published
// appointment.created without amount_cents -- which payment-service requires.
// Every booking on the platform was created, held a slot, and was then refused
// by payment with a log line nobody was watching.
//
// The tests below assert the three joints of that chain, in order:
//
//	1. doctor.approved carrying a fee populates the pricing projection.
//	2. Booking a priced doctor stamps the quote and puts it on the wire.
//	3. Booking an UNPRICED doctor fails, loudly, at booking time.
//
// (3) is the one that matters most. A booking that quietly defaults to zero
// recreates the original defect exactly.

// TestDoctorApprovedSeedsPricing is joint 1.
func TestDoctorApprovedSeedsPricing(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	consumers := scheduling.NewConsumers(svc, nil, nil, zerolog.Nop())

	doctorID := uuid.New()
	approved := events.DoctorApproved{
		DoctorID:   doctorID,
		UserID:     uuid.New(),
		DoctorName: "Dr Anula Perera",
		Specialty:  "cardiology",
		FeeCents:   250000,
		Currency:   "LKR",
		Languages:  []string{"en", "si"},
		ApprovedAt: time.Now().UTC(),
	}
	if err := consumers.Handle(ctx, envelope(t, events.SubjectDoctorApproved, approved)); err != nil {
		t.Fatalf("handle doctor.approved: %v", err)
	}

	got := readPricing(t, pool, doctorID)
	if got.FeeCents != 250000 {
		t.Errorf("fee_cents = %d, want 250000", got.FeeCents)
	}
	if got.Currency != "LKR" {
		t.Errorf("currency = %q, want LKR", got.Currency)
	}
	if got.Specialty != "cardiology" {
		t.Errorf("specialty = %q, want cardiology", got.Specialty)
	}
	if got.Status != "approved" {
		t.Errorf("status = %q, want approved", got.Status)
	}
}

// TestDoctorUpdatedRepricesFutureBookingsOnly is the locked-quote design, made
// executable.
//
// A doctor raises their fee between two bookings. The first patient keeps the
// price they were quoted; the second is quoted the new one. If this test ever
// fails in the direction of "both appointments show the new price", the
// platform is charging people a number they never agreed to.
func TestDoctorUpdatedRepricesFutureBookingsOnly(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	consumers := scheduling.NewConsumers(svc, nil, nil, zerolog.Nop())
	doctorID := uuid.New()

	approvedAt := time.Now().UTC().Add(-time.Hour)
	if err := consumers.Handle(ctx, envelope(t, events.SubjectDoctorApproved, events.DoctorApproved{
		DoctorID: doctorID, UserID: uuid.New(), DoctorName: "Dr Fee",
		Specialty: "general", FeeCents: 200000, Currency: "LKR", ApprovedAt: approvedAt,
	})); err != nil {
		t.Fatalf("approve: %v", err)
	}

	slotA := seedSlot(t, pool, doctorID, time.Now().Add(3*time.Hour), 15*time.Minute)
	first, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotA, PatientID: uuid.New()})
	if err != nil {
		t.Fatalf("first booking: %v", err)
	}
	if first.AmountCents != 200000 {
		t.Fatalf("first booking quoted %d, want 200000", first.AmountCents)
	}

	// The doctor puts their price up.
	if err := consumers.Handle(ctx, envelope(t, events.SubjectDoctorUpdated, events.DoctorUpdated{
		DoctorID: doctorID, Specialty: "general", FeeCents: 350000, Currency: "LKR",
		Status: "approved", UpdatedAt: time.Now().UTC(),
	})); err != nil {
		t.Fatalf("update: %v", err)
	}

	slotB := seedSlot(t, pool, doctorID, time.Now().Add(4*time.Hour), 15*time.Minute)
	second, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotB, PatientID: uuid.New()})
	if err != nil {
		t.Fatalf("second booking: %v", err)
	}
	if second.AmountCents != 350000 {
		t.Fatalf("second booking quoted %d, want the new price 350000", second.AmountCents)
	}

	// And the first appointment's stored quote must not have moved.
	var stored int64
	if err := pool.QueryRow(ctx,
		`SELECT amount_cents FROM appointments WHERE id = $1`, first.ID).Scan(&stored); err != nil {
		t.Fatalf("re-read first appointment: %v", err)
	}
	if stored != 200000 {
		t.Fatalf("the first patient's quote moved to %d: they booked at 200000 and "+
			"that is what they agreed to pay", stored)
	}
}

// TestDoctorPricingIgnoresStaleEvents is the out-of-order guard.
//
// JetStream is at-least-once with no ordering guarantee across redeliveries, so
// a doctor.approved from an hour ago can genuinely arrive after the
// doctor.updated that superseded it. Without last_event_at that redelivery
// rolls the fee back and every subsequent booking quotes the wrong number.
func TestDoctorPricingIgnoresStaleEvents(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	consumers := scheduling.NewConsumers(svc, nil, nil, zerolog.Nop())
	doctorID := uuid.New()

	old := time.Now().UTC().Add(-2 * time.Hour)
	recent := time.Now().UTC()

	// Newest first.
	if err := consumers.Handle(ctx, envelope(t, events.SubjectDoctorUpdated, events.DoctorUpdated{
		DoctorID: doctorID, Specialty: "general", FeeCents: 350000, Currency: "LKR",
		Status: "approved", UpdatedAt: recent,
	})); err != nil {
		t.Fatalf("recent update: %v", err)
	}

	// Now the stale redelivery.
	if err := consumers.Handle(ctx, envelope(t, events.SubjectDoctorUpdated, events.DoctorUpdated{
		DoctorID: doctorID, Specialty: "general", FeeCents: 200000, Currency: "LKR",
		Status: "approved", UpdatedAt: old,
	})); err != nil {
		t.Fatalf("stale update: %v", err)
	}

	if got := readPricing(t, pool, doctorID).FeeCents; got != 350000 {
		t.Fatalf("fee_cents = %d after a stale redelivery, want 350000: an older "+
			"event rolled the price back", got)
	}

	// An exact redelivery of the newest event is also a no-op, not a churn.
	if err := consumers.Handle(ctx, envelope(t, events.SubjectDoctorUpdated, events.DoctorUpdated{
		DoctorID: doctorID, Specialty: "general", FeeCents: 350000, Currency: "LKR",
		Status: "approved", UpdatedAt: recent,
	})); err != nil {
		t.Fatalf("duplicate update: %v", err)
	}
	if got := readPricing(t, pool, doctorID).FeeCents; got != 350000 {
		t.Fatalf("fee_cents = %d after a duplicate, want 350000", got)
	}
}

// TestBookingProducesAPayableEvent is joint 2, and it is the headline.
//
// It asserts the exact bytes that leave this service: the outbox row for
// appointment.created must decode into events.AppointmentCreated -- the struct
// payment-service consumes -- with a non-zero amount, the right currency and
// the right specialty.
func TestBookingProducesAPayableEvent(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	consumers := scheduling.NewConsumers(svc, nil, nil, zerolog.Nop())

	doctorID, patientID := uuid.New(), uuid.New()
	if err := consumers.Handle(ctx, envelope(t, events.SubjectDoctorApproved, events.DoctorApproved{
		DoctorID: doctorID, UserID: uuid.New(), DoctorName: "Dr Payable",
		Specialty: "dermatology", FeeCents: 275000, Currency: "LKR",
		ApprovedAt: time.Now().UTC(),
	})); err != nil {
		t.Fatalf("approve: %v", err)
	}

	slotID := seedSlot(t, pool, doctorID, time.Now().Add(3*time.Hour), 15*time.Minute)
	appt, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: patientID})
	if err != nil {
		t.Fatalf("book: %v", err)
	}

	payload := decodeAppointmentCreated(t, pool, appt.ID)

	// The assertion the whole reconciliation exists for.
	if payload.AmountCents <= 0 {
		t.Fatalf("appointment.created carries amount_cents=%d. payment-service "+
			"refuses a zero amount as unpayable, so this booking would be "+
			"silently discarded and the patient could never pay",
			payload.AmountCents)
	}
	if payload.AmountCents != 275000 {
		t.Errorf("amount_cents = %d, want 275000", payload.AmountCents)
	}
	if payload.Currency != "LKR" {
		t.Errorf("currency = %q, want LKR", payload.Currency)
	}
	if payload.Specialty != "dermatology" {
		t.Errorf("specialty = %q, want dermatology: payment prices its commission off it", payload.Specialty)
	}

	// Everything else payment-service reads must also be populated.
	switch {
	case payload.AppointmentID != appt.ID:
		t.Errorf("appointment_id = %s, want %s", payload.AppointmentID, appt.ID)
	case payload.PatientID != patientID:
		t.Errorf("patient_id = %s, want %s", payload.PatientID, patientID)
	case payload.DoctorID != doctorID:
		t.Errorf("doctor_id = %s, want %s", payload.DoctorID, doctorID)
	case payload.SlotID != slotID:
		t.Errorf("slot_id = %s, want %s", payload.SlotID, slotID)
	case payload.StartAt.IsZero():
		t.Error("start_at is zero")
	case payload.ExpiresAt.IsZero():
		t.Error("expires_at is zero: payment sizes its checkout timeout from it")
	case payload.CreatedAt.IsZero():
		t.Error("created_at is zero")
	case payload.Status == "":
		t.Error("status is empty")
	}
}

// TestBookingWithoutPricingFailsLoudly is joint 3, and it is the guard rail.
//
// A doctor with no pricing row must not be bookable. Not "bookable at zero",
// not "bookable and reconciled later" -- refused, at the moment the patient
// tries, with an error that says why. Anything else recreates the defect:
// the slot is held, the patient believes they have an appointment, and the
// payment never happens.
func TestBookingWithoutPricingFailsLoudly(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()
	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)

	t.Run("no pricing row at all", func(t *testing.T) {
		resetTables(t, pool)
		doctorID := uuid.New()
		slotID := seedSlot(t, pool, doctorID, time.Now().Add(3*time.Hour), 15*time.Minute)

		_, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: uuid.New()})
		if !errors.Is(err, scheduling.ErrDoctorNotPriced) {
			t.Fatalf("err = %v, want ErrDoctorNotPriced", err)
		}
		assertNothingHappened(t, pool, slotID)
	})

	t.Run("a pricing row with a zero fee", func(t *testing.T) {
		resetTables(t, pool)
		doctorID := uuid.New()
		// A zero fee is a projection that never got a real price. It is not a
		// free consultation, and treating it as one is the bug.
		seedPricingAt(t, pool, doctorID, 0, "LKR", "general", time.Now().UTC())
		slotID := seedSlot(t, pool, doctorID, time.Now().Add(3*time.Hour), 15*time.Minute)

		_, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: uuid.New()})
		if !errors.Is(err, scheduling.ErrDoctorNotPriced) {
			t.Fatalf("err = %v, want ErrDoctorNotPriced", err)
		}
		assertNothingHappened(t, pool, slotID)
	})

	t.Run("the refusal is a clean 422, not a 500", func(t *testing.T) {
		apiErr := scheduling.APIError(scheduling.ErrDoctorNotPriced)
		if apiErr == nil {
			t.Fatal("ErrDoctorNotPriced mapped to no API error")
		}
		msg := apiErr.Error()
		if strings.Contains(msg, "INTERNAL_ERROR") {
			t.Fatalf("ErrDoctorNotPriced became a 500: %s", msg)
		}
		if !strings.Contains(msg, "UNPROCESSABLE") {
			t.Fatalf("ErrDoctorNotPriced mapped to %s, want UNPROCESSABLE", msg)
		}
	})
}

// assertNothingHappened checks that a refused booking left no trace: the slot
// is still on the market, no appointment row exists, and -- critically -- no
// appointment.created was enqueued.
//
// The last one is the important assertion. A refusal that rolled back the
// appointment but still emitted the event would hand payment-service the
// unpayable booking anyway, which is the defect wearing a different hat. The
// enqueue is inside the same transaction (ADR-005), so it cannot happen -- and
// this asserts that rather than assuming it.
func assertNothingHappened(t *testing.T, pool *pgxpool.Pool, slotID uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	var slotStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM slots WHERE id = $1`, slotID).Scan(&slotStatus); err != nil {
		t.Fatalf("read slot: %v", err)
	}
	if slotStatus != string(scheduling.SlotAvailable) {
		t.Errorf("slot status = %s after a refused booking, want AVAILABLE: the "+
			"slot is held for a booking that does not exist", slotStatus)
	}

	var appts int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM appointments WHERE slot_id = $1`, slotID).Scan(&appts); err != nil {
		t.Fatalf("count appointments: %v", err)
	}
	if appts != 0 {
		t.Errorf("%d appointment row(s) exist for a booking that was refused", appts)
	}

	var enqueued int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM outbox_events WHERE subject IN ('appointment.created', 'slot.booked')`).
		Scan(&enqueued); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if enqueued != 0 {
		t.Errorf("%d booking event(s) were enqueued for a refused booking", enqueued)
	}
}

// TestAppointmentCreatedGoldenWireFormat pins the bytes.
//
// A struct-to-struct comparison cannot catch a renamed json tag, because both
// sides move together. This asserts the literal JSON, which is what actually
// crosses the process boundary into payment-service.
func TestAppointmentCreatedGoldenWireFormat(t *testing.T) {
	const golden = `{` +
		`"appointment_id":"33333333-3333-4333-8333-333333333333",` +
		`"patient_id":"44444444-4444-4444-8444-444444444444",` +
		`"doctor_id":"55555555-5555-4555-8555-555555555555",` +
		`"slot_id":"66666666-6666-4666-8666-666666666666",` +
		`"start_at":"2026-08-21T09:30:00Z",` +
		`"end_at":"2026-08-21T09:45:00Z",` +
		`"status":"pending_payment",` +
		`"amount_cents":200000,` +
		`"currency":"LKR",` +
		`"specialty":"cardiology",` +
		`"prepayment_required":false,` +
		`"expires_at":"2026-08-21T08:45:00Z",` +
		`"created_at":"2026-08-21T08:30:00Z"` +
		`}`

	start := time.Date(2026, 8, 21, 9, 30, 0, 0, time.UTC)
	created := time.Date(2026, 8, 21, 8, 30, 0, 0, time.UTC)

	raw, err := json.Marshal(events.AppointmentCreated{
		AppointmentID: uuid.MustParse("33333333-3333-4333-8333-333333333333"),
		PatientID:     uuid.MustParse("44444444-4444-4444-8444-444444444444"),
		DoctorID:      uuid.MustParse("55555555-5555-4555-8555-555555555555"),
		SlotID:        uuid.MustParse("66666666-6666-4666-8666-666666666666"),
		StartAt:       start,
		EndAt:         start.Add(15 * time.Minute),
		Status:        "pending_payment",
		AmountCents:   200000,
		Currency:      "LKR",
		Specialty:     "cardiology",
		ExpiresAt:     created.Add(15 * time.Minute),
		CreatedAt:     created,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != golden {
		t.Errorf("appointment.created wire format drifted.\n got: %s\nwant: %s", raw, golden)
	}
}

// TestPaymentResultPayloadMatchesProducers pins this service's view of
// payment.succeeded and payment.failed against the bytes payment-service
// actually publishes.
//
// It exists because this struct was wrong: it read `amount` and `reason` while
// payment-service publishes `amount_cents` and `failure_reason`, so both fields
// arrived empty on every payment event the platform has ever handled and the
// slot-release reason silently fell back to a hardcoded string.
func TestPaymentResultPayloadMatchesProducers(t *testing.T) {
	paymentID := uuid.MustParse("77777777-7777-4777-8777-777777777777")
	apptID := uuid.MustParse("88888888-8888-4888-8888-888888888888")

	// Exactly what telemed-payment-service's PaymentEvent marshals to.
	const fromPaymentService = `{
		"payment_id":"77777777-7777-4777-8777-777777777777",
		"appointment_id":"88888888-8888-4888-8888-888888888888",
		"patient_id":"99999999-9999-4999-8999-999999999999",
		"doctor_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		"amount_cents":200000,
		"currency":"LKR",
		"provider":"stripe",
		"status":"failed",
		"commission_cents":30000,
		"provider_fee_cents":6000,
		"doctor_payout_cents":164000,
		"failure_reason":"card_declined",
		"occurred_at":"2026-08-21T09:00:00Z"
	}`

	var got scheduling.PaymentResultPayload
	if err := json.Unmarshal([]byte(fromPaymentService), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.PaymentID == nil || *got.PaymentID != paymentID {
		t.Errorf("payment_id = %v, want %s", got.PaymentID, paymentID)
	}
	if got.AppointmentID != apptID {
		t.Errorf("appointment_id = %s, want %s", got.AppointmentID, apptID)
	}
	if got.Amount != 200000 {
		t.Errorf("amount = %d, want 200000 (payment-service sends amount_cents)", got.Amount)
	}
	if got.Currency != "LKR" {
		t.Errorf("currency = %q, want LKR", got.Currency)
	}
	if got.FailureText() != "card_declined" {
		t.Errorf("FailureText() = %q, want card_declined", got.FailureText())
	}

	// And the canonical spelling, for whenever payment-service migrates to
	// events.PaymentFailed.
	canonical, err := json.Marshal(events.PaymentFailed{
		PaymentID: paymentID, AppointmentID: apptID,
		AmountCents: 200000, Currency: "LKR", Reason: "card_declined",
	})
	if err != nil {
		t.Fatalf("marshal canonical: %v", err)
	}
	var fromCanonical scheduling.PaymentResultPayload
	if err := json.Unmarshal(canonical, &fromCanonical); err != nil {
		t.Fatalf("unmarshal canonical: %v", err)
	}
	if fromCanonical.Amount != 200000 || fromCanonical.FailureText() != "card_declined" {
		t.Errorf("canonical events.PaymentFailed decoded to amount=%d reason=%q",
			fromCanonical.Amount, fromCanonical.FailureText())
	}
}

// --- helpers ---------------------------------------------------------------

type pricingRow struct {
	Specialty string
	FeeCents  int64
	Currency  string
	Status    string
}

func readPricing(t *testing.T, pool *pgxpool.Pool, doctorID uuid.UUID) pricingRow {
	t.Helper()
	var got pricingRow
	err := pool.QueryRow(context.Background(),
		`SELECT specialty, fee_cents, currency, status FROM doctor_pricing WHERE doctor_id = $1`, doctorID).
		Scan(&got.Specialty, &got.FeeCents, &got.Currency, &got.Status)
	if err != nil {
		t.Fatalf("read doctor_pricing: %v", err)
	}
	got.Currency = strings.TrimSpace(got.Currency)
	return got
}

func decodeAppointmentCreated(t *testing.T, pool *pgxpool.Pool, appointmentID uuid.UUID) events.AppointmentCreated {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(), `
		SELECT payload FROM outbox_events
		WHERE subject = 'appointment.created' AND aggregate_id = $1`, appointmentID.String()).
		Scan(&raw); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	var env struct {
		Payload events.AppointmentCreated `json:"payload"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	return env.Payload
}

// TestDoctorUpdatedAppliesAvailabilityEdits pins the fix for a defect that made
// the availability editor -- the doctor app's flagship screen -- decorative.
//
// doctor.updated is published on every availability edit, but the handler
// applied only the pricing half and returned. The schedule was therefore
// whatever arrived on doctor.approved, frozen for the life of the account: a
// doctor who added Saturday mornings kept generating no Saturday slots, and one
// who dropped Friday kept taking Friday bookings. Nothing errored.
func TestDoctorUpdatedAppliesAvailabilityEdits(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	consumers := scheduling.NewConsumers(svc, nil, nil, zerolog.Nop())
	doctorID := uuid.New()

	// Approved with weekday mornings only.
	yes := true
	if err := consumers.Handle(ctx, envelope(t, events.SubjectDoctorApproved, events.DoctorApproved{
		DoctorID: doctorID, UserID: uuid.New(), DoctorName: "Dr Hours",
		Specialty: "general", FeeCents: 200000, Currency: "LKR",
		Timezone:            "Asia/Colombo",
		SlotDurationMinutes: 15,
		WorkingHours: []events.WorkingHour{
			{DayOfWeek: 1, StartTime: "09:00", EndTime: "12:00", IsAvailable: &yes},
		},
		ApprovedAt: time.Now().UTC().Add(-time.Hour),
	})); err != nil {
		t.Fatalf("approve: %v", err)
	}

	before, err := svc.Repo().ListWorkingHours(ctx, pool, doctorID)
	if err != nil {
		t.Fatalf("read hours after approval: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("after approval: %d working-hour rows, want 1", len(before))
	}

	// The doctor adds Saturday and lengthens their consultations.
	thirty := 30
	buffer := 0
	if err := consumers.Handle(ctx, envelope(t, events.SubjectDoctorUpdated, events.DoctorUpdated{
		DoctorID: doctorID, Specialty: "general", FeeCents: 200000, Currency: "LKR",
		Status: "approved", Timezone: "Asia/Colombo",
		SlotDurationMinutes: thirty,
		BufferMinutes:       &buffer,
		WorkingHours: []events.WorkingHour{
			{DayOfWeek: 1, StartTime: "09:00", EndTime: "12:00", IsAvailable: &yes},
			{DayOfWeek: 6, StartTime: "08:00", EndTime: "11:00", IsAvailable: &yes},
		},
		UpdatedAt: time.Now().UTC(),
	})); err != nil {
		t.Fatalf("update: %v", err)
	}

	after, err := svc.Repo().ListWorkingHours(ctx, pool, doctorID)
	if err != nil {
		t.Fatalf("read hours after update: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("after the edit: %d working-hour rows, want 2 -- the availability "+
			"edit was discarded, which is the bug this test exists for", len(after))
	}

	var sawSaturday bool
	for _, h := range after {
		if h.DayOfWeek == 6 {
			sawSaturday = true
		}
	}
	if !sawSaturday {
		t.Error("Saturday window did not reach scheduling")
	}

	settings, err := svc.Repo().GetScheduleSettings(ctx, pool, doctorID)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if settings.SlotDurationMinutes != thirty {
		t.Errorf("slot_duration_minutes = %d, want %d", settings.SlotDurationMinutes, thirty)
	}
	// Zero buffer is a real preference (back-to-back consultations), not an
	// absent one -- it must survive the round trip.
	if settings.BufferMinutes != 0 {
		t.Errorf("buffer_minutes = %d, want 0 -- zero must not be read as unset",
			settings.BufferMinutes)
	}
}
