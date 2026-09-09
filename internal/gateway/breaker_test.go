package gateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func testBreaker(c breakerCache) *Breaker {
	return NewBreaker(c, BreakerConfig{
		Name:             "test-upstream",
		FailureThreshold: 3,
		FailureWindow:    time.Second,
		OpenDuration:     60 * time.Millisecond,
		ProbeGrace:       200 * time.Millisecond,
	}, zerolog.Nop())
}

func TestBreaker_ClosedByDefault(t *testing.T) {
	b := testBreaker(newFakeCache())
	allowed, state := b.Allow(context.Background())
	if !allowed || state != StateClosed {
		t.Fatalf("fresh breaker: got allowed=%v state=%v, want true/closed", allowed, state)
	}
}

func TestBreaker_OpensAfterConsecutiveFailures(t *testing.T) {
	ctx := context.Background()
	b := testBreaker(newFakeCache())

	for i := 0; i < 3; i++ {
		allowed, state := b.Allow(ctx)
		if !allowed {
			t.Fatalf("failure %d: request should have been allowed before the breaker trips", i)
		}
		b.Report(ctx, state, false)
	}

	allowed, state := b.Allow(ctx)
	if allowed || state != StateOpen {
		t.Fatalf("after %d consecutive failures: got allowed=%v state=%v, want false/open", 3, allowed, state)
	}
	if got := b.Snapshot(ctx); got != StateOpen {
		t.Fatalf("Snapshot() = %v, want open", got)
	}
}

func TestBreaker_RejectsEveryRequestWhileOpen(t *testing.T) {
	ctx := context.Background()
	b := testBreaker(newFakeCache())
	tripOpen(t, b)

	// Hammer it a few times immediately; none should be let through, and the
	// state must stay "open" (not spuriously flip to half_open early).
	for i := 0; i < 5; i++ {
		allowed, state := b.Allow(ctx)
		if allowed || state != StateOpen {
			t.Fatalf("attempt %d while cooling down: got allowed=%v state=%v, want false/open", i, allowed, state)
		}
	}
}

func TestBreaker_HalfOpenProbeSuccessCloses(t *testing.T) {
	ctx := context.Background()
	b := testBreaker(newFakeCache())
	tripOpen(t, b)

	// Wait out the cooldown.
	time.Sleep(80 * time.Millisecond)

	allowed, state := b.Allow(ctx)
	if !allowed || state != StateHalfOpen {
		t.Fatalf("after cooldown: got allowed=%v state=%v, want true/half_open (the probe)", allowed, state)
	}

	b.Report(ctx, state, true)

	if got := b.Snapshot(ctx); got != StateClosed {
		t.Fatalf("after a successful probe: Snapshot() = %v, want closed", got)
	}
	allowed, state = b.Allow(ctx)
	if !allowed || state != StateClosed {
		t.Fatalf("after recovery: got allowed=%v state=%v, want true/closed", allowed, state)
	}
}

func TestBreaker_HalfOpenProbeFailureReopensImmediately(t *testing.T) {
	ctx := context.Background()
	b := testBreaker(newFakeCache())
	tripOpen(t, b)

	time.Sleep(80 * time.Millisecond)

	allowed, state := b.Allow(ctx)
	if !allowed || state != StateHalfOpen {
		t.Fatalf("got allowed=%v state=%v, want true/half_open", allowed, state)
	}

	// A single half-open failure must reopen immediately -- it must NOT take
	// FailureThreshold more failures to trip again.
	b.Report(ctx, state, false)

	if got := b.Snapshot(ctx); got != StateOpen {
		t.Fatalf("after a failed probe: Snapshot() = %v, want open", got)
	}
	allowed, _ = b.Allow(ctx)
	if allowed {
		t.Fatal("breaker should reject immediately after a failed half-open probe")
	}
}

func TestBreaker_OnlyOneCallerProbesConcurrently(t *testing.T) {
	ctx := context.Background()
	b := testBreaker(newFakeCache())
	tripOpen(t, b)
	time.Sleep(80 * time.Millisecond)

	const n = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	probes := 0
	rejected := 0

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			allowed, state := b.Allow(ctx)
			mu.Lock()
			defer mu.Unlock()
			if allowed && state == StateHalfOpen {
				probes++
			} else if !allowed {
				rejected++
			}
		}()
	}
	wg.Wait()

	if probes != 1 {
		t.Fatalf("exactly one goroutine should have won the probe, got %d", probes)
	}
	if rejected != n-1 {
		t.Fatalf("the other %d goroutines should have been rejected, got %d", n-1, rejected)
	}
}

func TestBreaker_SuccessResetsFailureStreak(t *testing.T) {
	ctx := context.Background()
	b := testBreaker(newFakeCache())

	// Two failures, then a success: the streak must not carry over.
	for i := 0; i < 2; i++ {
		_, state := b.Allow(ctx)
		b.Report(ctx, state, false)
	}
	_, state := b.Allow(ctx)
	b.Report(ctx, state, true)

	// Two more failures should NOT be enough to trip (threshold is 3, streak
	// was reset).
	for i := 0; i < 2; i++ {
		allowed, s := b.Allow(ctx)
		if !allowed {
			t.Fatalf("breaker tripped early after streak reset (failure %d)", i)
		}
		b.Report(ctx, s, false)
	}
	allowed, _ := b.Allow(ctx)
	if !allowed {
		t.Fatal("breaker should still be closed: only 2 failures since the reset, threshold is 3")
	}
}

func TestBreaker_StoreOutageFailsOpen(t *testing.T) {
	ctx := context.Background()
	c := newFakeCache()
	b := testBreaker(c)
	tripOpen(t, b)

	c.mu.Lock()
	c.failGet = true
	c.mu.Unlock()

	// Even though the breaker was open, a Redis outage must not translate
	// into every upstream call being rejected -- the gateway needing Redis
	// for rate limiting/circuit state must not mean Redis being down takes
	// consultations down with it.
	allowed, state := b.Allow(ctx)
	if !allowed || state != StateClosed {
		t.Fatalf("during a store outage: got allowed=%v state=%v, want true/closed (fail open)", allowed, state)
	}
}

func TestBreaker_OnStateCallbackObservesTransitions(t *testing.T) {
	ctx := context.Background()
	b := testBreaker(newFakeCache())

	var mu sync.Mutex
	var seen []BreakerState
	b.OnState(func(s BreakerState) {
		mu.Lock()
		seen = append(seen, s)
		mu.Unlock()
	})

	for i := 0; i < 3; i++ {
		_, state := b.Allow(ctx)
		b.Report(ctx, state, false)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("OnState was never called")
	}
	if seen[len(seen)-1] != StateOpen {
		t.Fatalf("last observed state = %v, want open (breaker just tripped)", seen[len(seen)-1])
	}
}

func tripOpen(t *testing.T, b *Breaker) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		allowed, state := b.Allow(ctx)
		if !allowed {
			t.Fatalf("setup: request %d should have been allowed before the breaker trips", i)
		}
		b.Report(ctx, state, false)
	}
	if got := b.Snapshot(ctx); got != StateOpen {
		t.Fatalf("setup: breaker did not open, Snapshot() = %v", got)
	}
}
