package user

import (
	"context"
	"testing"
)

// TestOTPLimiter_SendBoundary asserts the exact boundary AGENT-BRIEF calls
// for: the 3rd send in a window succeeds, the 4th is rejected. Off-by-one
// errors here are the entire ballgame for a fixed-window limiter.
func TestOTPLimiter_SendBoundary(t *testing.T) {
	tests := []struct {
		attempt int
		wantOK  bool
	}{
		{1, true},
		{2, true},
		{3, true},
		{4, false},
		{5, false}, // stays rejected, does not "reopen"
	}

	l := newOTPLimiter(newFakeCache())
	ctx := context.Background()
	const phone = "+94771234567"

	for _, tt := range tests {
		n, allowed, err := l.allowSend(ctx, phone)
		if err != nil {
			t.Fatalf("attempt %d: unexpected error: %v", tt.attempt, err)
		}
		if int64(tt.attempt) != n {
			t.Fatalf("attempt %d: counter = %d, want %d", tt.attempt, n, tt.attempt)
		}
		if allowed != tt.wantOK {
			t.Errorf("attempt %d: allowed = %v, want %v", tt.attempt, allowed, tt.wantOK)
		}
	}
}

// TestOTPLimiter_SendIsolatedPerPhone confirms the limiter keys on phone, so
// one caller's budget cannot be exhausted by traffic to a different number.
func TestOTPLimiter_SendIsolatedPerPhone(t *testing.T) {
	l := newOTPLimiter(newFakeCache())
	ctx := context.Background()

	for range 3 {
		if _, allowed, err := l.allowSend(ctx, "+94771111111"); err != nil || !allowed {
			t.Fatalf("phone A: got allowed=%v err=%v, want true/nil", allowed, err)
		}
	}
	// Phone A is now exhausted; phone B must be unaffected.
	if _, allowed, err := l.allowSend(ctx, "+94772222222"); err != nil || !allowed {
		t.Fatalf("phone B first send: got allowed=%v err=%v, want true/nil", allowed, err)
	}
}

// TestOTPLimiter_VerifyAttemptLockout asserts the 5-attempts-then-invalidate
// rule: attempts 1-5 are counted but not yet locked, attempt 6 is the one
// that reports locked=true, at which point the caller destroys the OTP hash.
func TestOTPLimiter_VerifyAttemptLockout(t *testing.T) {
	l := newOTPLimiter(newFakeCache())
	ctx := context.Background()
	const phone = "+94771234567"

	for attempt := 1; attempt <= otpVerifyMaxAttempts; attempt++ {
		_, locked, err := l.registerVerifyAttempt(ctx, phone)
		if err != nil {
			t.Fatalf("attempt %d: unexpected error: %v", attempt, err)
		}
		if locked {
			t.Fatalf("attempt %d: locked=true, want false (cap is %d)", attempt, otpVerifyMaxAttempts)
		}
	}

	n, locked, err := l.registerVerifyAttempt(ctx, phone)
	if err != nil {
		t.Fatalf("attempt %d: unexpected error: %v", otpVerifyMaxAttempts+1, err)
	}
	if !locked {
		t.Fatalf("attempt %d: locked=false, want true", otpVerifyMaxAttempts+1)
	}
	if n != int64(otpVerifyMaxAttempts+1) {
		t.Errorf("counter = %d, want %d", n, otpVerifyMaxAttempts+1)
	}
}
