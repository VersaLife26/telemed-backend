package payment

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Promo-code rules.
//
// These test the arithmetic and the state machine. They deliberately do NOT
// test concurrency: fakeStore.InTx serialises every transaction with a mutex,
// so "100 goroutines fight over the last redemption" would pass here whether
// or not the row lock exists. That test is
// TestIntegrationPromoConcurrentRedemption, against real Postgres.

// freshPayment seeds a payment that has not been quoted to any rail yet --
// pending, no provider intent -- which is the state a promo code may be
// applied to.
func (h *harness) freshPayment(t *testing.T, amountCents int64) Payment {
	t.Helper()
	return h.store.addPayment(Payment{
		ID:               uuid.New(),
		AppointmentID:    uuid.New(),
		PatientID:        uuid.New(),
		DoctorID:         uuid.New(),
		AmountCents:      amountCents,
		GrossAmountCents: amountCents,
		Currency:         CurrencyLKR,
		Provider:         ProviderMock,
		Status:           StatusPending,
		IdempotencyKey:   "appointment:" + uuid.NewString(),
	})
}

// seedCode creates a promotion through the real service path, so the tests
// exercise the same validation an operator would hit.
func (h *harness) seedCode(t *testing.T, in CreatePromoCodeInput) PromoCode {
	t.Helper()
	c, err := h.svc.CreatePromoCode(context.Background(), in)
	require.NoError(t, err)
	return c
}

func intPtr(n int) *int { return &n }

// TestPromoDiscountArithmetic pins the cent figures.
//
// Every case is integers end to end. A percentage is basis points rounded half
// up, exactly as ComputeSplit rounds, so a discount and a commission computed
// on the same fee never disagree about the half-cent.
func TestPromoDiscountArithmetic(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		code   PromoCode
		amount int64
		want   int64
	}{
		{
			name:   "20 percent of LKR 2,500",
			code:   PromoCode{DiscountType: PromoPercent, PercentBps: 2000},
			amount: 250_000,
			want:   50_000,
		},
		{
			name:   "rounding is half up, not truncation",
			code:   PromoCode{DiscountType: PromoPercent, PercentBps: 3333},
			amount: 1_000, // 333.3 -> 333
			want:   333,
		},
		{
			name:   "half rounds away from zero",
			code:   PromoCode{DiscountType: PromoPercent, PercentBps: 5000},
			amount: 1_001, // 500.5 -> 501
			want:   501,
		},
		{
			name:   "a percentage cap applies",
			code:   PromoCode{DiscountType: PromoPercent, PercentBps: 5000, MaxDiscountCents: 20_000},
			amount: 250_000,
			want:   20_000,
		},
		{
			name:   "a fixed amount is taken verbatim",
			code:   PromoCode{DiscountType: PromoFixed, AmountOffCents: 30_000},
			amount: 250_000,
			want:   30_000,
		},
		{
			name:   "a fixed amount never exceeds the fee",
			code:   PromoCode{DiscountType: PromoFixed, AmountOffCents: 300_000},
			amount: 250_000,
			want:   250_000,
		},
		{
			name:   "below the minimum spend the code does not apply",
			code:   PromoCode{DiscountType: PromoPercent, PercentBps: 2000, MinAmountCents: 500_000},
			amount: 250_000,
			want:   0,
		},
		{
			name:   "100 percent is arithmetically fine here; the service refuses it separately",
			code:   PromoCode{DiscountType: PromoPercent, PercentBps: 10000},
			amount: 250_000,
			want:   250_000,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.code.DiscountFor(tc.amount))
		})
	}
}

// TestNormalizePromoCode. A code the patient types with a space or in lower
// case is the same code; anything that could not have been issued is not a
// code at all and is rejected before it reaches a database lookup.
func TestNormalizePromoCode(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"welcome20":    "WELCOME20",
		" WELCOME 20 ": "WELCOME20",
		"new-user_10":  "NEW-USER_10",
		"":             "",
		"drop table":   "DROPTABLE", // still harmless: every query is parameterised
		"සුබ":          "",          // non-ASCII is not a code we could have issued
		"WELCOME'20":   "",
	}
	for in, want := range cases {
		assert.Equal(t, want, NormalizePromoCode(in), "input %q", in)
	}
}

// TestApplyPromoReducesTheChargeAndKeepsTheListPrice is the core contract:
// amount_cents is what the patient pays, gross_amount_cents is what the
// consultation lists at, and the two never drift.
func TestApplyPromoReducesTheChargeAndKeepsTheListPrice(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	ctx := context.Background()
	p := h.freshPayment(t, 250_000)
	h.seedCode(t, CreatePromoCodeInput{
		Code: "welcome20", DiscountType: PromoPercent, PercentBps: 2000, MaxPerUser: 1,
	})

	summary, err := h.svc.ApplyPromo(ctx, ApplyPromoInput{
		AppointmentID: p.AppointmentID, CallerID: p.PatientID, Code: "welcome20",
	})
	require.NoError(t, err)

	assert.Equal(t, int64(250_000), summary.ConsultationFeeCents)
	assert.Equal(t, int64(50_000), summary.DiscountCents)
	assert.Equal(t, int64(200_000), summary.TotalCents)
	assert.Equal(t, "WELCOME20", summary.AppliedPromoCode)
	require.NotNil(t, summary.PromoExpiresAt, "the client has to know how long the hold lasts")

	stored, err := h.store.GetPayment(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(200_000), stored.AmountCents, "the rail must be asked for the discounted amount")
	assert.Equal(t, int64(250_000), stored.GrossAmountCents, "the list price is frozen at creation")
	assert.True(t, stored.DiscountBalances(), "gross must equal amount plus discount")
}

// TestApplyPromoRefusesToZeroTheBill.
//
// A 100%-off code is not a cheap consultation, it is a free one, and a free
// consultation is not a payment at all -- no rail accepts a zero charge and
// the ledger refuses a zero leg. Refusing with a clear error beats silently
// capping the discount and charging the patient one cent.
func TestApplyPromoRefusesToZeroTheBill(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.freshPayment(t, 250_000)
	h.seedCode(t, CreatePromoCodeInput{
		Code: "FREE", DiscountType: PromoPercent, PercentBps: 10000, MaxPerUser: 1,
	})

	_, err := h.svc.ApplyPromo(context.Background(), ApplyPromoInput{
		AppointmentID: p.AppointmentID, CallerID: p.PatientID, Code: "FREE",
	})
	require.ErrorIs(t, err, ErrPromoNotApplicable)

	stored, _ := h.store.GetPayment(context.Background(), p.ID)
	assert.Equal(t, int64(250_000), stored.AmountCents, "a refused code must not have moved the price")
}

// TestApplyPromoReplacesRatherThanStacks. A second code replaces the first and
// returns its slot; two codes on one consultation is not a feature.
func TestApplyPromoReplacesRatherThanStacks(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	ctx := context.Background()
	p := h.freshPayment(t, 250_000)
	first := h.seedCode(t, CreatePromoCodeInput{
		Code: "TEN", DiscountType: PromoPercent, PercentBps: 1000, MaxPerUser: 1, MaxRedemptions: intPtr(1),
	})
	h.seedCode(t, CreatePromoCodeInput{
		Code: "TWENTY", DiscountType: PromoPercent, PercentBps: 2000, MaxPerUser: 1,
	})

	_, err := h.svc.ApplyPromo(ctx, ApplyPromoInput{AppointmentID: p.AppointmentID, CallerID: p.PatientID, Code: "TEN"})
	require.NoError(t, err)

	summary, err := h.svc.ApplyPromo(ctx, ApplyPromoInput{AppointmentID: p.AppointmentID, CallerID: p.PatientID, Code: "TWENTY"})
	require.NoError(t, err)
	assert.Equal(t, int64(50_000), summary.DiscountCents, "the second code replaced the first, it did not stack")
	assert.Equal(t, "TWENTY", summary.AppliedPromoCode)

	// TEN's single redemption is back in the pool, so somebody else can use it.
	reloaded, err := h.store.GetPromoCodeByCode(ctx, first.Code)
	require.NoError(t, err)
	assert.Equal(t, 0, reloaded.RedemptionCount, "replacing a code must return its slot")
}

// TestApplyPromoIsIdempotentForTheSameCode. A double-tapped Apply button
// refreshes the hold and changes nothing else.
func TestApplyPromoIsIdempotentForTheSameCode(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	ctx := context.Background()
	p := h.freshPayment(t, 250_000)
	code := h.seedCode(t, CreatePromoCodeInput{
		Code: "TWENTY", DiscountType: PromoPercent, PercentBps: 2000, MaxPerUser: 1, MaxRedemptions: intPtr(1),
	})

	for i := range 3 {
		summary, err := h.svc.ApplyPromo(ctx, ApplyPromoInput{
			AppointmentID: p.AppointmentID, CallerID: p.PatientID, Code: "twenty",
		})
		require.NoErrorf(t, err, "apply %d", i)
		assert.Equal(t, int64(50_000), summary.DiscountCents)
	}

	reloaded, err := h.store.GetPromoCodeByCode(ctx, code.Code)
	require.NoError(t, err)
	assert.Equal(t, 1, reloaded.RedemptionCount, "three taps must consume one redemption, not three")
}

// TestPromoPerUserLimit. The limit is per patient, not per payment: a patient
// booking a second consultation must not get the same welcome discount again.
func TestPromoPerUserLimit(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	ctx := context.Background()
	patient := uuid.New()

	first := h.store.addPayment(Payment{
		ID: uuid.New(), AppointmentID: uuid.New(), PatientID: patient, DoctorID: uuid.New(),
		AmountCents: 250_000, GrossAmountCents: 250_000, Currency: CurrencyLKR,
		Provider: ProviderMock, Status: StatusPending, IdempotencyKey: "a:" + uuid.NewString(),
	})
	second := h.store.addPayment(Payment{
		ID: uuid.New(), AppointmentID: uuid.New(), PatientID: patient, DoctorID: uuid.New(),
		AmountCents: 250_000, GrossAmountCents: 250_000, Currency: CurrencyLKR,
		Provider: ProviderMock, Status: StatusPending, IdempotencyKey: "b:" + uuid.NewString(),
	})
	h.seedCode(t, CreatePromoCodeInput{
		Code: "WELCOME", DiscountType: PromoFixed, AmountOffCents: 25_000, MaxPerUser: 1,
	})

	_, err := h.svc.ApplyPromo(ctx, ApplyPromoInput{AppointmentID: first.AppointmentID, CallerID: patient, Code: "WELCOME"})
	require.NoError(t, err)

	_, err = h.svc.ApplyPromo(ctx, ApplyPromoInput{AppointmentID: second.AppointmentID, CallerID: patient, Code: "WELCOME"})
	require.ErrorIs(t, err, ErrPromoExhausted, "one per user means one per user, across bookings")
}

// TestPromoWindowAndDeactivation.
func TestPromoWindowAndDeactivation(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	ctx := context.Background()
	past := h.now.Add(-48 * time.Hour)
	ended := h.now.Add(-time.Hour)
	future := h.now.Add(time.Hour)

	h.seedCode(t, CreatePromoCodeInput{
		Code: "EXPIRED", DiscountType: PromoFixed, AmountOffCents: 10_000,
		ValidFrom: &past, ValidUntil: &ended,
	})
	h.seedCode(t, CreatePromoCodeInput{
		Code: "FUTURE", DiscountType: PromoFixed, AmountOffCents: 10_000, ValidFrom: &future,
	})
	h.seedCode(t, CreatePromoCodeInput{
		Code: "PULLED", DiscountType: PromoFixed, AmountOffCents: 10_000,
	})
	require.NoError(t, h.svc.DeactivatePromoCode(ctx, "pulled"))

	for _, code := range []string{"EXPIRED", "FUTURE"} {
		p := h.freshPayment(t, 250_000)
		_, err := h.svc.ApplyPromo(ctx, ApplyPromoInput{AppointmentID: p.AppointmentID, CallerID: p.PatientID, Code: code})
		assert.ErrorIsf(t, err, ErrPromoExpired, "code %s", code)
	}

	p := h.freshPayment(t, 250_000)
	_, err := h.svc.ApplyPromo(ctx, ApplyPromoInput{AppointmentID: p.AppointmentID, CallerID: p.PatientID, Code: "PULLED"})
	assert.ErrorIs(t, err, ErrPromoUnknown, "a deactivated code is indistinguishable from one that never existed")
}

// TestPromoIsRefusedOnceThePriceIsQuoted is the guard that stops a patient
// discounting a charge the rail has already been told about.
//
// A Stripe PaymentIntent is denominated in a fixed amount. Moving ours after
// the fact means the patient is charged the old figure and shown the new one,
// and the difference is money the platform collected from nobody.
func TestPromoIsRefusedOnceThePriceIsQuoted(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	quoted := h.pendingPayment(t, 250_000) // has a provider_intent_id
	h.seedCode(t, CreatePromoCodeInput{Code: "TWENTY", DiscountType: PromoPercent, PercentBps: 2000})

	_, err := h.svc.ApplyPromo(context.Background(), ApplyPromoInput{
		AppointmentID: quoted.AppointmentID, CallerID: quoted.PatientID, Code: "TWENTY",
	})
	require.ErrorIs(t, err, ErrPromoNotApplicable)
}

// TestRemovePromoRestoresTheListPrice.
func TestRemovePromoRestoresTheListPrice(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	ctx := context.Background()
	p := h.freshPayment(t, 250_000)
	code := h.seedCode(t, CreatePromoCodeInput{
		Code: "TWENTY", DiscountType: PromoPercent, PercentBps: 2000, MaxRedemptions: intPtr(5),
	})

	_, err := h.svc.ApplyPromo(ctx, ApplyPromoInput{AppointmentID: p.AppointmentID, CallerID: p.PatientID, Code: "TWENTY"})
	require.NoError(t, err)

	summary, err := h.svc.RemovePromo(ctx, RemovePromoInput{AppointmentID: p.AppointmentID, CallerID: p.PatientID})
	require.NoError(t, err)
	assert.Equal(t, int64(0), summary.DiscountCents)
	assert.Equal(t, int64(250_000), summary.TotalCents)
	assert.Empty(t, summary.AppliedPromoCode)

	reloaded, err := h.store.GetPromoCodeByCode(ctx, code.Code)
	require.NoError(t, err)
	assert.Equal(t, 0, reloaded.RedemptionCount, "removing a code returns its slot")

	_, err = h.svc.RemovePromo(ctx, RemovePromoInput{AppointmentID: p.AppointmentID, CallerID: p.PatientID})
	assert.ErrorIs(t, err, ErrNoPromoApplied, "removing nothing is a clear 422, not a silent success")
}

// TestPromoIsConsumedOnlyWhenTheMoneyMoves.
//
// This is the whole reason a reservation and a consumption are different
// states. A patient who applies a code and pays has spent it; a patient whose
// payment fails has not, and must get it back.
func TestPromoIsConsumedOnlyWhenTheMoneyMoves(t *testing.T) {
	t.Parallel()

	t.Run("capture consumes it", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		ctx := context.Background()
		p := h.freshPayment(t, 250_000)
		code := h.seedCode(t, CreatePromoCodeInput{
			Code: "TWENTY", DiscountType: PromoPercent, PercentBps: 2000, MaxRedemptions: intPtr(1),
		})
		_, err := h.svc.ApplyPromo(ctx, ApplyPromoInput{AppointmentID: p.AppointmentID, CallerID: p.PatientID, Code: "TWENTY"})
		require.NoError(t, err)

		// Quote the rail, which is what freezes the commission rule. Skipping
		// this would leave the split at zero and the capture would refuse the
		// payment for a reason that has nothing to do with promotions.
		view, err := h.svc.CreateIntent(ctx, CreateIntentInput{AppointmentID: p.AppointmentID, CallerID: p.PatientID})
		require.NoError(t, err)
		require.Equal(t, int64(200_000), view.Payment.AmountCents)

		h.provider.setEvent(WebhookEvent{
			EventID: "evt_ok", Type: "payment_intent.succeeded", Outcome: OutcomePaymentSucceeded,
			PaymentID: p.ID.String(), AmountCents: 200_000, Currency: CurrencyLKR, OccurredAt: h.now,
		})
		_, err = h.svc.HandleWebhook(ctx, ProviderMock, nil, []byte("{}"))
		require.NoError(t, err)

		reloaded, err := h.store.GetPromoCodeByCode(ctx, code.Code)
		require.NoError(t, err)
		assert.Equal(t, 1, reloaded.RedemptionCount, "a consumed code stays spent")

		paid, err := h.store.GetPayment(ctx, p.ID)
		require.NoError(t, err)
		assert.Equal(t, StatusSucceeded, paid.Status)
		assert.Equal(t, int64(200_000), paid.AmountCents)
		assert.True(t, paid.SplitBalances(),
			"the commission split must reconstitute the DISCOUNTED amount, which is what was charged")
	})

	t.Run("a failed payment gives it back", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		ctx := context.Background()
		p := h.freshPayment(t, 250_000)
		code := h.seedCode(t, CreatePromoCodeInput{
			Code: "TWENTY", DiscountType: PromoPercent, PercentBps: 2000, MaxRedemptions: intPtr(1),
		})
		_, err := h.svc.ApplyPromo(ctx, ApplyPromoInput{AppointmentID: p.AppointmentID, CallerID: p.PatientID, Code: "TWENTY"})
		require.NoError(t, err)

		h.provider.setEvent(WebhookEvent{
			EventID: "evt_fail", Type: "payment_intent.payment_failed", Outcome: OutcomePaymentFailed,
			PaymentID: p.ID.String(), FailureReason: "card declined", OccurredAt: h.now,
		})
		_, err = h.svc.HandleWebhook(ctx, ProviderMock, nil, []byte("{}"))
		require.NoError(t, err)

		reloaded, err := h.store.GetPromoCodeByCode(ctx, code.Code)
		require.NoError(t, err)
		assert.Equal(t, 0, reloaded.RedemptionCount,
			"a patient whose card was declined did not use the promotion")
	})
}

// TestExpiredReservationCannotBeCharged is the guard that closes the gap the
// lazy expiry opens.
//
// Applying a code releases that code's *other* stale holds without touching
// their payment rows, so a payment can be carrying a discount whose
// reservation is gone. refreshQuote catches that before the rail is quoted; if
// it did not, the platform would charge a discounted price against a
// promotion nobody holds.
func TestExpiredReservationCannotBeCharged(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	ctx := context.Background()
	p := h.freshPayment(t, 250_000)
	h.seedCode(t, CreatePromoCodeInput{
		Code: "TWENTY", DiscountType: PromoPercent, PercentBps: 2000, MaxRedemptions: intPtr(5),
	})

	_, err := h.svc.ApplyPromo(ctx, ApplyPromoInput{AppointmentID: p.AppointmentID, CallerID: p.PatientID, Code: "TWENTY"})
	require.NoError(t, err)

	discounted, err := h.store.GetPayment(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, int64(200_000), discounted.AmountCents)

	// Walk the clock past the hold and quote the rail.
	h.svc.WithClock(func() time.Time { return h.now.Add(2 * time.Hour) })

	view, err := h.svc.CreateIntent(ctx, CreateIntentInput{
		AppointmentID: p.AppointmentID, CallerID: p.PatientID,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(250_000), view.Payment.AmountCents,
		"an expired hold must restore the list price BEFORE the rail is quoted")
	assert.Empty(t, view.Payment.PromoCode)
	assert.Equal(t, int64(0), view.Payment.DiscountCents)
}

// TestPromoStaleHoldIsReleasedByTheNextPersonToTryTheCode.
//
// This is the property that makes the sweeper optional rather than
// load-bearing: a limited code held by patients who never paid must not stay
// burned until a background job happens to run. Applying it releases its own
// stale holds first, so the next patient to type it cleans up for themselves.
func TestPromoStaleHoldIsReleasedByTheNextPersonToTryTheCode(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	ctx := context.Background()
	abandoned := h.freshPayment(t, 250_000)
	newcomer := h.freshPayment(t, 250_000)
	code := h.seedCode(t, CreatePromoCodeInput{
		Code: "ONLYONE", DiscountType: PromoPercent, PercentBps: 2000, MaxRedemptions: intPtr(1),
	})

	_, err := h.svc.ApplyPromo(ctx, ApplyPromoInput{
		AppointmentID: abandoned.AppointmentID, CallerID: abandoned.PatientID, Code: "ONLYONE",
	})
	require.NoError(t, err)

	// A second patient right now is correctly refused: the one redemption is
	// held.
	_, err = h.svc.ApplyPromo(ctx, ApplyPromoInput{
		AppointmentID: newcomer.AppointmentID, CallerID: newcomer.PatientID, Code: "ONLYONE",
	})
	require.ErrorIs(t, err, ErrPromoExhausted)

	// Past the hold, the same attempt succeeds -- with no sweeper having run.
	h.svc.WithClock(func() time.Time { return h.now.Add(2 * time.Hour) })
	summary, err := h.svc.ApplyPromo(ctx, ApplyPromoInput{
		AppointmentID: newcomer.AppointmentID, CallerID: newcomer.PatientID, Code: "ONLYONE",
	})
	require.NoError(t, err, "an abandoned hold must not burn a limited code permanently")
	assert.Equal(t, int64(50_000), summary.DiscountCents)

	reloaded, err := h.store.GetPromoCodeByCode(ctx, code.Code)
	require.NoError(t, err)
	assert.Equal(t, 1, reloaded.RedemptionCount, "the count still reflects exactly one live hold")
}

// TestPromoSweeperReleasesAbandonedHolds. The sweeper is what gives an
// abandoned hold a terminal status and restores the payment's list price, so
// "how many people applied this code and did not pay" is answerable and a
// patient who comes back is quoted the real fee.
func TestPromoSweeperReleasesAbandonedHolds(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	ctx := context.Background()
	abandoned := h.freshPayment(t, 250_000)
	code := h.seedCode(t, CreatePromoCodeInput{
		Code: "TWENTY", DiscountType: PromoPercent, PercentBps: 2000,
		MaxPerUser: 5, MaxRedemptions: intPtr(5),
	})

	_, err := h.svc.ApplyPromo(ctx, ApplyPromoInput{
		AppointmentID: abandoned.AppointmentID, CallerID: abandoned.PatientID, Code: "TWENTY",
	})
	require.NoError(t, err)

	// Nothing to sweep while the hold is live.
	released, err := h.svc.SweepExpiredPromoReservations(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, 0, released, "a live hold must survive the sweep")

	h.svc.WithClock(func() time.Time { return h.now.Add(2 * time.Hour) })
	released, err = h.svc.SweepExpiredPromoReservations(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, 1, released)

	after, err := h.store.GetPayment(ctx, abandoned.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(250_000), after.AmountCents, "the swept payment is quoted at the list price again")
	assert.Empty(t, after.PromoCode)
	assert.Equal(t, int64(0), after.DiscountCents)

	reloaded, err := h.store.GetPromoCodeByCode(ctx, code.Code)
	require.NoError(t, err)
	assert.Equal(t, 0, reloaded.RedemptionCount)
}

// TestCreatePromoCodeRefusesIncoherentShapes mirrors the promo_codes_shape and
// promo_codes_window CHECKs, so an ops mistake comes back as readable prose
// rather than a constraint name.
func TestCreatePromoCodeRefusesIncoherentShapes(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	ctx := context.Background()
	from := h.now
	before := h.now.Add(-time.Hour)

	cases := []struct {
		name string
		in   CreatePromoCodeInput
	}{
		{"percent with no rate", CreatePromoCodeInput{Code: "A1B", DiscountType: PromoPercent}},
		{"percent carrying a fixed amount", CreatePromoCodeInput{
			Code: "A1B", DiscountType: PromoPercent, PercentBps: 1000, AmountOffCents: 100}},
		{"fixed with no amount", CreatePromoCodeInput{Code: "A1B", DiscountType: PromoFixed}},
		{"fixed carrying a percentage", CreatePromoCodeInput{
			Code: "A1B", DiscountType: PromoFixed, AmountOffCents: 100, PercentBps: 500}},
		{"fixed carrying a percentage cap", CreatePromoCodeInput{
			Code: "A1B", DiscountType: PromoFixed, AmountOffCents: 100, MaxDiscountCents: 50}},
		{"window ends before it starts", CreatePromoCodeInput{
			Code: "A1B", DiscountType: PromoFixed, AmountOffCents: 100, ValidFrom: &from, ValidUntil: &before}},
		{"unusable code", CreatePromoCodeInput{Code: "??", DiscountType: PromoFixed, AmountOffCents: 100}},
		{"unknown discount type", CreatePromoCodeInput{Code: "A1B", DiscountType: "buy-one-get-one"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.svc.CreatePromoCode(ctx, tc.in)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrPromoNotApplicable)
		})
	}
}

// TestPromoOwnership. One patient must not be able to discount another's
// booking, which would also burn their own per-user allowance on it.
func TestPromoOwnership(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.freshPayment(t, 250_000)
	h.seedCode(t, CreatePromoCodeInput{Code: "TWENTY", DiscountType: PromoPercent, PercentBps: 2000})

	_, err := h.svc.ApplyPromo(context.Background(), ApplyPromoInput{
		AppointmentID: p.AppointmentID, CallerID: uuid.New(), Code: "TWENTY",
	})
	assert.ErrorIs(t, err, ErrForbidden)
}
