package payment

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// This file is the entire monetary arithmetic of the platform. It is pure,
// dependency-free, integer-only, and exhaustively tested. Nothing else in the
// service is permitted to compute a commission, a fee, or a payout.
//
// Two rules, and everything else follows:
//
//  1. There is no float64. Not in a parameter, not in a return value, not in an
//     intermediate. Rates are basis points (int); amounts are cents (int64).
//  2. commission + provider_fee + doctor_payout == amount, exactly, always.
//     The payout is derived by subtraction rather than by its own rounding,
//     which is what makes the identity hold by construction instead of by luck.

// Errors returned by the arithmetic. They are sentinel values so a caller can
// distinguish "the rule is misconfigured" from "the database is down".
var (
	ErrNegativeAmount  = errors.New("payment: amount must be positive")
	ErrAmountTooLarge  = errors.New("payment: amount exceeds the safe arithmetic range")
	ErrRateOutOfRange  = errors.New("payment: rate must be between 0 and 10000 basis points")
	ErrSplitExceeds    = errors.New("payment: commission plus provider fee exceeds the amount")
	ErrUnknownRounding = errors.New("payment: unknown rounding mode")
)

// BasisPointDenominator is 100% expressed in basis points.
const BasisPointDenominator = 10_000

// maxAmountCents bounds an amount so that amount * 10000 cannot overflow int64.
// int64 tops out near 9.22e18; 9.2e14 cents is roughly LKR 9.2 trillion, which
// is several times Sri Lanka's annual GDP. If a consultation ever costs that
// much, an overflow is not the most pressing problem.
const maxAmountCents int64 = math.MaxInt64 / BasisPointDenominator

// Rounding names the tie-breaking rule applied when a proportional share lands
// exactly halfway between two cents.
//
// This choice is load-bearing. On a million consultations at a rate whose
// products land on .5 cent, half-up and half-even differ by roughly five
// thousand rupees of platform revenue. It is therefore stored per commission
// rule version, not hardcoded, so changing it is a priced, dated, auditable
// event rather than a deploy.
type Rounding string

const (
	// RoundHalfUp rounds .5 away from zero. This is the platform default.
	//
	// Rationale: it is what a Sri Lankan accountant reproduces with a
	// calculator, what PayHere's own settlement statements do, and what a
	// doctor disputing a payout will arrive at independently. Being
	// reproducible by hand matters more here than being statistically
	// unbiased, because every disagreement about a payout is settled by
	// someone redoing the sum on paper.
	RoundHalfUp Rounding = "half_up"

	// RoundHalfEven is banker's rounding: .5 goes to the nearest even cent, so
	// rounding error does not accumulate in one direction over large volumes.
	// Available per rule for corporate contracts that specify it.
	RoundHalfEven Rounding = "half_even"
)

// DefaultRounding is applied when a rule does not name one.
const DefaultRounding = RoundHalfUp

// Valid reports whether r is a supported mode.
func (r Rounding) Valid() bool {
	return r == RoundHalfUp || r == RoundHalfEven
}

// Split is the decomposition of one payment amount.
//
// The zero value is not meaningful; construct it only via ComputeSplit.
type Split struct {
	AmountCents      int64
	CommissionCents  int64
	ProviderFeeCents int64
	PayoutCents      int64

	// The provenance of the numbers above, copied so an invoice can explain
	// itself without a second query.
	RateBps        int
	ProviderFeeBps int
	FeeFixedCents  int64
	Rounding       Rounding
}

// Balanced is the invariant the property test hammers: the three parts
// reconstitute the whole, exactly, with no tolerance.
func (s Split) Balanced() bool {
	return s.CommissionCents+s.ProviderFeeCents+s.PayoutCents == s.AmountCents
}

// ComputeSplit prices an amount against a commission rule.
//
//	commission = round(amount × rate_bps / 10000)
//	fee        = round(amount × fee_bps  / 10000) + fee_fixed
//	payout     = amount − commission − fee
//
// The payout is a subtraction, never its own rounded product. That is the
// whole trick: any rounding residue lands in the payout, and the identity
// holds for every input rather than for most of them.
func ComputeSplit(amountCents int64, rule CommissionRule) (Split, error) {
	if amountCents <= 0 {
		return Split{}, fmt.Errorf("%w: got %d", ErrNegativeAmount, amountCents)
	}
	if amountCents > maxAmountCents {
		return Split{}, fmt.Errorf("%w: got %d, max %d", ErrAmountTooLarge, amountCents, maxAmountCents)
	}
	if rule.RateBps < 0 || rule.RateBps > BasisPointDenominator {
		return Split{}, fmt.Errorf("%w: rate_bps=%d", ErrRateOutOfRange, rule.RateBps)
	}
	if rule.ProviderFeeBps < 0 || rule.ProviderFeeBps > BasisPointDenominator {
		return Split{}, fmt.Errorf("%w: provider_fee_bps=%d", ErrRateOutOfRange, rule.ProviderFeeBps)
	}
	if rule.ProviderFeeFixedCents < 0 {
		return Split{}, fmt.Errorf("%w: provider_fee_fixed_cents=%d", ErrNegativeAmount, rule.ProviderFeeFixedCents)
	}

	mode := rule.Rounding
	if mode == "" {
		mode = DefaultRounding
	}
	if !mode.Valid() {
		return Split{}, fmt.Errorf("%w: %q", ErrUnknownRounding, mode)
	}

	commission, err := applyRate(amountCents, rule.RateBps, mode)
	if err != nil {
		return Split{}, err
	}
	feeVariable, err := applyRate(amountCents, rule.ProviderFeeBps, mode)
	if err != nil {
		return Split{}, err
	}
	fee := feeVariable + rule.ProviderFeeFixedCents

	if commission+fee > amountCents {
		return Split{}, fmt.Errorf("%w: amount=%d commission=%d fee=%d",
			ErrSplitExceeds, amountCents, commission, fee)
	}

	s := Split{
		AmountCents:      amountCents,
		CommissionCents:  commission,
		ProviderFeeCents: fee,
		PayoutCents:      amountCents - commission - fee,
		RateBps:          rule.RateBps,
		ProviderFeeBps:   rule.ProviderFeeBps,
		FeeFixedCents:    rule.ProviderFeeFixedCents,
		Rounding:         mode,
	}
	if !s.Balanced() {
		// Unreachable by construction. It is asserted anyway because the cost
		// of being wrong here is real money, and the cost of the check is a
		// single comparison.
		return Split{}, fmt.Errorf("payment: internal split imbalance for amount %d", amountCents)
	}
	return s, nil
}

// applyRate computes round(amount × bps / 10000) with the given tie rule.
func applyRate(amountCents int64, bps int, mode Rounding) (int64, error) {
	if bps == 0 {
		return 0, nil
	}
	// amountCents is already bounded by maxAmountCents, so this cannot overflow.
	return roundedDiv(amountCents*int64(bps), BasisPointDenominator, mode)
}

// roundedDiv divides numerator by denominator with the requested tie rule.
// Both arguments must be non-negative; every monetary quantity in this service
// is, and refusing negatives keeps the two rounding implementations from having
// to agree about what "half away from zero" means below zero.
func roundedDiv(numerator, denominator int64, mode Rounding) (int64, error) {
	if denominator <= 0 {
		return 0, fmt.Errorf("payment: division by non-positive denominator %d", denominator)
	}
	if numerator < 0 {
		return 0, fmt.Errorf("%w: numerator %d", ErrNegativeAmount, numerator)
	}

	q := numerator / denominator
	r := numerator % denominator
	if r == 0 {
		return q, nil
	}

	// Compare 2r against the denominator rather than r against denominator/2,
	// so an odd denominator does not lose the tie case to integer truncation.
	twice := 2 * r
	switch {
	case twice > denominator:
		return q + 1, nil
	case twice < denominator:
		return q, nil
	}

	// Exact tie.
	switch mode {
	case RoundHalfUp:
		return q + 1, nil
	case RoundHalfEven:
		if q%2 == 0 {
			return q, nil
		}
		return q + 1, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrUnknownRounding, mode)
	}
}

// PercentOf computes round(amount × percent / 100), used by the refund policy.
// It shares roundedDiv with the commission maths so a 50% refund and a 50%
// commission never disagree about the same half-cent.
func PercentOf(amountCents int64, percent int, mode Rounding) (int64, error) {
	if percent < 0 || percent > 100 {
		return 0, fmt.Errorf("payment: refund percent %d out of range", percent)
	}
	if amountCents < 0 {
		return 0, fmt.Errorf("%w: %d", ErrNegativeAmount, amountCents)
	}
	if percent == 0 || amountCents == 0 {
		return 0, nil
	}
	if percent == 100 {
		return amountCents, nil
	}
	if amountCents > math.MaxInt64/100 {
		return 0, fmt.Errorf("%w: %d", ErrAmountTooLarge, amountCents)
	}
	if !mode.Valid() {
		mode = DefaultRounding
	}
	return roundedDiv(amountCents*int64(percent), 100, mode)
}

// ProrateRefund splits a refund of refundCents across the commission and the
// doctor payout in the same proportion the original capture used.
//
// The provider fee is deliberately NOT clawed back: Stripe and PayHere both
// keep their processing fee on a refunded charge, so pretending we recover it
// would overstate what we can return. The commission share absorbs its own
// rounding and the doctor share takes the remainder, which keeps
// commissionBack + payoutBack == refundCents exactly.
func ProrateRefund(p Payment, refundCents int64, mode Rounding) (commissionBack, payoutBack int64, err error) {
	switch {
	case refundCents <= 0:
		return 0, 0, fmt.Errorf("%w: refund %d", ErrNegativeAmount, refundCents)
	case p.AmountCents <= 0:
		return 0, 0, fmt.Errorf("%w: payment amount %d", ErrNegativeAmount, p.AmountCents)
	case refundCents > p.AmountCents:
		return 0, 0, fmt.Errorf("payment: refund %d exceeds amount %d", refundCents, p.AmountCents)
	}
	if p.CommissionCents > math.MaxInt64/max64(refundCents, 1) {
		return 0, 0, fmt.Errorf("%w: commission %d", ErrAmountTooLarge, p.CommissionCents)
	}
	if !mode.Valid() {
		mode = DefaultRounding
	}

	commissionBack, err = roundedDiv(p.CommissionCents*refundCents, p.AmountCents, mode)
	if err != nil {
		return 0, 0, err
	}
	if commissionBack > refundCents {
		commissionBack = refundCents
	}
	payoutBack = refundCents - commissionBack
	return commissionBack, payoutBack, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// FormatLKR renders cents as a human-readable amount for invoices and API
// responses. It never participates in arithmetic; it is a presentation
// function that happens to live next to the arithmetic so the two stay
// consistent about what a cent is.
func FormatLKR(cents int64) string {
	neg := cents < 0
	if neg {
		cents = -cents
	}
	rupees := cents / 100
	sub := cents % 100

	// Group the rupee part in thousands.
	digits := strconv.FormatInt(rupees, 10)
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	for i, d := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(d)
	}
	return fmt.Sprintf("%s.%02d", b.String(), sub)
}

// BpsString renders basis points as a percentage for display: 1750 -> "17.50%".
func BpsString(bps int) string {
	return fmt.Sprintf("%d.%02d%%", bps/100, bps%100)
}
