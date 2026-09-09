package payment

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"telemed/internal/platform/events"
)

// payoutHarness builds a settlement scenario: N doctors, each with captured,
// aged payments waiting to be paid out.
type payoutHarness struct {
	*harness
	runner  *PayoutRunner
	doctors []uuid.UUID
	dest    StaticDestinations
}

func newPayoutHarness(t *testing.T, doctors int, paymentsPer int, amountCents int64) *payoutHarness {
	t.Helper()

	h := newHarness(t)
	dest := StaticDestinations{}
	ids := make([]uuid.UUID, 0, doctors)

	// Payments captured two days ago, comfortably past the 24-hour hold.
	capturedAt := h.now.Add(-48 * time.Hour)

	for d := 0; d < doctors; d++ {
		doctorID := uuid.New()
		ids = append(ids, doctorID)
		dest[doctorID] = "acct_" + doctorID.String()[:8]

		for i := 0; i < paymentsPer; i++ {
			split, err := ComputeSplit(amountCents, h.rule)
			require.NoError(t, err)
			at := capturedAt
			ruleID := h.rule.ID
			seeded := h.store.addPayment(Payment{
				ID:                uuid.New(),
				AppointmentID:     uuid.New(),
				PatientID:         uuid.New(),
				DoctorID:          doctorID,
				AmountCents:       amountCents,
				Currency:          CurrencyLKR,
				Provider:          ProviderMock,
				ProviderIntentID:  "pi_" + uuid.NewString(),
				Status:            StatusSucceeded,
				CommissionCents:   split.CommissionCents,
				ProviderFeeCents:  split.ProviderFeeCents,
				DoctorPayoutCents: split.PayoutCents,
				CommissionRuleID:  &ruleID,
				SucceededAt:       &at,
				CreatedAt:         at,
				IdempotencyKey:    "appointment:" + uuid.NewString(),
			})

			// Write the capture legs the webhook path would have written, so
			// the doctor_payable balance is real and the netting assertion in
			// the re-run test means something.
			require.NoError(t, h.store.InTx(context.Background(), func(ctx context.Context, tx Tx) error {
				return tx.WriteLedger(ctx, h.svc.captureLedger(seeded, at))
			}))
		}
	}

	runner := NewPayoutRunner(h.store, h.svc.providers, dest, zerolog.Nop(), PayoutConfig{
		HoldPeriod:       24 * time.Hour,
		Provider:         ProviderMock,
		MaxDoctorsPerRun: 100,
	}).WithClock(func() time.Time { return h.now })

	return &payoutHarness{harness: h, runner: runner, doctors: ids, dest: dest}
}

// TestPayoutRerunAfterMidBatchFailurePaysNobodyTwice is the payout equivalent
// of the webhook idempotency test.
//
// The first run claims every doctor's payments and then the rail goes away
// mid-batch, exactly as it would if the pod were killed or Stripe returned 503.
// The second run must finish the job and must not pay anyone a second time.
func TestPayoutRerunAfterMidBatchFailurePaysNobodyTwice(t *testing.T) {
	t.Parallel()

	h := newPayoutHarness(t, 4, 3, 500_000) // 4 doctors, 3 consultations each
	ctx := context.Background()

	// Doctor payout on LKR 5,000 at 20% commission and 3% fee: 3,850.00 each,
	// so 11,550.00 per doctor across three consultations.
	const perPayment = 385_000
	const perDoctor = perPayment * 3

	// --- run 1: the rail fails partway through -----------------------------
	// The second doctor's transfer is the one that dies. Everything before it
	// succeeded; everything after never got attempted before the run moved on.
	failing := h.dest[h.doctors[1]]
	h.provider.mu.Lock()
	h.provider.payoutErrOnce[failing] = ErrProviderUnavailable
	h.provider.mu.Unlock()

	first, err := h.runner.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 4, first.Created, "one batch per doctor")
	assert.Equal(t, 3, first.Paid, "three of four settled before the rail went away")

	// The failed doctor's batch is back to pending, not failed: an unavailable
	// rail is a retryable condition and must be picked up automatically.
	payouts, _, err := h.store.ListPayoutsForDoctor(ctx, h.doctors[1], Page{PerPage: 10})
	require.NoError(t, err)
	require.Len(t, payouts, 1)
	assert.Equal(t, PayoutPending, payouts[0].Status)
	assert.Equal(t, int64(perDoctor), payouts[0].AmountCents)

	// Every payment is claimed regardless, so no other run can grab them.
	for _, d := range h.doctors {
		items, _, err := h.store.ListPaymentsForDoctor(ctx, d, Page{PerPage: 10})
		require.NoError(t, err)
		for _, p := range items {
			require.NotNil(t, p.PayoutID, "every eligible payment must be claimed by run 1")
		}
	}

	transfersAfterFirst := h.provider.distinctTransfers()
	assert.Equal(t, 3, transfersAfterFirst)

	// --- run 2: the rail is back -------------------------------------------
	second, err := h.runner.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, second.Created, "run 2 must not create a second batch for anyone")
	assert.Equal(t, 1, second.Paid, "only the doctor left behind is settled")

	// --- run 3 and 4: nothing at all ---------------------------------------
	for i := 0; i < 2; i++ {
		extra, err := h.runner.Run(ctx)
		require.NoError(t, err)
		assert.Equal(t, 0, extra.Created)
		assert.Equal(t, 0, extra.Paid)
	}

	// --- the assertion that matters ----------------------------------------
	assert.Equal(t, 4, h.provider.distinctTransfers(),
		"four doctors must receive exactly four transfers across four runs")

	all, total, err := h.store.ListAllPayouts(ctx, Page{PerPage: 100})
	require.NoError(t, err)
	assert.Equal(t, int64(4), total, "exactly one payout row per doctor")

	var settled int64
	for _, p := range all {
		assert.Equal(t, PayoutPaid, p.Status, "doctor %s", p.DoctorID)
		assert.NotEmpty(t, p.TransferID)
		assert.Equal(t, int64(perDoctor), p.AmountCents)
		assert.Equal(t, 3, p.PaymentCount)
		settled += p.AmountCents
	}
	assert.Equal(t, int64(4*perDoctor), settled)

	// The ledger tells the same story: doctor_payable is debited exactly once
	// per payout, and the four capture credits net against the four payout
	// debits to zero.
	assert.Equal(t, 4, h.store.ledgerGroups(EntryPayoutSent))
	totals := h.store.ledgerTotals()
	assert.Equal(t, int64(0), totals[AccountDoctorPayable],
		"every cent credited to doctors at capture must have been debited exactly once at payout")
	assert.Equal(t, -settled, totals[AccountCash])

	// And exactly four payout.sent events, not five.
	assert.Equal(t, 4, h.store.outboxCount(events.SubjectPayoutSent))
}

// TestPayoutCrashBetweenClaimAndTransfer simulates the worst window: the
// payments are claimed and the process dies before any transfer is attempted.
func TestPayoutCrashBetweenClaimAndTransfer(t *testing.T) {
	t.Parallel()

	h := newPayoutHarness(t, 3, 2, 250_000)
	ctx := context.Background()

	// The rail is entirely unavailable for run 1: claims happen, transfers do not.
	h.provider.mu.Lock()
	h.provider.payoutErr = ErrProviderUnavailable
	h.provider.mu.Unlock()

	first, err := h.runner.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, first.Created)
	assert.Equal(t, 0, first.Paid)
	assert.Equal(t, 0, h.provider.distinctTransfers())

	// The rail returns.
	h.provider.mu.Lock()
	h.provider.payoutErr = nil
	h.provider.mu.Unlock()

	second, err := h.runner.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, second.Created, "the crashed run's batches are resumed, not recreated")
	assert.Equal(t, 3, second.Paid)
	assert.Equal(t, 3, h.provider.distinctTransfers())

	third, err := h.runner.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, third.Paid)
	assert.Equal(t, 3, h.provider.distinctTransfers(), "a third run must move no money")
}

// TestPayoutHoldPeriod: money captured inside the hold window is not settled.
// The hold exists so a same-day chargeback is caught before the funds leave.
func TestPayoutHoldPeriod(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	doctorID := uuid.New()
	dest := StaticDestinations{doctorID: "acct_test"}

	seed := func(age time.Duration) {
		split, err := ComputeSplit(200_000, h.rule)
		require.NoError(t, err)
		at := h.now.Add(-age)
		ruleID := h.rule.ID
		h.store.addPayment(Payment{
			ID: uuid.New(), AppointmentID: uuid.New(), PatientID: uuid.New(), DoctorID: doctorID,
			AmountCents: 200_000, Currency: CurrencyLKR, Provider: ProviderMock,
			Status: StatusSucceeded, CommissionCents: split.CommissionCents,
			ProviderFeeCents: split.ProviderFeeCents, DoctorPayoutCents: split.PayoutCents,
			CommissionRuleID: &ruleID, SucceededAt: &at, CreatedAt: at,
			IdempotencyKey: "appointment:" + uuid.NewString(),
		})
	}
	seed(48 * time.Hour) // eligible
	seed(2 * time.Hour)  // still inside the hold

	runner := NewPayoutRunner(h.store, h.svc.providers, dest, zerolog.Nop(), PayoutConfig{
		HoldPeriod: 24 * time.Hour, Provider: ProviderMock, MaxDoctorsPerRun: 10,
	}).WithClock(func() time.Time { return h.now })

	summary, err := runner.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Paid)

	payouts, _, err := h.store.ListPayoutsForDoctor(context.Background(), doctorID, Page{PerPage: 10})
	require.NoError(t, err)
	require.Len(t, payouts, 1)
	assert.Equal(t, 1, payouts[0].PaymentCount, "only the aged payment is settled")
	assert.Equal(t, int64(154_000), payouts[0].AmountCents, "77% of LKR 2,000.00")
}

// TestPayoutSettlesNetOfRefunds: a partially refunded consultation pays the
// doctor their share of what the patient actually kept, not the original share.
func TestPayoutSettlesNetOfRefunds(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.captured(t, 500_000)

	// Refund half. Doctor payout 3,850.00 loses its share of the 2,500.00.
	_, err := h.svc.Refund(context.Background(), RefundInput{
		PaymentID: p.ID, CallerIsOps: true, Actor: ActorPatient,
		StartAt: h.now.Add(30 * time.Minute), CancelledAt: h.now,
	})
	require.NoError(t, err)

	// Age it past the hold period.
	h.store.mu.Lock()
	cur := h.store.payments[p.ID]
	aged := h.now.Add(-48 * time.Hour)
	cur.SucceededAt = &aged
	h.store.payments[p.ID] = cur
	h.store.mu.Unlock()

	dest := StaticDestinations{p.DoctorID: "acct_x"}
	runner := NewPayoutRunner(h.store, h.svc.providers, dest, zerolog.Nop(), PayoutConfig{
		HoldPeriod: 24 * time.Hour, Provider: ProviderMock, MaxDoctorsPerRun: 10,
	}).WithClock(func() time.Time { return h.now })

	summary, err := runner.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Paid)

	after, err := h.store.GetPayment(context.Background(), p.ID)
	require.NoError(t, err)

	payouts, _, err := h.store.ListPayoutsForDoctor(context.Background(), p.DoctorID, Page{PerPage: 10})
	require.NoError(t, err)
	require.Len(t, payouts, 1)
	assert.Equal(t, after.NetPayoutCents(), payouts[0].AmountCents,
		"the doctor is settled net of the refund, not gross")
	assert.Equal(t, int64(185_000), payouts[0].AmountCents)

	// Capture credit minus refund debit minus payout debit nets to zero.
	assert.Equal(t, int64(0), h.store.ledgerTotals()[AccountDoctorPayable])
}

// TestPayoutWithoutADestinationFailsLoudly: a doctor with no settlement account
// must produce a failed payout with a reason an operator can act on, not a
// silent success and not a crash.
func TestPayoutWithoutADestinationFailsLoudly(t *testing.T) {
	t.Parallel()

	h := newPayoutHarness(t, 1, 1, 300_000)
	// Forget the account.
	h.dest = StaticDestinations{}
	runner := NewPayoutRunner(h.store, h.svc.providers, h.dest, zerolog.Nop(), PayoutConfig{
		HoldPeriod: 24 * time.Hour, Provider: ProviderMock, MaxDoctorsPerRun: 10,
	}).WithClock(func() time.Time { return h.now })

	summary, err := runner.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, summary.Paid)
	assert.Equal(t, 1, summary.Failed)
	assert.Equal(t, 0, h.provider.distinctTransfers())

	payouts, _, err := h.store.ListPayoutsForDoctor(context.Background(), h.doctors[0], Page{PerPage: 10})
	require.NoError(t, err)
	require.Len(t, payouts, 1)
	assert.Equal(t, PayoutFailed, payouts[0].Status)
	assert.Contains(t, payouts[0].FailureReason, "no payout account")
	assert.Equal(t, 0, h.store.ledgerGroups(EntryPayoutSent), "a failed transfer must not move the ledger")
}

// TestPayoutRunWithNothingDue is a no-op, not an error.
func TestPayoutRunWithNothingDue(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	runner := NewPayoutRunner(h.store, h.svc.providers, StaticDestinations{}, zerolog.Nop(), PayoutConfig{
		HoldPeriod: 24 * time.Hour, Provider: ProviderMock,
	}).WithClock(func() time.Time { return h.now })

	summary, err := runner.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, summary.Created)
	assert.Equal(t, 0, summary.Paid)
	assert.Equal(t, 0, summary.Failed)
}

// TestPayoutScheduleIsValidCron catches a typo in the default schedule at test
// time rather than at 02:00 in production.
func TestPayoutScheduleIsValidCron(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	runner := NewPayoutRunner(h.store, h.svc.providers, StaticDestinations{}, zerolog.Nop(), PayoutConfig{
		Schedule: "0 2 * * *", Timezone: "Asia/Colombo", Provider: ProviderMock,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Start(ctx) }()
	cancel()
	require.NoError(t, <-done)
}

func TestPayoutRejectsAnInvalidSchedule(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	runner := NewPayoutRunner(h.store, h.svc.providers, StaticDestinations{}, zerolog.Nop(), PayoutConfig{
		Schedule: "not a cron expression", Timezone: "Asia/Colombo", Provider: ProviderMock,
	})
	err := runner.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cron expression")
}
