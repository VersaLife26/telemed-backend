package gateway

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/rs/zerolog"

	"telemed/internal/platform/cache"
)

// BreakerState is the externally observable state of a circuit breaker.
type BreakerState string

const (
	StateClosed   BreakerState = "closed"
	StateOpen     BreakerState = "open"
	StateHalfOpen BreakerState = "half_open"
)

// BreakerConfig tunes one upstream's breaker.
type BreakerConfig struct {
	// Name identifies the upstream; it namespaces every Redis key.
	Name string
	// FailureThreshold consecutive failures trip the breaker open.
	FailureThreshold int64
	// FailureWindow bounds how long a failure streak is remembered. A
	// failure older than this does not count toward the threshold, so a
	// slow trickle of unrelated errors over a day cannot trip the breaker.
	FailureWindow time.Duration
	// OpenDuration is how long the breaker stays fully open (rejecting every
	// request with no upstream traffic at all) before allowing one probe.
	OpenDuration time.Duration
	// ProbeGrace bounds how long a single probe attempt has to report back
	// before another caller is allowed to try. Keep it close to the route's
	// own request timeout.
	ProbeGrace time.Duration
}

func (c BreakerConfig) withDefaults() BreakerConfig {
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = 5
	}
	if c.FailureWindow <= 0 {
		c.FailureWindow = 30 * time.Second
	}
	if c.OpenDuration <= 0 {
		c.OpenDuration = 30 * time.Second
	}
	if c.ProbeGrace <= 0 {
		c.ProbeGrace = 5 * time.Second
	}
	return c
}

// breakerCache is the slice of cache.Cache the breaker actually needs. Every
// *cache.RedisCache satisfies it structurally; tests supply a tiny in-memory
// fake instead of standing up Redis.
type breakerCache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Del(ctx context.Context, keys ...string) error
	Incr(ctx context.Context, key string, ttl time.Duration) (int64, error)
	Lock(ctx context.Context, key string, ttl time.Duration) (token string, acquired bool, err error)
}

var _ breakerCache = (*cache.RedisCache)(nil)

// Breaker is a per-upstream circuit breaker whose state lives in Redis, so
// every gateway replica agrees on whether an upstream is open without any
// direct communication between pods -- exactly the "shared circuit-breaker
// state" the brief calls for.
//
// State transitions:
//
//	closed --(FailureThreshold consecutive failures)--> open
//	open   --(OpenDuration elapses, one caller wins the probe lock)--> half_open
//	half_open --(probe succeeds)--> closed
//	half_open --(probe fails)--> open
//
// A Redis outage fails the breaker OPEN to closed (i.e. it lets traffic
// through) rather than wedging every upstream call behind a dependency the
// gateway does not otherwise require; the outage is logged so it is visible
// operationally.
type Breaker struct {
	cfg     BreakerConfig
	cache   breakerCache
	log     zerolog.Logger
	onState func(BreakerState)
}

// NewBreaker builds a breaker backed by c.
func NewBreaker(c breakerCache, cfg BreakerConfig, log zerolog.Logger) *Breaker {
	return &Breaker{cfg: cfg.withDefaults(), cache: c, log: log.With().Str("breaker", cfg.Name).Logger()}
}

// OnState registers a callback invoked with the observed state every time
// Allow or Report computes one. It is how the gateway keeps a Prometheus
// circuit-state gauge current without an extra Redis round trip per request.
func (b *Breaker) OnState(fn func(BreakerState)) { b.onState = fn }

func (b *Breaker) notify(state BreakerState) {
	if b.onState != nil {
		b.onState(state)
	}
}

func (b *Breaker) openKey() string  { return "gw:cb:" + b.cfg.Name + ":open" }
func (b *Breaker) failKey() string  { return "gw:cb:" + b.cfg.Name + ":fails" }
func (b *Breaker) probeKey() string { return "gw:cb:" + b.cfg.Name + ":probe" }

// Allow reports whether a request may proceed and the state it proceeds
// under. Callers MUST pass the returned state to Report once the request
// finishes, because half-open results are interpreted differently from
// closed-state results (a single half-open failure reopens immediately; a
// closed-state failure only reopens once FailureThreshold is reached).
func (b *Breaker) Allow(ctx context.Context) (allowed bool, state BreakerState) {
	defer func() { b.notify(state) }()

	raw, err := b.cache.Get(ctx, b.openKey())
	switch {
	case errors.Is(err, cache.ErrNotFound):
		// Never opened, or long since recovered: the cheap, common path.
		return true, StateClosed
	case err != nil:
		b.log.Error().Err(err).Msg("breaker store unavailable, failing open")
		return true, StateClosed
	}

	openedAt, perr := strconv.ParseInt(string(raw), 10, 64)
	if perr != nil {
		// Corrupt value: do not let a bad key wedge the breaker open forever.
		b.log.Warn().Str("value", string(raw)).Msg("breaker: unparseable open marker, treating as closed")
		return true, StateClosed
	}

	elapsed := time.Since(time.Unix(0, openedAt))
	if elapsed < b.cfg.OpenDuration {
		return false, StateOpen
	}

	// Cooldown has elapsed. Exactly one caller gets to probe; the rest are
	// still rejected until the probe reports back.
	_, acquired, lockErr := b.cache.Lock(ctx, b.probeKey(), b.cfg.ProbeGrace)
	if lockErr != nil {
		b.log.Error().Err(lockErr).Msg("breaker probe lock unavailable, failing open")
		return true, StateHalfOpen
	}
	if !acquired {
		return false, StateHalfOpen
	}
	return true, StateHalfOpen
}

// Report records the outcome of a request that Allow let through.
func (b *Breaker) Report(ctx context.Context, state BreakerState, success bool) {
	if success {
		if err := b.cache.Del(ctx, b.openKey(), b.failKey()); err != nil {
			b.log.Error().Err(err).Msg("breaker: reset on success failed")
			return
		}
		b.notify(StateClosed)
		return
	}

	if state == StateHalfOpen {
		// A probe failure reopens immediately -- no need to accumulate a
		// fresh streak of FailureThreshold failures first.
		b.trip(ctx)
		return
	}

	n, err := b.cache.Incr(ctx, b.failKey(), b.cfg.FailureWindow)
	if err != nil {
		b.log.Error().Err(err).Msg("breaker: failure counter unavailable")
		return
	}
	if n >= b.cfg.FailureThreshold {
		b.trip(ctx)
	}
}

func (b *Breaker) trip(ctx context.Context) {
	now := fmt.Sprintf("%d", time.Now().UnixNano())
	if err := b.cache.Set(ctx, b.openKey(), []byte(now), b.cfg.OpenDuration+b.cfg.ProbeGrace); err != nil {
		b.log.Error().Err(err).Msg("breaker: failed to open circuit")
		return
	}
	_ = b.cache.Del(ctx, b.failKey())
	b.notify(StateOpen)
	b.log.Warn().Msg("circuit breaker open")
}

// Snapshot returns the current state without mutating anything (no probe
// lock is attempted), for read-only reporting such as /health/ready and
// Prometheus metrics.
func (b *Breaker) Snapshot(ctx context.Context) BreakerState {
	raw, err := b.cache.Get(ctx, b.openKey())
	if errors.Is(err, cache.ErrNotFound) {
		return StateClosed
	}
	if err != nil {
		return StateClosed // see Allow: a degraded store must not paint every upstream red
	}
	openedAt, perr := strconv.ParseInt(string(raw), 10, 64)
	if perr != nil {
		return StateClosed
	}
	if time.Since(time.Unix(0, openedAt)) < b.cfg.OpenDuration {
		return StateOpen
	}
	return StateHalfOpen
}

// Name returns the upstream name this breaker guards.
func (b *Breaker) Name() string { return b.cfg.Name }
