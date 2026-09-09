package payment

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Commission rules live in the database, not in an environment variable.
//
// The source documentation (§8.5, §11.3) puts them in COMMISSION_RULES as a
// JSON blob: [{"specialty":"GP","commission":20}, ...]. That works exactly once.
// The moment the rate changes, every invoice ever issued reprints at the new
// rate, because there is nowhere to record what the rate was on the day. A
// doctor disputing a six-month-old payout cannot be answered, and a tax audit
// cannot be satisfied.
//
// So: rules are versioned rows in commission_rules, a payment stores the id of
// the exact row that priced it, and COMMISSION_RULES is demoted to what it is
// actually good for -- a first-boot seed for an empty database. Editing the
// env var after the first boot does nothing, deliberately; rates change by
// inserting a new rule version.

// RuleKey builds the stable identifier for a rule's lineage.
func RuleKey(scope RuleScope, match string) string {
	if scope == ScopeDefault {
		return "default"
	}
	return string(scope) + ":" + strings.ToLower(strings.TrimSpace(match))
}

// PricingContext is what a rule is matched against.
type PricingContext struct {
	Specialty       string
	CorporateClient string
}

// Pricer selects the commission rule for a payment and computes the split.
//
// It caches the active rule set because the payout job and the webhook path
// both price on every call and the rule table changes a handful of times a
// year. The cache is bounded by time, not by size; a rate change is visible
// within one TTL on every replica without a deploy.
type Pricer struct {
	store Store
	ttl   time.Duration

	mu        sync.RWMutex
	rules     []CommissionRule
	loadedAt  time.Time
	fallback  CommissionRule
	hasLoaded bool

	// byID caches rule versions forever, and that is safe precisely because a
	// rule version is immutable: superseding a rule inserts a new row, it never
	// edits this one.
	//
	// The cache is not an optimisation for its own sake. Repricing at capture
	// time happens inside a database transaction, and reaching back into the
	// pool for a second connection while holding one is how a saturated pool
	// deadlocks. An in-memory hit means the transaction never needs one.
	byID map[uuid.UUID]CommissionRule
}

// NewPricer builds a Pricer. The fallback rule is used only when the database
// contains no matching rule at all, which after seeding should never happen;
// it exists so that a pricing lookup can fail closed with a sane rate rather
// than fail open with a zero commission.
func NewPricer(store Store, ttl time.Duration, fallback CommissionRule) *Pricer {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &Pricer{store: store, ttl: ttl, fallback: fallback, byID: map[uuid.UUID]CommissionRule{}}
}

// Refresh reloads the active rules immediately.
func (p *Pricer) Refresh(ctx context.Context) error {
	rules, err := p.store.ActiveRules(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.rules = rules
	p.loadedAt = time.Now()
	p.hasLoaded = true
	for i := range rules {
		p.byID[rules[i].ID] = rules[i]
	}
	p.mu.Unlock()
	return nil
}

// RuleByID returns one specific rule version, preferring the immutable cache.
//
// This is the lookup that makes a historical invoice reproducible, and the one
// that repricing at capture time uses. It must be callable from inside a
// database transaction without acquiring a second pooled connection, which is
// why the cache is consulted first and populated on every miss.
func (p *Pricer) RuleByID(ctx context.Context, id uuid.UUID) (CommissionRule, error) {
	if id == uuid.Nil {
		return CommissionRule{}, ErrNotFound
	}
	p.mu.RLock()
	r, ok := p.byID[id]
	p.mu.RUnlock()
	if ok {
		return r, nil
	}

	r, err := p.store.GetRule(ctx, id)
	if err != nil {
		return CommissionRule{}, err
	}
	p.mu.Lock()
	p.byID[id] = r
	p.mu.Unlock()
	return r, nil
}

func (p *Pricer) current(ctx context.Context) ([]CommissionRule, error) {
	p.mu.RLock()
	fresh := p.hasLoaded && time.Since(p.loadedAt) < p.ttl
	rules := p.rules
	p.mu.RUnlock()
	if fresh {
		return rules, nil
	}
	if err := p.Refresh(ctx); err != nil {
		// A stale rule set prices correctly enough to keep taking payments
		// through a brief database blip; an empty one does not.
		if len(rules) > 0 {
			return rules, nil
		}
		return nil, err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.rules, nil
}

// Select returns the most specific active rule for a pricing context.
// Precedence is corporate > specialty > default; within a scope, the highest
// version wins, which is by construction the only open one.
func (p *Pricer) Select(ctx context.Context, pc PricingContext) (CommissionRule, error) {
	rules, err := p.current(ctx)
	if err != nil {
		return CommissionRule{}, err
	}

	var best CommissionRule
	bestRank := -1
	for i := range rules {
		r := &rules[i]
		if !matches(r, pc) {
			continue
		}
		rank := r.Scope.Precedence()
		if rank > bestRank || (rank == bestRank && r.Version > best.Version) {
			best, bestRank = *r, rank
		}
	}
	if bestRank < 0 {
		if p.fallback.RateBps == 0 && p.fallback.RuleKey == "" {
			return CommissionRule{}, fmt.Errorf("payment: no commission rule matches specialty=%q corporate=%q and no fallback configured",
				pc.Specialty, pc.CorporateClient)
		}
		return p.fallback, nil
	}
	return best, nil
}

func matches(r *CommissionRule, pc PricingContext) bool {
	switch r.Scope {
	case ScopeDefault:
		return true
	case ScopeSpecialty:
		return pc.Specialty != "" && strings.EqualFold(r.MatchValue, pc.Specialty)
	case ScopeCorporate:
		return pc.CorporateClient != "" && strings.EqualFold(r.MatchValue, pc.CorporateClient)
	default:
		return false
	}
}

// Price selects a rule and computes the split in one step.
func (p *Pricer) Price(ctx context.Context, amountCents int64, pc PricingContext) (Split, CommissionRule, error) {
	rule, err := p.Select(ctx, pc)
	if err != nil {
		return Split{}, CommissionRule{}, err
	}
	split, err := ComputeSplit(amountCents, rule)
	if err != nil {
		return Split{}, CommissionRule{}, err
	}
	return split, rule, nil
}

// --- bootstrap seed --------------------------------------------------------

// seedEntry is one element of the COMMISSION_RULES env blob. The shape is the
// one the source documentation specifies, extended with the fields it forgot:
// a provider fee, a fixed fee component, and a rounding mode.
type seedEntry struct {
	Default          bool        `json:"default"`
	Specialty        string      `json:"specialty"`
	CorporateClient  string      `json:"corporate_client"`
	Commission       json.Number `json:"commission"`
	ProviderFee      json.Number `json:"provider_fee"`
	ProviderFeeFixed int64       `json:"provider_fee_fixed_cents"`
	Rounding         string      `json:"rounding"`
	Note             string      `json:"note"`
}

// ParseSeedRules turns the COMMISSION_RULES environment blob into version-1
// rule rows. It is only ever used to populate an empty commission_rules table.
func ParseSeedRules(raw string, now time.Time) ([]CommissionRule, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var entries []seedEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("payment: COMMISSION_RULES is not a JSON array of rules: %w", err)
	}

	seen := map[string]bool{}
	out := make([]CommissionRule, 0, len(entries))
	for i, e := range entries {
		var scope RuleScope
		var match string
		switch {
		case e.CorporateClient != "":
			scope, match = ScopeCorporate, e.CorporateClient
		case e.Specialty != "":
			scope, match = ScopeSpecialty, e.Specialty
		case e.Default:
			scope, match = ScopeDefault, ""
		default:
			return nil, fmt.Errorf("payment: COMMISSION_RULES[%d] names neither a specialty, a corporate_client, nor default", i)
		}

		rateBps, err := PercentToBps(e.Commission.String())
		if err != nil {
			return nil, fmt.Errorf("payment: COMMISSION_RULES[%d] commission: %w", i, err)
		}
		feeBps := 0
		if e.ProviderFee.String() != "" {
			if feeBps, err = PercentToBps(e.ProviderFee.String()); err != nil {
				return nil, fmt.Errorf("payment: COMMISSION_RULES[%d] provider_fee: %w", i, err)
			}
		}
		rounding := Rounding(strings.TrimSpace(e.Rounding))
		if rounding == "" {
			rounding = DefaultRounding
		}
		if !rounding.Valid() {
			return nil, fmt.Errorf("payment: COMMISSION_RULES[%d] rounding %q is not half_up or half_even", i, rounding)
		}

		key := RuleKey(scope, match)
		if seen[key] {
			return nil, fmt.Errorf("payment: COMMISSION_RULES declares %q twice", key)
		}
		seen[key] = true

		out = append(out, CommissionRule{
			ID:                    uuid.New(),
			RuleKey:               key,
			Version:               1,
			Scope:                 scope,
			MatchValue:            match,
			RateBps:               rateBps,
			ProviderFeeBps:        feeBps,
			ProviderFeeFixedCents: e.ProviderFeeFixed,
			Rounding:              rounding,
			EffectiveFrom:         now,
			Note:                  firstNonEmpty(e.Note, "seeded from COMMISSION_RULES at first boot"),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].RuleKey < out[j].RuleKey })
	return out, nil
}

// PercentToBps converts a decimal percentage string to integer basis points
// without ever constructing a float. "20" -> 2000, "17.5" -> 1750,
// "2.75" -> 275. More than two decimal places is an error rather than a silent
// truncation, because silently dropping a digit from a rate is how a platform
// discovers it has been undercharging for a year.
func PercentToBps(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("percentage is empty")
	}
	neg := strings.HasPrefix(s, "-")
	if neg {
		return 0, fmt.Errorf("percentage %q must not be negative", s)
	}
	s = strings.TrimPrefix(s, "+")

	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	w, err := strconv.Atoi(whole)
	if err != nil {
		return 0, fmt.Errorf("percentage %q is not a number", s)
	}
	bps := w * 100

	if hasFrac {
		switch {
		case frac == "":
			// "20." is tolerated as "20".
		case len(frac) <= 2:
			padded := frac + strings.Repeat("0", 2-len(frac))
			f, err := strconv.Atoi(padded)
			if err != nil {
				return 0, fmt.Errorf("percentage %q is not a number", s)
			}
			bps += f
		default:
			return 0, fmt.Errorf("percentage %q has more than two decimal places", s)
		}
	}
	if bps < 0 || bps > BasisPointDenominator {
		return 0, fmt.Errorf("percentage %q is outside 0-100", s)
	}
	return bps, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
