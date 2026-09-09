package user

import (
	"context"

	"telemed/internal/platform/cache"
)

// otpLimiter enforces the OTP front door's two Redis-backed counters: the
// per-phone send budget and the per-code verify-attempt budget. It depends
// only on cache.Cache -- not on the repository or any other part of the
// service -- so it is fully unit-testable against an in-memory fake with no
// database and no network.
type otpLimiter struct {
	cache cache.Cache
}

func newOTPLimiter(c cache.Cache) *otpLimiter { return &otpLimiter{cache: c} }

// allowSend enforces "3 sends per phone per hour" as a Redis fixed window
// (Cache.Incr applies the TTL only on the window's first hit, so the window
// does not slide forward on every request the way a naive
// read-then-write implementation would). It returns the count for this
// window including the current call, and whether the caller may proceed.
func (l *otpLimiter) allowSend(ctx context.Context, phone string) (count int64, allowed bool, err error) {
	n, err := l.cache.Incr(ctx, otpSendCacheKey(phone), otpSendLimitWindow)
	if err != nil {
		return 0, false, err
	}
	return n, n <= otpSendLimitPerPhone, nil
}

// registerVerifyAttempt enforces "cap verify attempts at 5 per OTP, then
// invalidate". It returns the attempt count and whether this attempt pushed
// the caller over the cap. When locked is true, the caller is responsible
// for deleting the OTP hash itself (the "invalidate" half of the rule) --
// a limiter that only counted attempts without destroying the code would
// still be brute-forceable once the window rolled over.
func (l *otpLimiter) registerVerifyAttempt(ctx context.Context, phone string) (count int64, locked bool, err error) {
	n, err := l.cache.Incr(ctx, otpAttemptsCacheKey(phone), otpTTL)
	if err != nil {
		return 0, false, err
	}
	return n, n > otpVerifyMaxAttempts, nil
}
