package payment

import (
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestComputeSplitInvariant is the property test the whole service rests on.
//
// For any amount and any pair of rates, the three parts must reconstitute the
// amount exactly. Not to within a cent. Exactly. A one-cent drift on a million
// consultations is ten thousand rupees that belongs to somebody and is sitting
// nowhere.
func TestComputeSplitInvariant(t *testing.T) {
	t.Parallel()

	// Deterministic seed: a property test that fails only on Tuesdays is worse
	// than no property test, because nobody can reproduce it.
	rng := rand.New(rand.NewPCG(0x7e1e_1_ed, 0x9e37_79b9))

	modes := []Rounding{RoundHalfUp, RoundHalfEven}

	for i := 0; i < 200_000; i++ {
		// Amounts from 1 cent to LKR 10,000,000. The low end matters: rounding
		// bugs hide in small numbers, not large ones.
		amount := int64(rng.IntN(1_000_000_000)) + 1
		rateBps := rng.IntN(BasisPointDenominator + 1)
		feeBps := rng.IntN(1001) // fees above 10% do not exist in the wild
		feeFixed := int64(rng.IntN(500))
		mode := modes[rng.IntN(len(modes))]

		rule := CommissionRule{
			RateBps:               rateBps,
			ProviderFeeBps:        feeBps,
			ProviderFeeFixedCents: feeFixed,
			Rounding:              mode,
		}

		split, err := ComputeSplit(amount, rule)
		if err != nil {
			// The only legitimate failure is a rule that takes more than the
			// whole amount. Assert that is genuinely what happened rather than
			// letting a real bug hide behind an error return.
			require.ErrorIs(t, err, ErrSplitExceeds,
				"amount=%d rate=%d fee=%d fixed=%d", amount, rateBps, feeBps, feeFixed)
			continue
		}

		require.Equal(t, amount, split.CommissionCents+split.ProviderFeeCents+split.PayoutCents,
			"split does not reconstitute the amount: amount=%d rate=%d fee=%d fixed=%d mode=%s -> c=%d f=%d p=%d",
			amount, rateBps, feeBps, feeFixed, mode,
			split.CommissionCents, split.ProviderFeeCents, split.PayoutCents)

		require.GreaterOrEqual(t, split.CommissionCents, int64(0))
		require.GreaterOrEqual(t, split.ProviderFeeCents, int64(0))
		require.GreaterOrEqual(t, split.PayoutCents, int64(0))
		require.True(t, split.Balanced())

		// The commission must never be more than a cent away from the exact
		// rational value; anything larger means the rounding is not rounding.
		exactNumerator := amount * int64(rateBps)
		low := exactNumerator / BasisPointDenominator
		require.GreaterOrEqual(t, split.CommissionCents, low)
		require.LessOrEqual(t, split.CommissionCents, low+1)
	}
}

// TestRoundingBoundaries pins the exact-half cases, which is where half-up and
// banker's rounding differ and where the money actually moves.
func TestRoundingBoundaries(t *testing.T) {
	t.Parallel()

	// amount × bps / 10000 lands exactly on .5 when the remainder is 5000.
	// amount=1, rate=5000 (50%) -> 0.5 cents.
	cases := []struct {
		name     string
		amount   int64
		rateBps  int
		halfUp   int64
		halfEven int64
	}{
		// q = 0, tie. half-up -> 1, half-even -> 0 (0 is even).
		{"one cent at fifty percent", 1, 5000, 1, 0},
		// 3 × 5000 / 10000 = 1.5. half-up -> 2, half-even -> 2 (1 is odd).
		{"three cents at fifty percent", 3, 5000, 2, 2},
		// 5 × 5000 / 10000 = 2.5. half-up -> 3, half-even -> 2 (2 is even).
		{"five cents at fifty percent", 5, 5000, 3, 2},
		// 7 × 5000 / 10000 = 3.5. half-up -> 4, half-even -> 4 (3 is odd).
		{"seven cents at fifty percent", 7, 5000, 4, 4},
		// 1 × 2500 / 10000 = 0.25 -> below the tie, both round down.
		{"quarter rounds down under both", 1, 2500, 0, 0},
		// 3 × 2500 / 10000 = 0.75 -> above the tie, both round up.
		{"three quarters rounds up under both", 3, 2500, 1, 1},
		// A realistic one: LKR 2,500.01 at 17.5%.
		{"realistic odd amount", 250_001, 1750, 43_750, 43_750},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, err := ComputeSplit(tc.amount, CommissionRule{RateBps: tc.rateBps, Rounding: RoundHalfUp})
			require.NoError(t, err)
			assert.Equal(t, tc.halfUp, up.CommissionCents, "half_up commission")
			assert.Equal(t, tc.amount, up.CommissionCents+up.ProviderFeeCents+up.PayoutCents)

			even, err := ComputeSplit(tc.amount, CommissionRule{RateBps: tc.rateBps, Rounding: RoundHalfEven})
			require.NoError(t, err)
			assert.Equal(t, tc.halfEven, even.CommissionCents, "half_even commission")
			assert.Equal(t, tc.amount, even.CommissionCents+even.ProviderFeeCents+even.PayoutCents)
		})
	}
}

// TestRoundingModesDivergeOverVolume documents, in code, that the choice is not
// cosmetic. If this test ever starts reporting zero the tie-breaking has been
// broken into a no-op.
func TestRoundingModesDivergeOverVolume(t *testing.T) {
	t.Parallel()

	var upTotal, evenTotal int64
	// Every odd cent amount at 50% is an exact tie.
	for amount := int64(1); amount <= 20_001; amount += 2 {
		up, err := ComputeSplit(amount, CommissionRule{RateBps: 5000, Rounding: RoundHalfUp})
		require.NoError(t, err)
		even, err := ComputeSplit(amount, CommissionRule{RateBps: 5000, Rounding: RoundHalfEven})
		require.NoError(t, err)
		upTotal += up.CommissionCents
		evenTotal += even.CommissionCents
	}

	assert.Greater(t, upTotal, evenTotal,
		"half-up must collect more than banker's rounding across exact ties; if these are equal the tie rule is not being applied")
	t.Logf("across 10,001 exact ties: half_up commission %d cents, half_even %d cents, difference %d cents",
		upTotal, evenTotal, upTotal-evenTotal)
}

func TestComputeSplitRejectsBadInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		amount int64
		rule   CommissionRule
		want   error
	}{
		{"zero amount", 0, CommissionRule{RateBps: 2000}, ErrNegativeAmount},
		{"negative amount", -100, CommissionRule{RateBps: 2000}, ErrNegativeAmount},
		{"rate above 100 percent", 1000, CommissionRule{RateBps: 10_001}, ErrRateOutOfRange},
		{"negative rate", 1000, CommissionRule{RateBps: -1}, ErrRateOutOfRange},
		{"fee above 100 percent", 1000, CommissionRule{RateBps: 0, ProviderFeeBps: 10_001}, ErrRateOutOfRange},
		{"commission plus fee exceeds amount", 100,
			CommissionRule{RateBps: 9000, ProviderFeeBps: 2000}, ErrSplitExceeds},
		{"fixed fee alone exceeds amount", 100,
			CommissionRule{RateBps: 0, ProviderFeeFixedCents: 101}, ErrSplitExceeds},
		{"unknown rounding mode", 1000,
			CommissionRule{RateBps: 2000, Rounding: Rounding("stochastic")}, ErrUnknownRounding},
		{"amount beyond the safe range", maxAmountCents + 1,
			CommissionRule{RateBps: 2000}, ErrAmountTooLarge},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ComputeSplit(tc.amount, tc.rule)
			assert.ErrorIs(t, err, tc.want)
		})
	}
}

// TestComputeSplitAtTheLimit proves the largest permitted amount does not
// overflow, which is the failure mode that would silently invert a commission.
func TestComputeSplitAtTheLimit(t *testing.T) {
	t.Parallel()

	split, err := ComputeSplit(maxAmountCents, CommissionRule{RateBps: BasisPointDenominator})
	require.NoError(t, err)
	assert.Equal(t, maxAmountCents, split.CommissionCents)
	assert.Equal(t, int64(0), split.PayoutCents)
	assert.True(t, split.Balanced())
}

func TestPercentOf(t *testing.T) {
	t.Parallel()

	cases := []struct {
		amount  int64
		percent int
		mode    Rounding
		want    int64
	}{
		{10_000, 100, RoundHalfUp, 10_000},
		{10_000, 50, RoundHalfUp, 5_000},
		{10_000, 0, RoundHalfUp, 0},
		{1, 50, RoundHalfUp, 1},   // 0.5 -> 1
		{1, 50, RoundHalfEven, 0}, // 0.5 -> 0
		{3, 50, RoundHalfUp, 2},
		{3, 50, RoundHalfEven, 2},
		{250_001, 50, RoundHalfUp, 125_001},   // 125000.5 -> 125001
		{250_001, 50, RoundHalfEven, 125_000}, // 125000.5 -> 125000
	}
	for _, tc := range cases {
		got, err := PercentOf(tc.amount, tc.percent, tc.mode)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, "PercentOf(%d, %d, %s)", tc.amount, tc.percent, tc.mode)
	}

	_, err := PercentOf(100, 101, RoundHalfUp)
	assert.Error(t, err)
	_, err = PercentOf(100, -1, RoundHalfUp)
	assert.Error(t, err)
}

// TestProrateRefundInvariant: a refund's clawback split must reconstitute the
// refund exactly, for every amount and every fraction of it.
func TestProrateRefundInvariant(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(0xdeadbeef, 0xfeedface))

	for i := 0; i < 100_000; i++ {
		amount := int64(rng.IntN(10_000_000)) + 2
		rule := CommissionRule{
			RateBps:        rng.IntN(5001),
			ProviderFeeBps: rng.IntN(500),
			Rounding:       RoundHalfUp,
		}
		split, err := ComputeSplit(amount, rule)
		if err != nil {
			continue
		}
		p := Payment{
			AmountCents:       amount,
			CommissionCents:   split.CommissionCents,
			ProviderFeeCents:  split.ProviderFeeCents,
			DoctorPayoutCents: split.PayoutCents,
		}

		refund := int64(rng.IntN(int(amount))) + 1
		commissionBack, payoutBack, err := ProrateRefund(p, refund, RoundHalfUp)
		require.NoError(t, err)
		require.Equal(t, refund, commissionBack+payoutBack,
			"refund clawback does not reconstitute the refund: amount=%d refund=%d -> c=%d p=%d",
			amount, refund, commissionBack, payoutBack)
		require.GreaterOrEqual(t, commissionBack, int64(0))
		require.GreaterOrEqual(t, payoutBack, int64(0))
		require.LessOrEqual(t, commissionBack, refund)
	}
}

func TestPercentToBps(t *testing.T) {
	t.Parallel()

	ok := map[string]int{
		"0": 0, "20": 2000, "100": 10000, "17.5": 1750, "2.75": 275,
		"0.01": 1, "20.": 2000, ".5": 50, "+3": 300,
	}
	for in, want := range ok {
		got, err := PercentToBps(in)
		require.NoError(t, err, "input %q", in)
		assert.Equal(t, want, got, "input %q", in)
	}

	for _, bad := range []string{"", "abc", "-5", "20.123", "101", "1e2"} {
		_, err := PercentToBps(bad)
		assert.Error(t, err, "input %q should have been rejected", bad)
	}
}

func TestFormatLKR(t *testing.T) {
	t.Parallel()

	cases := map[int64]string{
		0:         "0.00",
		1:         "0.01",
		99:        "0.99",
		100:       "1.00",
		123_456:   "1,234.56",
		1_000_000: "10,000.00",
		-250_050:  "-2,500.50",
	}
	for cents, want := range cases {
		assert.Equal(t, want, FormatLKR(cents), "cents=%d", cents)
	}
}

func TestBpsString(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "20.00%", BpsString(2000))
	assert.Equal(t, "17.50%", BpsString(1750))
	assert.Equal(t, "0.01%", BpsString(1))
	assert.Equal(t, "100.00%", BpsString(10000))
}
