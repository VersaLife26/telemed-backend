package payment

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRefundPolicy walks the cancellation policy across every branch and,
// specifically, across the two-hour boundary from both sides and exactly on it.
//
// The boundary case is the one the documentation never resolves: it says
// ">2h full, <2h 50%" and is silent about exactly 2h. This test is where that
// silence is turned into a decision, so a future change to it is a visible,
// reviewed change rather than a drifting default.
func TestRefundPolicy(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)

	cases := []struct {
		name        string
		actor       CancelActor
		noShow      bool
		cancelledAt time.Time
		wantPercent int
		wantReason  RefundReason
	}{
		{
			name:        "doctor cancels a week ahead",
			actor:       ActorDoctor,
			cancelledAt: start.Add(-7 * 24 * time.Hour),
			wantPercent: 100,
			wantReason:  ReasonDoctorCancelled,
		},
		{
			name:        "doctor cancels one minute before",
			actor:       ActorDoctor,
			cancelledAt: start.Add(-time.Minute),
			wantPercent: 100,
			wantReason:  ReasonDoctorCancelled,
		},
		{
			name:        "doctor cancels after the start time",
			actor:       ActorDoctor,
			cancelledAt: start.Add(10 * time.Minute),
			wantPercent: 100,
			wantReason:  ReasonDoctorCancelled,
		},
		{
			name:        "patient cancels three hours ahead",
			actor:       ActorPatient,
			cancelledAt: start.Add(-3 * time.Hour),
			wantPercent: 100,
			wantReason:  ReasonPatientCancelledEarly,
		},
		{
			name:        "patient cancels one second past the boundary",
			actor:       ActorPatient,
			cancelledAt: start.Add(-LateCancellationWindow - time.Second),
			wantPercent: 100,
			wantReason:  ReasonPatientCancelledEarly,
		},
		{
			// The documented gap. Exactly two hours' notice refunds in full:
			// the patient is given the boundary.
			name:        "patient cancels exactly on the two-hour boundary",
			actor:       ActorPatient,
			cancelledAt: start.Add(-LateCancellationWindow),
			wantPercent: 100,
			wantReason:  ReasonPatientCancelledEarly,
		},
		{
			name:        "patient cancels one second inside the boundary",
			actor:       ActorPatient,
			cancelledAt: start.Add(-LateCancellationWindow + time.Second),
			wantPercent: 50,
			wantReason:  ReasonPatientCancelledLate,
		},
		{
			name:        "patient cancels one hour ahead",
			actor:       ActorPatient,
			cancelledAt: start.Add(-time.Hour),
			wantPercent: 50,
			wantReason:  ReasonPatientCancelledLate,
		},
		{
			name:        "patient cancels after the appointment should have started",
			actor:       ActorPatient,
			cancelledAt: start.Add(5 * time.Minute),
			wantPercent: 50,
			wantReason:  ReasonPatientCancelledLate,
		},
		{
			name:        "no show overrides an early cancellation",
			actor:       ActorPatient,
			noShow:      true,
			cancelledAt: start.Add(-24 * time.Hour),
			wantPercent: 0,
			wantReason:  ReasonNoShow,
		},
		{
			name:        "no show overrides even a doctor cancellation",
			actor:       ActorDoctor,
			noShow:      true,
			cancelledAt: start.Add(time.Hour),
			wantPercent: 0,
			wantReason:  ReasonNoShow,
		},
		{
			name:        "platform cancellation refunds in full",
			actor:       ActorSystem,
			cancelledAt: start.Add(-time.Minute),
			wantPercent: 100,
			wantReason:  ReasonAdminOverride,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideRefund(tc.actor, tc.noShow, tc.cancelledAt, start)
			assert.Equal(t, tc.wantPercent, got.Percent, "percent")
			assert.Equal(t, tc.wantReason, got.Reason, "reason")
			assert.NotEmpty(t, got.Policy, "every decision must name the clause it came from")
			assert.Equal(t, tc.wantPercent > 0, got.Refundable())
		})
	}
}

// TestRefundPolicyIsTimezoneIndependent proves the boundary is evaluated on
// instants, not on wall-clock fields. A patient in Colombo and a server in UTC
// must reach the same answer.
func TestRefundPolicyIsTimezoneIndependent(t *testing.T) {
	t.Parallel()

	colombo, err := time.LoadLocation("Asia/Colombo")
	require.NoError(t, err)

	startUTC := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	startColombo := startUTC.In(colombo)
	cancelUTC := startUTC.Add(-90 * time.Minute)
	cancelColombo := cancelUTC.In(colombo)

	a := DecideRefund(ActorPatient, false, cancelUTC, startUTC)
	b := DecideRefund(ActorPatient, false, cancelColombo, startColombo)
	c := DecideRefund(ActorPatient, false, cancelColombo, startUTC)

	assert.Equal(t, a.Percent, b.Percent)
	assert.Equal(t, a.Percent, c.Percent)
	assert.Equal(t, 50, a.Percent)
}

func TestParseCancelActor(t *testing.T) {
	t.Parallel()

	assert.Equal(t, ActorDoctor, ParseCancelActor("doctor"))
	assert.Equal(t, ActorDoctor, ParseCancelActor("  DOCTOR "))
	assert.Equal(t, ActorPatient, ParseCancelActor("Patient"))
	assert.Equal(t, ActorSystem, ParseCancelActor("system"))

	// The important case: anything unrecognised must NOT become "patient",
	// because patient is the only branch that can halve a refund.
	for _, s := range []string{"", "admin", "ops", "unknown", "PATIENT_APP", "null"} {
		got := ParseCancelActor(s)
		assert.NotEqual(t, ActorPatient, got, "input %q must not be treated as a patient cancellation", s)
	}
}

// TestRefundAmount checks the policy percentage becomes the right number of
// cents, and that it is clamped by what is actually left to refund.
func TestRefundAmount(t *testing.T) {
	t.Parallel()

	base := Payment{AmountCents: 250_001, Status: StatusSucceeded}

	full := DecideRefund(ActorDoctor, false, time.Now(), time.Now().Add(time.Hour))
	amount, err := RefundAmount(base, full, RoundHalfUp)
	require.NoError(t, err)
	assert.Equal(t, int64(250_001), amount, "a full refund returns every cent, including the odd one")

	half := DecideRefund(ActorPatient, false, time.Now(), time.Now().Add(30*time.Minute))
	amount, err = RefundAmount(base, half, RoundHalfUp)
	require.NoError(t, err)
	assert.Equal(t, int64(125_001), amount, "half of an odd amount rounds half-up")

	amount, err = RefundAmount(base, half, RoundHalfEven)
	require.NoError(t, err)
	assert.Equal(t, int64(125_000), amount, "half of an odd amount rounds to even under banker's rounding")

	// Clamped by what is left.
	partial := base
	partial.RefundedCents = 200_000
	partial.Status = StatusPartiallyRefunded
	amount, err = RefundAmount(partial, full, RoundHalfUp)
	require.NoError(t, err)
	assert.Equal(t, int64(50_001), amount, "a refund can never exceed the unrefunded balance")

	// Nothing to refund on an unsettled payment.
	pending := base
	pending.Status = StatusPending
	amount, err = RefundAmount(pending, full, RoundHalfUp)
	require.NoError(t, err)
	assert.Equal(t, int64(0), amount)

	// A no-show returns nothing regardless of balance.
	noShow := DecideRefund(ActorPatient, true, time.Now(), time.Now().Add(time.Hour))
	amount, err = RefundAmount(base, noShow, RoundHalfUp)
	require.NoError(t, err)
	assert.Equal(t, int64(0), amount)
}
