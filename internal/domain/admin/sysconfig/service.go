package sysconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"sort"
	"time"

	"github.com/google/uuid"

	"telemed/internal/domain/admin/audit"
	"telemed/internal/platform/middleware"
)

// keyPattern keeps config keys predictable: lowercase, dot/underscore
// separated, e.g. "commission_rules", "feature.waitlist_v2",
// "cancellation_policy". Anything else is rejected before it reaches SQL.
var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)

// backoff returns a small jittered delay that grows with attempt, capped at
// 20ms. The jitter is what keeps N concurrent writers from retrying in
// lockstep and re-colliding on every single attempt.
func backoff(attempt int) time.Duration {
	base := min(1<<attempt, 20)
	// G404: math/rand/v2 is correct here. This jitter decorrelates concurrent
	// retries of an optimistic-lock conflict; it guards nothing, is never a
	// token, a key or a nonce, and an attacker who could predict it would gain
	// only the ability to know when a losing writer retries.
	return time.Duration(base) * time.Millisecond * time.Duration(1+rand.IntN(3)) //nolint:gosec // G404: retry jitter, not a security decision
}

var ErrInvalidKey = errors.New("sysconfig: key must match ^[a-z][a-z0-9_]*(\\.[a-z][a-z0-9_]*)*$")

// ErrUnknownKey is a key that is well-formed but is not one this platform
// knows about.
var ErrUnknownKey = errors.New("sysconfig: unknown configuration key")

// ErrKeyNotPermitted is a known key the caller's role may not write.
var ErrKeyNotPermitted = errors.New("sysconfig: your role may not write this configuration key")

// writePolicy is the allowlist of writable configuration keys and, for each,
// the roles permitted to write it.
//
// SECURITY-REVIEW F18 -- and the reason this is an allowlist rather than a
// denylist.
//
// GroupFinance is {finance, super_admin}, commented "support and ops staff
// have no legitimate reason to move money or edit commission rules". But the
// commission rule is STORED as sysconfig key "commission_rules", Put validated
// the key against keyPattern only, and GroupConfig is
// {super_admin, ops, finance}. So an ops admin who got a 403 from
// PUT /admin/finance/commission-rules simply called
// PUT /admin/configs/commission_rules with {"value":{"default_percent":0}} and
// wrote the same row -- logged as a generic "config.updated", invisible in a
// review of finance actions.
//
// The check belongs here rather than in the router because two routers reach
// this row. A client-side or route-level guard closes one of them; curl still
// gets the other.
//
// A key absent from this map cannot be written at all. Adding a feature flag
// is a code change, which is the right amount of friction for a table whose
// rows change how money is split.
var writePolicy = map[string][]middleware.Role{
	// Money. Finance owns it; ops and support have no business here.
	"commission_rules": {middleware.RoleFinance, middleware.RoleSuperAdmin},
	// Operational policy: how much notice a cancellation needs. Ops owns the
	// day-to-day setting, finance sees the refund consequence.
	"cancellation_policy": {middleware.RoleOps, middleware.RoleFinance, middleware.RoleSuperAdmin},
	// Slot generation defaults applied when a doctor has not set their own.
	"slot_defaults": {middleware.RoleOps, middleware.RoleSuperAdmin},
	// Consultation fee bounds. Financial policy, same owners as commission.
	"fee_caps": {middleware.RoleFinance, middleware.RoleSuperAdmin},
	// Feature flags. Ops turns things on and off.
	"feature_flags": {middleware.RoleOps, middleware.RoleSuperAdmin},
	// Corporate cover roster. Ops manages relationships; finance sees discounts.
	"corporate_clients": {middleware.RoleOps, middleware.RoleFinance, middleware.RoleSuperAdmin},
	// Legacy per-flag key kept for curl examples in API.md.
	"feature.waitlist_v2": {middleware.RoleOps, middleware.RoleSuperAdmin},
}

// WritableKeys is every key Put accepts, for the error message and for tests.
func WritableKeys() []string {
	out := make([]string, 0, len(writePolicy))
	for k := range writePolicy {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// authoriseWrite applies writePolicy.
func authoriseWrite(p middleware.Principal, key string) error {
	roles, known := writePolicy[key]
	if !known {
		return fmt.Errorf("%w: %q; writable keys are %v", ErrUnknownKey, key, WritableKeys())
	}
	// HasAdminRole, not HasAnyRole: an administrative role is only honoured
	// from the admin issuer, and this decision is made in the service layer,
	// which never passes through RequireRole.
	if !p.HasAdminRole(roles...) {
		return fmt.Errorf("%w: %q requires one of %v", ErrKeyNotPermitted, key, roles)
	}
	return nil
}

type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service { return &Service{repo: repo} }

// Get returns the currently-effective value for key.
func (s *Service) Get(ctx context.Context, key string) (Config, error) {
	if !keyPattern.MatchString(key) {
		return Config{}, ErrInvalidKey
	}
	return s.repo.Current(ctx, key, time.Time{})
}

// History returns every version of key, newest first.
func (s *Service) History(ctx context.Context, key string) ([]Config, error) {
	if !keyPattern.MatchString(key) {
		return nil, ErrInvalidKey
	}
	return s.repo.History(ctx, key)
}

// CurrentAll returns every key's currently-effective value.
func (s *Service) CurrentAll(ctx context.Context) ([]Config, error) {
	return s.repo.CurrentAll(ctx)
}

// Put inserts a new version of key, effective at effectiveFrom (defaulting
// to now, i.e. immediately). It retries a handful of times on ErrVersionRace
// -- two admins editing the same key within the same instant is rare but not
// impossible, and Postgres's UNIQUE(key, version) is what makes the race
// detectable at all.
func (s *Service) Put(ctx context.Context, caller middleware.Principal, actorID uuid.UUID, key string, value json.RawMessage, effectiveFrom time.Time) (Config, error) {
	if !keyPattern.MatchString(key) {
		return Config{}, ErrInvalidKey
	}
	if err := authoriseWrite(caller, key); err != nil {
		return Config{}, err
	}
	if !json.Valid(value) {
		return Config{}, fmt.Errorf("sysconfig: value must be valid JSON")
	}

	previous, err := s.repo.Current(ctx, key, time.Time{})
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Config{}, err
	}

	// maxAttempts is generous on purpose: this is a handful of admins editing
	// the same key within the same instant, not a hot request path, so
	// trading a little latency for a very low false-failure probability is
	// the right side to err on. A small jittered backoff between attempts
	// keeps N concurrent writers from retrying in lockstep and re-colliding
	// on every attempt.
	const maxAttempts = 20
	var cfg Config
	for attempt := 0; attempt < maxAttempts; attempt++ {
		cfg, err = s.repo.Put(ctx, key, value, actorID, effectiveFrom)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrVersionRace) {
			return Config{}, err
		}
		select {
		case <-ctx.Done():
			return Config{}, ctx.Err()
		case <-time.After(backoff(attempt)):
		}
	}
	if err != nil {
		return Config{}, fmt.Errorf("sysconfig: put %s: %w", key, err)
	}

	var oldVal any
	if previous.Value != nil {
		oldVal = previous.Value
	}
	audit.Stage(ctx, audit.Draft{
		Action:       "config.updated",
		ResourceType: "system_config",
		ResourceID:   key,
		OldValue:     oldVal,
		NewValue:     value,
	})
	return cfg, nil
}
