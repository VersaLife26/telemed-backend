package payment

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"telemed/internal/platform/events"
)

// This file is the payment-service half of the proof for INTEGRATION-FIXES #10.
//
// scheduling-service's half asserts that a booking produces an outbox row whose
// appointment.created carries a payable amount. This half takes the SAME BYTES
// -- the literal JSON scheduling emits -- and asserts they become a payment row
// here rather than a dropped event.
//
// The two golden strings, here and in scheduling's pricing_test.go, are the
// contract. If either service changes its wire format without the other, one of
// them fails. That is the entire mechanism: before this pass there was no such
// mechanism, both services were individually green, and no patient could pay.

// schedulingAppointmentCreated is a byte-for-byte copy of what
// telemed-scheduling-service enqueues on appointment.created, taken from
// TestAppointmentCreatedGoldenWireFormat in that repo.
const schedulingAppointmentCreated = `{
	"appointment_id":"33333333-3333-4333-8333-333333333333",
	"patient_id":"44444444-4444-4444-8444-444444444444",
	"doctor_id":"55555555-5555-4555-8555-555555555555",
	"slot_id":"66666666-6666-4666-8666-666666666666",
	"start_at":"2026-08-21T09:30:00Z",
	"end_at":"2026-08-21T09:45:00Z",
	"status":"pending_payment",
	"amount_cents":200000,
	"currency":"LKR",
	"specialty":"cardiology",
	"prepayment_required":false,
	"expires_at":"2026-08-21T08:45:00Z",
	"created_at":"2026-08-21T08:30:00Z"
}`

// schedulingAppointmentCancelled is likewise what scheduling emits, including
// the refund decision it owns.
const schedulingAppointmentCancelled = `{
	"appointment_id":"%s",
	"patient_id":"44444444-4444-4444-8444-444444444444",
	"doctor_id":"55555555-5555-4555-8555-555555555555",
	"slot_id":"66666666-6666-4666-8666-666666666666",
	"start_at":"%s",
	"cancelled_by":"patient",
	"no_show":false,
	"refund_policy":"PARTIAL",
	"refund_percent":50,
	"cancelled_at":"%s"
}`

// TestSchedulingAppointmentCreatedBecomesAPayment is the joint.
//
// The defect: scheduling published appointment.created with no amount_cents,
// this service required one, and OnAppointmentCreated refused every event. The
// slot was held, the patient waited, and nothing was ever charged.
func TestSchedulingAppointmentCreatedBecomesAPayment(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	var payload events.AppointmentCreated
	require.NoError(t, json.Unmarshal([]byte(schedulingAppointmentCreated), &payload),
		"scheduling's appointment.created must decode into this service's consumer type")

	// Every field this service reads must have survived the wire.
	require.NotEqual(t, uuid.Nil, payload.AppointmentID, "appointment_id")
	require.NotEqual(t, uuid.Nil, payload.PatientID, "patient_id")
	require.NotEqual(t, uuid.Nil, payload.DoctorID, "doctor_id")
	require.Equal(t, int64(200000), payload.AmountCents,
		"amount_cents: a zero here is the defect -- the event would be refused as unpayable")
	require.Equal(t, "LKR", payload.Currency, "currency")
	require.Equal(t, "cardiology", payload.Specialty, "specialty: the commission is priced off it")
	require.False(t, payload.StartAt.IsZero(), "start_at")

	require.NoError(t, h.svc.OnAppointmentCreated(ctx, payload))

	p, err := h.store.GetPaymentByAppointment(ctx, payload.AppointmentID)
	require.NoError(t, err, "no payment row was created: the booking is unpayable")
	assert.Equal(t, StatusPending, p.Status)
	assert.Equal(t, int64(200000), p.AmountCents)
	assert.Equal(t, CurrencyLKR, p.Currency)
	assert.Equal(t, "cardiology", p.Specialty)
	assert.Equal(t, payload.PatientID, p.PatientID)
	assert.Equal(t, payload.DoctorID, p.DoctorID)
}

// TestSchedulingCancellationRefundsAtSchedulingsPercent asserts that the refund
// percentage comes off the event, not out of this service's own policy copy.
//
// The event below says PARTIAL / 50 while giving SIX HOURS of notice -- which
// this service's own DecideRefund would price at 100%. The 50% outcome is
// therefore proof that scheduling's decision was adopted rather than recomputed.
// The disagreement is deliberately contrived; in production the two agree, and
// the day they stop agreeing is the day this matters.
func TestSchedulingCancellationRefundsAtSchedulingsPercent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	p := h.captured(t, 500_000)

	raw := jsonf(schedulingAppointmentCancelled,
		p.AppointmentID.String(),
		h.now.Add(6*time.Hour).Format(time.RFC3339),
		h.now.Format(time.RFC3339))

	var payload events.AppointmentCancelled
	require.NoError(t, json.Unmarshal([]byte(raw), &payload))
	require.Equal(t, 50, payload.RefundPercent)
	require.Equal(t, "PARTIAL", payload.RefundPolicy)

	require.NoError(t, h.svc.OnAppointmentCancelled(ctx, payload))

	after, err := h.store.GetPayment(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(250_000), after.RefundedCents,
		"the refund must follow scheduling's 50%%, not this service's own reading "+
			"of the notice period (which would be 100%%)")
}

// TestCancellationIsPricedOffCancelledAtNotReceiptTime is the queue-delay bug.
//
// A patient cancels with three hours' notice and earns a full refund. The event
// then sits in JetStream -- a relay restart, a redelivery backoff, a consumer
// lag spike -- and is consumed four hours later. Pricing off "now" at that point
// reads the notice as negative and halves their money.
func TestCancellationIsPricedOffCancelledAtNotReceiptTime(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	p := h.captured(t, 500_000)

	// Cancelled three hours before the appointment, which under this service's
	// own policy is a full refund. No refund_policy on the event, so the
	// fallback path is exercised -- which is exactly where the receipt-time bug
	// would live.
	cancelledAt := h.now.Add(-4 * time.Hour)
	startAt := cancelledAt.Add(3 * time.Hour)

	require.NoError(t, h.svc.OnAppointmentCancelled(ctx, events.AppointmentCancelled{
		AppointmentID: p.AppointmentID,
		CancelledBy:   "patient",
		StartAt:       startAt,
		CancelledAt:   cancelledAt,
	}))

	after, err := h.store.GetPayment(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(500_000), after.RefundedCents,
		"the patient gave three hours' notice and must get everything back, "+
			"however long the event took to arrive")
}

// TestZeroAmountIsStillRefused guards the guard.
//
// The fix must not be "accept anything". An appointment.created with no amount
// is a producer bug, and this service must keep saying so rather than creating
// a payment for nothing.
func TestZeroAmountIsStillRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	err := h.svc.OnAppointmentCreated(context.Background(), events.AppointmentCreated{
		AppointmentID: uuid.New(),
		PatientID:     uuid.New(),
		DoctorID:      uuid.New(),
		Currency:      CurrencyLKR,
		Specialty:     "GP",
		StartAt:       h.now.Add(48 * time.Hour),
	})
	require.Error(t, err, "a zero amount is a bug in the producer, not a free consultation")
	assert.Empty(t, h.store.payments)
}

// TestAppointmentCreatedGoldenWireFormat is the mirror of scheduling's golden
// test. Both repos pin the same bytes, from opposite ends.
func TestAppointmentCreatedGoldenWireFormat(t *testing.T) {
	t.Parallel()

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

	var payload events.AppointmentCreated
	require.NoError(t, json.Unmarshal([]byte(schedulingAppointmentCreated), &payload))

	round, err := json.Marshal(payload)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(round),
		"the appointment.created wire format drifted; scheduling-service's "+
			"golden test must be updated in the same change")
}

// jsonf is fmt.Sprintf, named so the fixture strings above read as templates
// rather than as format strings that happen to contain JSON.
func jsonf(tmpl string, args ...any) string {
	return fmt.Sprintf(tmpl, args...)
}
