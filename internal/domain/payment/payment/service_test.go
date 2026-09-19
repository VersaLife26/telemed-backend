package payment

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"telemed/internal/platform/events"
)

// testProvider is a minimal in-package rail.
//
// It lives here rather than reusing internal/payment/provider/mock because that
// package imports this one, and an in-package test file cannot import its own
// importers. The mock rail's own signature verification is tested in its own
// package; what is exercised here is the service's behaviour given a verified
// event.
type testProvider struct {
	mu sync.Mutex

	name          ProviderName
	verifyErr     error
	event         WebhookEvent
	intent        IntentResult
	intentErr     error
	refundErr     error
	refundResult  RefundResult
	payoutErr     error
	payoutErrOnce map[string]error

	intentCalls int
	refundCalls int
	payoutCalls int
	lastReq     IntentRequest
	transfers   map[string]string // idempotency key -> transfer id
}

func newTestProvider(name ProviderName) *testProvider {
	return &testProvider{
		name:          name,
		transfers:     map[string]string{},
		payoutErrOnce: map[string]error{},
		refundResult:  RefundResult{ProviderRefundID: "re_test", Status: RefundSucceeded},
		intent:        IntentResult{ProviderIntentID: "pi_test", ClientSecret: "cs_test", Status: IntentRequiresAction},
	}
}

func (p *testProvider) Name() string { return string(p.name) }

func (p *testProvider) CreateIntent(_ context.Context, req IntentRequest) (IntentResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.intentCalls++
	p.lastReq = req
	if p.intentErr != nil {
		return IntentResult{}, p.intentErr
	}
	return p.intent, nil
}

func (p *testProvider) VerifyWebhook(context.Context, http.Header, []byte) (WebhookEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.verifyErr != nil {
		return WebhookEvent{}, p.verifyErr
	}
	return p.event, nil
}

func (p *testProvider) Refund(context.Context, RefundRequest) (RefundResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refundCalls++
	if p.refundErr != nil {
		return RefundResult{}, p.refundErr
	}
	return p.refundResult, nil
}

func (p *testProvider) Capture(context.Context, CaptureRequest) (CaptureResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return CaptureResult{
		ProviderPaymentID: "capt_test",
		Status:            "succeeded",
	}, nil
}

func (p *testProvider) Payout(_ context.Context, req PayoutRequest) (PayoutResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.payoutCalls++
	if err, ok := p.payoutErrOnce[req.DestinationAccount]; ok {
		delete(p.payoutErrOnce, req.DestinationAccount)
		return PayoutResult{}, err
	}
	if p.payoutErr != nil {
		return PayoutResult{}, p.payoutErr
	}
	// Real idempotency: the same key returns the same transfer, exactly as
	// Stripe would. Without this the re-run test would prove nothing.
	if id, ok := p.transfers[req.IdempotencyKey]; ok {
		return PayoutResult{TransferID: id, Status: PayoutPaid}, nil
	}
	id := "tr_" + uuid.NewString()
	p.transfers[req.IdempotencyKey] = id
	return PayoutResult{TransferID: id, Status: PayoutPaid}, nil
}

// distinctTransfers is how many times money actually moved.
func (p *testProvider) distinctTransfers() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.transfers)
}

func (p *testProvider) setEvent(e WebhookEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.event = e
}

// --- harness ---------------------------------------------------------------

type harness struct {
	store    *fakeStore
	provider *testProvider
	svc      *Service
	rule     CommissionRule
	now      time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	store := newFakeStore()
	// 20% commission, 3% processing fee: the figures the platform
	// documentation names for a GP consultation on a card.
	rule := store.addRule(CommissionRule{
		RuleKey: "default", Version: 1, Scope: ScopeDefault,
		RateBps: 2000, ProviderFeeBps: 300, Rounding: RoundHalfUp,
		EffectiveFrom: time.Now().Add(-time.Hour),
	})

	prov := newTestProvider(ProviderMock)
	reg := NewRegistry(ProviderMock)
	reg.Register(prov)

	pricer := NewPricer(store, time.Minute, CommissionRule{})
	require.NoError(t, pricer.Refresh(context.Background()))

	now := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	svc := NewService(store, pricer, reg, zerolog.Nop(), Config{
		HoldPeriod:      24 * time.Hour,
		DefaultProvider: ProviderMock,
		Currency:        CurrencyLKR,
	}).WithClock(func() time.Time { return now })

	return &harness{store: store, provider: prov, svc: svc, rule: rule, now: now}
}

// pendingPayment seeds a payment that has an intent and is waiting for the
// provider to confirm.
func (h *harness) pendingPayment(t *testing.T, amountCents int64) Payment {
	t.Helper()
	ruleID := h.rule.ID
	split, err := ComputeSplit(amountCents, h.rule)
	require.NoError(t, err)

	return h.store.addPayment(Payment{
		ID:                uuid.New(),
		AppointmentID:     uuid.New(),
		PatientID:         uuid.New(),
		DoctorID:          uuid.New(),
		AmountCents:       amountCents,
		Currency:          CurrencyLKR,
		Provider:          ProviderMock,
		ProviderIntentID:  "pi_" + uuid.NewString(),
		Status:            StatusRequiresAction,
		CommissionCents:   split.CommissionCents,
		ProviderFeeCents:  split.ProviderFeeCents,
		DoctorPayoutCents: split.PayoutCents,
		CommissionRuleID:  &ruleID,
		CommissionRuleKey: h.rule.RuleKey,
		CommissionRuleVer: h.rule.Version,
		IdempotencyKey:    "appointment:" + uuid.NewString(),
	})
}

// --- the idempotency test --------------------------------------------------

// TestWebhookIdempotency is the test this service exists to pass.
//
// The same authenticated event is delivered twice. Exactly one state
// transition, exactly one balanced ledger transaction, exactly one published
// event must result. Anything else is a double-counted consultation in the
// finance report and a doctor paid twice.
func TestWebhookIdempotency(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.pendingPayment(t, 500_000) // LKR 5,000.00

	evt := WebhookEvent{
		EventID:          "evt_replay_me",
		Type:             "payment.succeeded",
		Outcome:          OutcomePaymentSucceeded,
		ProviderIntentID: p.ProviderIntentID,
		AmountCents:      p.AmountCents,
		Currency:         p.Currency,
		Raw:              []byte(`{"id":"evt_replay_me"}`),
	}
	h.provider.setEvent(evt)

	ctx := context.Background()

	first, err := h.svc.HandleWebhook(ctx, ProviderMock, http.Header{}, evt.Raw)
	require.NoError(t, err)
	assert.False(t, first.Replayed, "the first delivery must not be treated as a replay")
	require.NotNil(t, first.PaymentID)

	second, err := h.svc.HandleWebhook(ctx, ProviderMock, http.Header{}, evt.Raw)
	require.NoError(t, err)
	assert.True(t, second.Replayed, "the second delivery of the same event id must be a no-op")

	// Deliver it a further eight times, the way a provider retry storm would.
	for i := 0; i < 8; i++ {
		res, err := h.svc.HandleWebhook(ctx, ProviderMock, http.Header{}, evt.Raw)
		require.NoError(t, err)
		assert.True(t, res.Replayed)
	}

	// --- exactly one state transition
	after, err := h.store.GetPayment(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, after.Status)
	require.NotNil(t, after.SucceededAt)
	assert.Equal(t, h.now, after.SucceededAt.UTC())
	assert.Equal(t, p.Version+1, after.Version,
		"the payment row must have been written exactly once; a second write would bump the version again")

	// --- exactly one ledger effect
	assert.Equal(t, 1, h.store.ledgerGroups(EntryPaymentCaptured),
		"ten deliveries produced more than one balanced capture transaction")

	entries, err := h.store.LedgerForPayment(ctx, p.ID)
	require.NoError(t, err)
	require.Len(t, entries, 4, "capture writes four legs: receivable, commission, doctor payable, processing fee")

	totals := h.store.ledgerTotals()
	assert.Equal(t, after.AmountCents, totals[AccountProviderReceivable])
	assert.Equal(t, -after.CommissionCents, totals[AccountCommissionRevenue])
	assert.Equal(t, -after.DoctorPayoutCents, totals[AccountDoctorPayable])
	assert.Equal(t, -after.ProviderFeeCents, totals[AccountProviderFeePayable])

	// --- exactly one published event
	assert.Equal(t, 1, h.store.outboxCount(events.SubjectPaymentSucceeded))

	// --- exactly one audit row
	assert.Len(t, h.store.webhooks, 1)

	// --- and the money still adds up
	assert.True(t, after.SplitBalances())
	assert.Equal(t, after.AmountCents,
		after.CommissionCents+after.ProviderFeeCents+after.DoctorPayoutCents)
}

// TestWebhookIdempotencyUnderConcurrency delivers the same event from ten
// goroutines at once. The fake store serialises transactions the way Postgres
// serialises on the unique index, so exactly one delivery may win.
func TestWebhookIdempotencyUnderConcurrency(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.pendingPayment(t, 333_333)

	evt := WebhookEvent{
		EventID:          "evt_concurrent",
		Type:             "payment.succeeded",
		Outcome:          OutcomePaymentSucceeded,
		ProviderIntentID: p.ProviderIntentID,
		Raw:              []byte(`{"id":"evt_concurrent"}`),
	}
	h.provider.setEvent(evt)

	const n = 10
	var wg sync.WaitGroup
	results := make([]WebhookResult, n)
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = h.svc.HandleWebhook(context.Background(), ProviderMock, http.Header{}, evt.Raw)
		}(i)
	}
	wg.Wait()

	winners := 0
	for i := range results {
		require.NoError(t, errs[i])
		if !results[i].Replayed {
			winners++
		}
	}
	assert.Equal(t, 1, winners, "exactly one concurrent delivery may be the first")
	assert.Equal(t, 1, h.store.ledgerGroups(EntryPaymentCaptured))
	assert.Equal(t, 1, h.store.outboxCount(events.SubjectPaymentSucceeded))
}

// TestWebhookRejectedSignatureWritesNothing proves an unauthenticated caller
// cannot cause a single row to be written -- not even an audit row, which would
// otherwise be a free way to fill a disk.
func TestWebhookRejectedSignatureWritesNothing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"forged signature", ErrSignatureInvalid},
		{"replayed outside the tolerance window", ErrEventTooOld},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.pendingPayment(t, 100_000)
			h.provider.mu.Lock()
			h.provider.verifyErr = tc.err
			h.provider.mu.Unlock()

			_, err := h.svc.HandleWebhook(context.Background(), ProviderMock, http.Header{}, []byte(`{"forged":true}`))
			require.ErrorIs(t, err, tc.err)

			assert.Empty(t, h.store.webhooks, "a rejected webhook must not create an audit row")
			assert.Empty(t, h.store.ledger, "a rejected webhook must not touch the ledger")
			assert.Empty(t, h.store.outbox, "a rejected webhook must not publish an event")
		})
	}
}

// TestWebhookProcessingFailureRollsBackTheDedupRow is the subtle one.
//
// If processing fails, the dedup row must roll back with it. Otherwise the
// provider's retry finds the event already claimed, skips it, and the payment
// is stuck pending forever with the money taken.
func TestWebhookProcessingFailureRollsBackTheDedupRow(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	// An event referring to a payment that does not exist yet -- the real-world
	// case where the webhook overtakes appointment.created.
	evt := WebhookEvent{
		EventID:          "evt_early",
		Type:             "payment.succeeded",
		Outcome:          OutcomePaymentSucceeded,
		ProviderIntentID: "pi_not_here_yet",
		Raw:              []byte(`{"id":"evt_early"}`),
	}
	h.provider.setEvent(evt)

	_, err := h.svc.HandleWebhook(context.Background(), ProviderMock, http.Header{}, evt.Raw)
	require.ErrorIs(t, err, ErrNotFound)
	assert.Empty(t, h.store.webhooks, "the dedup row must roll back so the provider's retry is genuinely retried")

	// Now the payment arrives and the provider retries.
	p := h.pendingPayment(t, 250_000)
	evt.ProviderIntentID = p.ProviderIntentID
	h.provider.setEvent(evt)

	res, err := h.svc.HandleWebhook(context.Background(), ProviderMock, http.Header{}, evt.Raw)
	require.NoError(t, err)
	assert.False(t, res.Replayed, "the retry must be processed, not skipped as a duplicate")

	after, err := h.store.GetPayment(context.Background(), p.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, after.Status)
	assert.Equal(t, 1, h.store.ledgerGroups(EntryPaymentCaptured))
}

// TestWebhookDedupIsScopedByProvider proves the unique index is on
// (provider, event_id) and not on event_id alone. PayHere and Dialog both
// derive their event id from our own order reference, so a global unique index
// would let one rail's callback silently suppress the other's.
func TestWebhookDedupIsScopedByProvider(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	stripeish := newTestProvider(ProviderStripe)
	h.svc.providers.Register(stripeish)

	shared := "order-12345" // the same id, arriving on two rails

	mockPayment := h.pendingPayment(t, 100_000)
	h.provider.setEvent(WebhookEvent{
		EventID: shared, Type: "payment.succeeded", Outcome: OutcomePaymentSucceeded,
		ProviderIntentID: mockPayment.ProviderIntentID, Raw: []byte(`{}`),
	})

	stripePayment := h.pendingPayment(t, 200_000)
	stripePayment.Provider = ProviderStripe
	stripePayment.Version++
	h.store.mu.Lock()
	h.store.payments[stripePayment.ID] = stripePayment
	h.store.mu.Unlock()
	stripeish.setEvent(WebhookEvent{
		EventID: shared, Type: "payment.succeeded", Outcome: OutcomePaymentSucceeded,
		ProviderIntentID: stripePayment.ProviderIntentID, Raw: []byte(`{}`),
	})

	ctx := context.Background()
	a, err := h.svc.HandleWebhook(ctx, ProviderMock, http.Header{}, []byte(`{}`))
	require.NoError(t, err)
	assert.False(t, a.Replayed)

	b, err := h.svc.HandleWebhook(ctx, ProviderStripe, http.Header{}, []byte(`{}`))
	require.NoError(t, err)
	assert.False(t, b.Replayed, "the same event id on a different rail is a different event")

	assert.Equal(t, 2, h.store.ledgerGroups(EntryPaymentCaptured))
	assert.Len(t, h.store.webhooks, 2)
}

// TestWebhookFailureAfterSettlementIsIgnored: an out-of-order failure event for
// money we already hold must not reverse the capture.
func TestWebhookFailureAfterSettlementIsIgnored(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.pendingPayment(t, 400_000)

	h.provider.setEvent(WebhookEvent{
		EventID: "evt_ok", Outcome: OutcomePaymentSucceeded,
		ProviderIntentID: p.ProviderIntentID, Raw: []byte(`{}`),
	})
	_, err := h.svc.HandleWebhook(context.Background(), ProviderMock, http.Header{}, []byte(`{}`))
	require.NoError(t, err)

	h.provider.setEvent(WebhookEvent{
		EventID: "evt_late_failure", Outcome: OutcomePaymentFailed,
		ProviderIntentID: p.ProviderIntentID, FailureReason: "card declined", Raw: []byte(`{}`),
	})
	_, err = h.svc.HandleWebhook(context.Background(), ProviderMock, http.Header{}, []byte(`{}`))
	require.NoError(t, err)

	after, err := h.store.GetPayment(context.Background(), p.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, after.Status, "a late failure event must not un-capture settled money")
	assert.Equal(t, 1, h.store.ledgerGroups(EntryPaymentCaptured))
	assert.Equal(t, 0, h.store.outboxCount(events.SubjectPaymentFailed))
}

// TestWebhookActualFeeOverridesModelledFee: when the rail reports its real fee,
// the difference lands in the doctor payout and the split still balances.
func TestWebhookActualFeeOverridesModelledFee(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.pendingPayment(t, 500_000)
	modelledFee := p.ProviderFeeCents
	require.Equal(t, int64(15_000), modelledFee, "3% of LKR 5,000.00")

	const actualFee = 17_350
	h.provider.setEvent(WebhookEvent{
		EventID: "evt_fee", Outcome: OutcomePaymentSucceeded,
		ProviderIntentID: p.ProviderIntentID, ProviderFeeCents: actualFee, Raw: []byte(`{}`),
	})
	_, err := h.svc.HandleWebhook(context.Background(), ProviderMock, http.Header{}, []byte(`{}`))
	require.NoError(t, err)

	after, err := h.store.GetPayment(context.Background(), p.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(actualFee), after.ProviderFeeCents)
	assert.Equal(t, int64(100_000), after.CommissionCents, "the commission is our contract with the doctor; the rail's fee does not change it")
	assert.Equal(t, after.AmountCents-after.CommissionCents-actualFee, after.DoctorPayoutCents)
	assert.True(t, after.SplitBalances())
}

// --- capture, refund and consumer behaviour --------------------------------

func TestOnAppointmentCreatedIsIdempotent(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	payload := events.AppointmentCreated{
		AppointmentID: uuid.New(),
		PatientID:     uuid.New(),
		DoctorID:      uuid.New(),
		AmountCents:   350_000,
		Currency:      CurrencyLKR,
		Specialty:     "GP",
		StartAt:       h.now.Add(48 * time.Hour),
	}

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		require.NoError(t, h.svc.OnAppointmentCreated(ctx, payload))
	}

	assert.Len(t, h.store.payments, 1, "five deliveries of appointment.created must create one payment")

	p, err := h.store.GetPaymentByAppointment(ctx, payload.AppointmentID)
	require.NoError(t, err)
	assert.Equal(t, StatusPending, p.Status)
	assert.Equal(t, int64(350_000), p.AmountCents)
	assert.Equal(t, "appointment:"+payload.AppointmentID.String(), p.IdempotencyKey)
}

func TestOnAppointmentCreatedRejectsZeroAmount(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	err := h.svc.OnAppointmentCreated(context.Background(), events.AppointmentCreated{
		AppointmentID: uuid.New(), AmountCents: 0,
	})
	require.Error(t, err)
	assert.Empty(t, h.store.payments)
}

// TestRefundAppliesPolicyAndBalancesTheLedger walks a real 50% late
// cancellation end to end.
func TestRefundAppliesPolicyAndBalancesTheLedger(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.captured(t, 500_000)

	refund, err := h.svc.Refund(context.Background(), RefundInput{
		PaymentID:   p.ID,
		CallerIsOps: true,
		Actor:       ActorPatient,
		StartAt:     h.now.Add(30 * time.Minute), // less than two hours' notice
		CancelledAt: h.now,
	})
	require.NoError(t, err)

	assert.Equal(t, RefundSucceeded, refund.Status)
	assert.Equal(t, 50, refund.Percent)
	assert.Equal(t, ReasonPatientCancelledLate, refund.Reason)
	assert.Equal(t, int64(250_000), refund.AmountCents)

	after, err := h.store.GetPayment(context.Background(), p.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusPartiallyRefunded, after.Status)
	assert.Equal(t, int64(250_000), after.RefundedCents)
	assert.Equal(t, after.RefundedCents, after.RefundedCommissionCents+after.RefundedPayoutCents)
	// The capture-time split is immutable.
	assert.True(t, after.SplitBalances())

	// Half the commission comes back: 20% of 5,000 is 1,000; half is 500.
	assert.Equal(t, int64(50_000), after.RefundedCommissionCents)
	assert.Equal(t, int64(200_000), after.RefundedPayoutCents)
	assert.Equal(t, int64(50_000), after.NetCommissionCents())

	assert.Equal(t, 1, h.store.ledgerGroups(EntryRefundIssued))
	assert.Equal(t, 1, h.store.outboxCount(events.SubjectPaymentRefunded))
}

func TestRefundIsIdempotentOnItsKey(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.captured(t, 500_000)

	in := RefundInput{
		PaymentID: p.ID, CallerIsOps: true, Actor: ActorDoctor,
		StartAt: h.now.Add(time.Hour), CancelledAt: h.now,
		IdempotencyKey: "cancel:fixed-key",
	}

	first, err := h.svc.Refund(context.Background(), in)
	require.NoError(t, err)
	second, err := h.svc.Refund(context.Background(), in)
	require.NoError(t, err)

	assert.Equal(t, first.ID, second.ID, "the same key must return the same refund")
	assert.Len(t, h.store.refunds, 1)
	assert.Equal(t, 1, h.store.ledgerGroups(EntryRefundIssued))
	assert.Equal(t, 1, h.store.outboxCount(events.SubjectPaymentRefunded))

	after, err := h.store.GetPayment(context.Background(), p.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusRefunded, after.Status)
	assert.Equal(t, p.AmountCents, after.RefundedCents, "a doctor cancellation refunds in full, once")
}

func TestOnAppointmentCancelledRefundsPerPolicy(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		cancelledBy string
		noShow      bool
		notice      time.Duration
		wantRefund  int64
	}{
		{"doctor cancels", "doctor", false, 10 * time.Minute, 500_000},
		{"patient cancels early", "patient", false, 6 * time.Hour, 500_000},
		{"patient cancels late", "patient", false, 45 * time.Minute, 250_000},
		{"no show", "patient", true, 0, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			p := h.captured(t, 500_000)

			err := h.svc.OnAppointmentCancelled(context.Background(), events.AppointmentCancelled{
				AppointmentID: p.AppointmentID,
				CancelledBy:   tc.cancelledBy,
				NoShow:        tc.noShow,
				StartAt:       h.now.Add(tc.notice),
				CancelledAt:   h.now,
			})
			require.NoError(t, err)

			after, err := h.store.GetPayment(context.Background(), p.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.wantRefund, after.RefundedCents)
		})
	}
}

func TestOnAppointmentCancelledIsIdempotent(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.captured(t, 500_000)
	payload := events.AppointmentCancelled{
		AppointmentID: p.AppointmentID,
		CancelledBy:   "patient",
		StartAt:       h.now.Add(45 * time.Minute),
		CancelledAt:   h.now,
	}

	for i := 0; i < 4; i++ {
		require.NoError(t, h.svc.OnAppointmentCancelled(context.Background(), payload))
	}

	after, err := h.store.GetPayment(context.Background(), p.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(250_000), after.RefundedCents, "four redeliveries must refund once")
	assert.Len(t, h.store.refunds, 1)
	assert.Equal(t, 1, h.store.ledgerGroups(EntryRefundIssued))
}

func TestOnAppointmentCancelledWithNoPaymentIsANoOp(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	err := h.svc.OnAppointmentCancelled(context.Background(), events.AppointmentCancelled{
		AppointmentID: uuid.New(), CancelledBy: "patient", StartAt: h.now.Add(time.Hour),
	})
	assert.NoError(t, err, "a cancellation with nothing to refund must acknowledge, not retry forever")
}

func TestCreateIntentRequiresThePaymentToExist(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	_, err := h.svc.CreateIntent(context.Background(), CreateIntentInput{
		AppointmentID: uuid.New(), CallerID: uuid.New(),
	})
	assert.ErrorIs(t, err, ErrPaymentNotReady)
}

func TestCreateIntentRefusesAnotherPatientsAppointment(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := h.pendingPayment(t, 100_000)
	_, err := h.svc.CreateIntent(context.Background(), CreateIntentInput{
		AppointmentID: p.AppointmentID,
		CallerID:      uuid.New(), // not the patient on the payment
	})
	assert.ErrorIs(t, err, ErrForbidden)
}

func TestCreateIntentFreezesTheCommissionRule(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.store.addPayment(Payment{
		ID: uuid.New(), AppointmentID: uuid.New(), PatientID: uuid.New(), DoctorID: uuid.New(),
		AmountCents: 500_000, Currency: CurrencyLKR, Provider: ProviderMock,
		Status: StatusPending, IdempotencyKey: "appointment:" + uuid.NewString(),
	})

	view, err := h.svc.CreateIntent(context.Background(), CreateIntentInput{
		AppointmentID: p.AppointmentID,
		CallerID:      p.PatientID,
	})
	require.NoError(t, err)
	require.NotNil(t, view.Payment.CommissionRuleID)
	assert.Equal(t, h.rule.ID, *view.Payment.CommissionRuleID)
	assert.Equal(t, "default", view.Payment.CommissionRuleKey)
	assert.Equal(t, 1, view.Payment.CommissionRuleVer)
	assert.Equal(t, "confirm_card", view.NextAction)
	assert.Equal(t, "cs_test", view.ClientSecret)
	assert.True(t, view.Payment.SplitBalances())
}

func TestCreateIntentPayHereAuthorizeWindow(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	payhereRail := newTestProvider(ProviderPayHere)
	h.svc.providers.Register(payhereRail)

	ctx := context.Background()

	// Case 1: Appointment in 2 days (<= 6 days) -> AuthorizeOnly is true
	inTwoDays := h.now.Add(48 * time.Hour)
	p1 := Payment{
		ID:               uuid.New(),
		AppointmentID:    uuid.New(),
		PatientID:        uuid.New(),
		DoctorID:         uuid.New(),
		AmountCents:      150000,
		GrossAmountCents: 150000,
		Currency:         "LKR",
		Provider:         ProviderPayHere,
		Status:           StatusPending,
		ScheduledStartAt: &inTwoDays,
		IdempotencyKey:   "appt:1",
	}
	require.NoError(t, h.store.InTx(ctx, func(_ context.Context, tx Tx) error {
		return tx.InsertPayment(ctx, &p1)
	}))

	_, err := h.svc.CreateIntent(ctx, CreateIntentInput{
		AppointmentID: p1.AppointmentID,
		CallerID:      p1.PatientID,
		Provider:      ProviderPayHere,
	})
	require.NoError(t, err)
	assert.True(t, payhereRail.lastReq.AuthorizeOnly, "appointment within 6 days must use AuthorizeOnly")

	// Case 2: Appointment in 10 days (> 6 days) -> AuthorizeOnly is false
	inTenDays := h.now.Add(10 * 24 * time.Hour)
	p2 := Payment{
		ID:               uuid.New(),
		AppointmentID:    uuid.New(),
		PatientID:        uuid.New(),
		DoctorID:         uuid.New(),
		AmountCents:      150000,
		GrossAmountCents: 150000,
		Currency:         "LKR",
		Provider:         ProviderPayHere,
		Status:           StatusPending,
		ScheduledStartAt: &inTenDays,
		IdempotencyKey:   "appt:2",
	}
	require.NoError(t, h.store.InTx(ctx, func(_ context.Context, tx Tx) error {
		return tx.InsertPayment(ctx, &p2)
	}))

	_, err = h.svc.CreateIntent(ctx, CreateIntentInput{
		AppointmentID: p2.AppointmentID,
		CallerID:      p2.PatientID,
		Provider:      ProviderPayHere,
	})
	require.NoError(t, err)
	assert.False(t, payhereRail.lastReq.AuthorizeOnly, "appointment > 6 days must NOT use AuthorizeOnly")
}

func TestGetPaymentAuthorization(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.pendingPayment(t, 100_000)
	ctx := context.Background()

	_, err := h.svc.GetPayment(ctx, p.ID, p.PatientID, uuid.Nil, false)
	assert.NoError(t, err, "the paying patient may read their own payment")

	_, err = h.svc.GetPayment(ctx, p.ID, uuid.New(), p.DoctorID, false)
	assert.NoError(t, err, "the treating doctor may read it")

	_, err = h.svc.GetPayment(ctx, p.ID, uuid.New(), uuid.New(), false)
	assert.ErrorIs(t, err, ErrForbidden, "a stranger may not")

	_, err = h.svc.GetPayment(ctx, p.ID, uuid.New(), uuid.Nil, true)
	assert.NoError(t, err, "finance may")
}

// captured seeds a payment that has already settled, with its ledger written,
// so refund and payout tests start from a realistic state.
func (h *harness) captured(t *testing.T, amountCents int64) Payment {
	t.Helper()

	p := h.pendingPayment(t, amountCents)
	h.provider.setEvent(WebhookEvent{
		EventID:          "evt_capture_" + p.ID.String(),
		Type:             "payment.succeeded",
		Outcome:          OutcomePaymentSucceeded,
		ProviderIntentID: p.ProviderIntentID,
		Raw:              []byte(`{}`),
	})
	_, err := h.svc.HandleWebhook(context.Background(), ProviderMock, http.Header{}, []byte(`{}`))
	require.NoError(t, err)

	out, err := h.store.GetPayment(context.Background(), p.ID)
	require.NoError(t, err)
	require.Equal(t, StatusSucceeded, out.Status)
	return out
}

// discard keeps the linter honest about the io import used by nothing else.
var _ = io.Discard

// errUnusedGuard keeps errors imported for the assertions above.
var _ = errors.Is

// --- F10: a webhook must not settle a payment at an amount nobody agreed to --

// TestWebhookAmountMismatchIsRefused is the control that would have contained
// an unauthenticated Dialog callback to zero impact.
//
// Every rail populates WebhookEvent.AmountCents and .Currency, and until this
// existed nothing read either: applyCapture settled the payment at the
// database's figure whatever the rail said it had actually collected. A
// callback claiming any amount at all was accepted.
func TestWebhookAmountMismatchIsRefused(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		amount   int64
		currency string
	}{
		{"the rail collected less than we charged", 100_000, CurrencyLKR},
		{"the rail claims more than we charged", 900_000, CurrencyLKR},
		{"one cent short, which is still a mismatch", 499_999, CurrencyLKR},
		{"a partially captured intent settling as a full capture", 250_000, CurrencyLKR},
		{"the currency disagrees", 500_000, "USD"},
		{"both disagree", 100_000, "USD"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			p := h.pendingPayment(t, 500_000)

			evt := WebhookEvent{
				EventID:          "evt_mismatch",
				Type:             "payment.succeeded",
				Outcome:          OutcomePaymentSucceeded,
				ProviderIntentID: p.ProviderIntentID,
				AmountCents:      tc.amount,
				Currency:         tc.currency,
				Raw:              []byte(`{"id":"evt_mismatch"}`),
			}
			h.provider.setEvent(evt)

			ctx := context.Background()
			_, err := h.svc.HandleWebhook(ctx, ProviderMock, http.Header{}, evt.Raw)
			require.Error(t, err, "a webhook that disagrees about the money must not settle the payment")
			assert.ErrorIs(t, err, ErrWebhookAmountMismatch)

			// The payment did not move.
			after, gerr := h.store.GetPayment(ctx, p.ID)
			require.NoError(t, gerr)
			assert.Equal(t, StatusRequiresAction, after.Status)
			assert.Nil(t, after.SucceededAt)
			assert.Equal(t, p.Version, after.Version, "the payment row must not have been written at all")

			// No ledger effect, which is the part that could not be undone.
			assert.Empty(t, h.store.ledgerTotals(), "an append-only ledger must not record a disputed capture")
			assert.Zero(t, h.store.outboxCount(events.SubjectPaymentSucceeded),
				"nothing downstream may be told the consultation is paid")

			// The evidence survived the refusal: the audit row is committed,
			// not rolled back, with both figures in processing_error.
			rec, ok := h.store.webhookRecord(ProviderMock, "evt_mismatch")
			require.True(t, ok, "the webhook_events row must be kept, or the refusal leaves no trace")
			require.NotNil(t, rec.ProcessedAt)
			assert.Contains(t, rec.ProcessingError, "does not match")
			require.NotNil(t, rec.PaymentID)
			assert.Equal(t, p.ID, *rec.PaymentID)
		})
	}
}

// TestWebhookAmountMismatchIsNotRetriedForever proves the refusal is terminal
// rather than a loop: because the event row commits, the rail's next delivery
// of the same event deduplicates instead of being refused all over again.
func TestWebhookAmountMismatchIsNotRetriedForever(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.pendingPayment(t, 500_000)
	evt := WebhookEvent{
		EventID:          "evt_mismatch_retry",
		Type:             "payment.succeeded",
		Outcome:          OutcomePaymentSucceeded,
		ProviderIntentID: p.ProviderIntentID,
		AmountCents:      1,
		Currency:         CurrencyLKR,
		Raw:              []byte(`{"id":"evt_mismatch_retry"}`),
	}
	h.provider.setEvent(evt)
	ctx := context.Background()

	_, err := h.svc.HandleWebhook(ctx, ProviderMock, http.Header{}, evt.Raw)
	require.ErrorIs(t, err, ErrWebhookAmountMismatch)

	res, err := h.svc.HandleWebhook(ctx, ProviderMock, http.Header{}, evt.Raw)
	require.NoError(t, err, "the redelivery must deduplicate, not re-litigate")
	assert.True(t, res.Replayed)

	after, err := h.store.GetPayment(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusRequiresAction, after.Status, "a redelivery must not settle it either")
	assert.Empty(t, h.store.ledgerTotals())
}

// TestWebhookWithNoReportedAmountStillSettles: zero means "this rail did not
// report an amount on this event", not "zero rupees". PayHere's chargeback
// notify and several Stripe failure events legitimately carry none, and
// refusing those would break settlement rather than protect it.
func TestWebhookWithNoReportedAmountStillSettles(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.pendingPayment(t, 500_000)
	evt := WebhookEvent{
		EventID:          "evt_silent",
		Type:             "payment.succeeded",
		Outcome:          OutcomePaymentSucceeded,
		ProviderIntentID: p.ProviderIntentID,
		Raw:              []byte(`{"id":"evt_silent"}`),
	}
	h.provider.setEvent(evt)

	ctx := context.Background()
	_, err := h.svc.HandleWebhook(ctx, ProviderMock, http.Header{}, evt.Raw)
	require.NoError(t, err)

	after, err := h.store.GetPayment(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, after.Status)
}

// TestWebhookAmountMatchesCaseInsensitiveCurrency: Stripe lower-cases its
// currency codes and PayHere upper-cases them. "lkr" and "LKR" are the same
// currency, and treating them as a mismatch would stop every Stripe capture.
func TestWebhookAmountMatchesCaseInsensitiveCurrency(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.pendingPayment(t, 500_000)
	evt := WebhookEvent{
		EventID:          "evt_case",
		Type:             "payment.succeeded",
		Outcome:          OutcomePaymentSucceeded,
		ProviderIntentID: p.ProviderIntentID,
		AmountCents:      500_000,
		Currency:         "lkr",
		Raw:              []byte(`{"id":"evt_case"}`),
	}
	h.provider.setEvent(evt)

	ctx := context.Background()
	_, err := h.svc.HandleWebhook(ctx, ProviderMock, http.Header{}, evt.Raw)
	require.NoError(t, err)

	after, err := h.store.GetPayment(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, after.Status)
}

// refundCount is how many refund rows exist for a payment. Asserting zero is
// stronger than asserting a status code: it says no money moved, whatever the
// handler chose to reply.
func (h *harness) refundCount(t *testing.T, paymentID uuid.UUID) int {
	t.Helper()
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	n := 0
	for _, r := range h.store.refunds {
		if r.PaymentID == paymentID {
			n++
		}
	}
	return n
}

func TestWebhookAuthorizesPayment(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.pendingPayment(t, 250000)
	evt := WebhookEvent{
		EventID:            "evt_auth_1",
		Type:               "payhere.notify.3",
		Outcome:            OutcomePaymentAuthorized,
		ProviderIntentID:   p.ProviderIntentID,
		AuthorizationToken: "auth_token_xyz",
		AmountCents:        250000,
		Currency:           "LKR",
		Raw:                []byte(`{"id":"evt_auth_1"}`),
	}
	h.provider.setEvent(evt)

	ctx := context.Background()
	_, err := h.svc.HandleWebhook(ctx, ProviderMock, http.Header{}, evt.Raw)
	require.NoError(t, err)

	after, err := h.store.GetPayment(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusAuthorized, after.Status)
	assert.NotNil(t, after.AuthorizedAt)
	assert.Equal(t, "auth_token_xyz", after.AuthorizationToken)
	assert.Equal(t, 1, h.store.outboxCount(events.SubjectPaymentAuthorized))
}

func TestCaptureAfterConsultationEndedAndAppointmentCompleted(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.pendingPayment(t, 300000)

	// Step 1: Place payment on hold (authorized)
	evt := WebhookEvent{
		EventID:            "evt_auth_capture",
		Type:               "payhere.notify.3",
		Outcome:            OutcomePaymentAuthorized,
		ProviderIntentID:   p.ProviderIntentID,
		AuthorizationToken: "tok_capture_123",
		AmountCents:        300000,
		Currency:           "LKR",
		Raw:                []byte(`{"id":"evt_auth_capture"}`),
	}
	h.provider.setEvent(evt)

	ctx := context.Background()
	_, err := h.svc.HandleWebhook(ctx, ProviderMock, http.Header{}, evt.Raw)
	require.NoError(t, err)

	// Step 2: consultation.ended arrives
	err = h.svc.OnConsultationEnded(ctx, events.ConsultationEnded{
		ConsultationID: uuid.New(),
		AppointmentID:  p.AppointmentID,
		PatientID:      p.PatientID,
		DoctorID:       p.DoctorID,
	})
	require.NoError(t, err)

	// Payment should STILL be in StatusAuthorized (waiting for doctor to mark completed)
	mid, err := h.store.GetPayment(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusAuthorized, mid.Status)
	assert.NotNil(t, mid.ConsultationEndedAt)
	assert.Nil(t, mid.CompletedAt)
	assert.Zero(t, h.store.ledgerGroups(EntryPaymentCaptured))

	// Step 3: appointment.completed arrives
	err = h.svc.OnAppointmentCompleted(ctx, events.AppointmentTerminal{
		AppointmentID: p.AppointmentID,
		PatientID:     p.PatientID,
		DoctorID:      p.DoctorID,
		OccurredAt:    time.Now().UTC(),
	})
	require.NoError(t, err)

	// Payment must now be captured and settled!
	finalP, err := h.store.GetPayment(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, finalP.Status)
	assert.NotNil(t, finalP.SucceededAt)
	assert.NotNil(t, finalP.CompletedAt)
	assert.Equal(t, 1, h.store.ledgerGroups(EntryPaymentCaptured))
	assert.Equal(t, 1, h.store.outboxCount(events.SubjectPaymentSucceeded))
}

func TestAppointmentCancelledReleasesAuthorizedHoldWithoutRefund(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.pendingPayment(t, 400000)

	// Authorize payment
	evt := WebhookEvent{
		EventID:            "evt_auth_cancel",
		Type:               "payhere.notify.3",
		Outcome:            OutcomePaymentAuthorized,
		ProviderIntentID:   p.ProviderIntentID,
		AuthorizationToken: "tok_to_cancel",
		AmountCents:        400000,
		Currency:           "LKR",
		Raw:                []byte(`{"id":"evt_auth_cancel"}`),
	}
	h.provider.setEvent(evt)

	ctx := context.Background()
	_, err := h.svc.HandleWebhook(ctx, ProviderMock, http.Header{}, evt.Raw)
	require.NoError(t, err)

	// Patient/Doctor cancels
	err = h.svc.OnAppointmentCancelled(ctx, events.AppointmentCancelled{
		AppointmentID: p.AppointmentID,
		CancelledBy:   "patient",
		CancelledAt:   time.Now().UTC(),
	})
	require.NoError(t, err)

	after, err := h.store.GetPayment(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusFailed, after.Status)
	assert.Equal(t, "cancelled before capture", after.FailureReason)
	// Zero refunds created because funds were never captured
	assert.Equal(t, 0, h.refundCount(t, p.ID))
}
