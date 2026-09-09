package payment

import (
	"context"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"telemed/internal/platform/repopath"
)

// TestParseSeedRulesAcceptsTheDocumentedShape parses exactly the blob the
// source documentation specifies in §8.5 and §11.3, so the migration path from
// the documented design to this one is real rather than asserted.
func TestParseSeedRulesAcceptsTheDocumentedShape(t *testing.T) {
	t.Parallel()

	raw := `[{"specialty":"GP","commission":20},{"specialty":"Cardiology","commission":25},{"corporate_client":"AIA","commission":15}]`
	now := time.Now().UTC()

	rules, err := ParseSeedRules(raw, now)
	require.NoError(t, err)
	require.Len(t, rules, 3)

	byKey := map[string]CommissionRule{}
	for _, r := range rules {
		byKey[r.RuleKey] = r
		assert.Equal(t, 1, r.Version, "a seed is always version 1")
		assert.Equal(t, DefaultRounding, r.Rounding)
		assert.Nil(t, r.EffectiveTo, "a seeded rule is open")
		assert.Equal(t, now, r.EffectiveFrom)
	}

	assert.Equal(t, 2000, byKey["specialty:gp"].RateBps)
	assert.Equal(t, ScopeSpecialty, byKey["specialty:gp"].Scope)
	assert.Equal(t, "GP", byKey["specialty:gp"].MatchValue)
	assert.Equal(t, 2500, byKey["specialty:cardiology"].RateBps)
	assert.Equal(t, 1500, byKey["corporate:aia"].RateBps)
	assert.Equal(t, ScopeCorporate, byKey["corporate:aia"].Scope)
}

func TestParseSeedRulesExtendedFields(t *testing.T) {
	t.Parallel()

	raw := `[{"default":true,"commission":17.5,"provider_fee":2.9,"provider_fee_fixed_cents":3000,"rounding":"half_even","note":"launch rate"}]`
	rules, err := ParseSeedRules(raw, time.Now())
	require.NoError(t, err)
	require.Len(t, rules, 1)

	r := rules[0]
	assert.Equal(t, "default", r.RuleKey)
	assert.Equal(t, ScopeDefault, r.Scope)
	assert.Equal(t, 1750, r.RateBps)
	assert.Equal(t, 290, r.ProviderFeeBps)
	assert.Equal(t, int64(3000), r.ProviderFeeFixedCents)
	assert.Equal(t, RoundHalfEven, r.Rounding)
	assert.Equal(t, "launch rate", r.Note)
}

func TestParseSeedRulesRejectsBadInput(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"not json":              `{"specialty":"GP"}`,
		"no scope named":        `[{"commission":20}]`,
		"duplicate rule key":    `[{"specialty":"GP","commission":20},{"specialty":"gp","commission":30}]`,
		"rate over 100 percent": `[{"default":true,"commission":101}]`,
		"unknown rounding":      `[{"default":true,"commission":20,"rounding":"stochastic"}]`,
		"sub-basis-point rate":  `[{"default":true,"commission":20.123}]`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseSeedRules(raw, time.Now())
			assert.Error(t, err)
		})
	}

	empty, err := ParseSeedRules("", time.Now())
	require.NoError(t, err)
	assert.Empty(t, empty, "an empty COMMISSION_RULES seeds nothing, which the boot check reports")
}

// TestShippedEnvExampleParses guards the file a developer actually copies. A
// malformed COMMISSION_RULES in .env.example means `make run` fails on a fresh
// clone, which is the worst possible first impression.
func TestShippedEnvExampleParses(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(repopath.File(t, "deploy", "env", "payment.env.example"))
	require.NoError(t, err)

	m := regexp.MustCompile(`(?m)^COMMISSION_RULES=(.*)$`).FindSubmatch(raw)
	require.Len(t, m, 2, ".env.example must define COMMISSION_RULES")

	rules, err := ParseSeedRules(string(m[1]), time.Now())
	require.NoError(t, err, "the shipped COMMISSION_RULES must parse")
	require.NotEmpty(t, rules)

	// And every shipped rule must be able to price a consultation.
	for _, r := range rules {
		split, err := ComputeSplit(500_000, r)
		require.NoError(t, err, "rule %s", r.RuleKey)
		assert.True(t, split.Balanced())
	}
}

// TestPricerSelectsTheMostSpecificRule pins the precedence:
// corporate > specialty > default.
func TestPricerSelectsTheMostSpecificRule(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.addRule(CommissionRule{RuleKey: "default", Version: 1, Scope: ScopeDefault, RateBps: 2000})
	store.addRule(CommissionRule{RuleKey: "specialty:cardiology", Version: 1, Scope: ScopeSpecialty,
		MatchValue: "Cardiology", RateBps: 2500})
	store.addRule(CommissionRule{RuleKey: "corporate:aia", Version: 1, Scope: ScopeCorporate,
		MatchValue: "AIA", RateBps: 1500})

	pricer := NewPricer(store, time.Minute, CommissionRule{})
	ctx := context.Background()
	require.NoError(t, pricer.Refresh(ctx))

	cases := []struct {
		name string
		pc   PricingContext
		want int
	}{
		{"nothing matches but default", PricingContext{}, 2000},
		{"unknown specialty falls back to default", PricingContext{Specialty: "Dermatology"}, 2000},
		{"specialty beats default", PricingContext{Specialty: "Cardiology"}, 2500},
		{"specialty match is case-insensitive", PricingContext{Specialty: "cardiology"}, 2500},
		{"corporate beats specialty", PricingContext{Specialty: "Cardiology", CorporateClient: "AIA"}, 1500},
		{"corporate alone", PricingContext{CorporateClient: "aia"}, 1500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rule, err := pricer.Select(ctx, tc.pc)
			require.NoError(t, err)
			assert.Equal(t, tc.want, rule.RateBps)
		})
	}
}

// TestPricerFallsBackWhenNothingMatches: a pricing lookup must fail closed at a
// sane rate, never open at zero commission.
func TestPricerFallsBackWhenNothingMatches(t *testing.T) {
	t.Parallel()

	store := newFakeStore() // no rules at all
	fallback := CommissionRule{RuleKey: "fallback", Scope: ScopeDefault, RateBps: 2000, Rounding: RoundHalfUp}
	pricer := NewPricer(store, time.Minute, fallback)
	ctx := context.Background()

	rule, err := pricer.Select(ctx, PricingContext{Specialty: "GP"})
	require.NoError(t, err)
	assert.Equal(t, "fallback", rule.RuleKey)
	assert.Equal(t, 2000, rule.RateBps)

	// With no fallback configured either, pricing is an error rather than a
	// silent zero-commission consultation.
	bare := NewPricer(store, time.Minute, CommissionRule{})
	_, err = bare.Select(ctx, PricingContext{Specialty: "GP"})
	assert.Error(t, err)
}

// TestPricerRuleByIDCachesImmutableVersions proves a repricing lookup does not
// hit the database twice, which is what keeps it safe to call from inside a
// transaction.
func TestPricerRuleByIDCachesImmutableVersions(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	rule := store.addRule(CommissionRule{RuleKey: "default", Version: 1, Scope: ScopeDefault, RateBps: 2000})

	pricer := NewPricer(store, time.Minute, CommissionRule{})
	ctx := context.Background()
	require.NoError(t, pricer.Refresh(ctx))

	got, err := pricer.RuleByID(ctx, rule.ID)
	require.NoError(t, err)
	assert.Equal(t, 2000, got.RateBps)

	// Delete it from the store; the cache must still answer, because a rule
	// version is immutable and an invoice must remain reproducible.
	store.mu.Lock()
	delete(store.rules, rule.ID)
	store.mu.Unlock()

	again, err := pricer.RuleByID(ctx, rule.ID)
	require.NoError(t, err)
	assert.Equal(t, rule.ID, again.ID)

	_, err = pricer.RuleByID(ctx, uuidNil())
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestSupersedingARuleKeepsHistoricalPricing is the whole argument for keeping
// rules in the database rather than in an environment variable.
func TestSupersedingARuleKeepsHistoricalPricing(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	v1 := store.addRule(CommissionRule{
		RuleKey: "default", Version: 1, Scope: ScopeDefault, RateBps: 2000, Rounding: RoundHalfUp,
	})

	pricer := NewPricer(store, time.Nanosecond, CommissionRule{})
	ctx := context.Background()
	require.NoError(t, pricer.Refresh(ctx))

	// A consultation priced today.
	oldSplit, err := ComputeSplit(500_000, v1)
	require.NoError(t, err)
	assert.Equal(t, int64(100_000), oldSplit.CommissionCents)

	// The rate changes: close v1, open v2 at 30%.
	store.mu.Lock()
	closed := store.rules[v1.ID]
	now := time.Now()
	closed.EffectiveTo = &now
	store.rules[v1.ID] = closed
	store.mu.Unlock()
	store.addRule(CommissionRule{
		RuleKey: "default", Version: 2, Scope: ScopeDefault, RateBps: 3000, Rounding: RoundHalfUp,
	})
	require.NoError(t, pricer.Refresh(ctx))

	// New consultations price at the new rate...
	current, err := pricer.Select(ctx, PricingContext{})
	require.NoError(t, err)
	assert.Equal(t, 3000, current.RateBps)

	// ...and the old one still reprints at the rate it was sold at.
	historical, err := pricer.RuleByID(ctx, v1.ID)
	require.NoError(t, err)
	assert.Equal(t, 2000, historical.RateBps)

	reprint, err := ComputeSplit(500_000, historical)
	require.NoError(t, err)
	assert.Equal(t, oldSplit.CommissionCents, reprint.CommissionCents,
		"an invoice reprinted after a rate change must show the original numbers")
}

func TestRuleKey(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "default", RuleKey(ScopeDefault, ""))
	assert.Equal(t, "default", RuleKey(ScopeDefault, "ignored"))
	assert.Equal(t, "specialty:gp", RuleKey(ScopeSpecialty, "GP"))
	assert.Equal(t, "specialty:gp", RuleKey(ScopeSpecialty, "  gp  "))
	assert.Equal(t, "corporate:aia", RuleKey(ScopeCorporate, "AIA"))
}
