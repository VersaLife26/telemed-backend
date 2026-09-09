package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"telemed/internal/platform/cache"
)

// brokenCache is a cache.Cache whose Incr always fails, standing in for a
// Redis outage. Every other method is unreachable from RateLimit.
type brokenCache struct{ cache.Cache }

func (brokenCache) Incr(context.Context, string, time.Duration) (int64, error) {
	return 0, errors.New("redis: connection refused")
}

// TestRateLimitFailClosed pins SECURITY-REVIEW F24.
//
// The limiter passed every request through whenever Redis errored. That is
// the right default for authenticated traffic and the wrong one for
// /webhooks/*, which is unauthenticated, writes to the database, accepts
// 256 KiB per request, and has no other throttle in front of it -- so a Redis
// blip removed the only control.
func TestRateLimitFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		failClosed bool
		wantStatus int
		wantServed bool
	}{
		{
			name:       "authenticated buckets still fail open",
			failClosed: false,
			wantStatus: http.StatusOK,
			wantServed: true,
		},
		{
			name:       "the webhook bucket sheds instead",
			failClosed: true,
			wantStatus: http.StatusServiceUnavailable,
			wantServed: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			served := false
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				served = true
				w.WriteHeader(http.StatusOK)
			})

			h := RateLimit(brokenCache{}, RateLimitConfig{
				Name:       "webhooks",
				Requests:   600,
				Window:     time.Minute,
				FailClosed: tc.failClosed,
			}, zerolog.Nop())(next)

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhooks/stripe", http.NoBody)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if served != tc.wantServed {
				t.Errorf("handler reached = %v, want %v", served, tc.wantServed)
			}
		})
	}
}

// TestRateLimitFailClosedIsNotTheDefault states the default explicitly, so
// that flipping it platform-wide is a deliberate act with a failing test
// rather than a quiet change of blast radius.
func TestRateLimitFailClosedIsNotTheDefault(t *testing.T) {
	if (RateLimitConfig{}).FailClosed {
		t.Fatal("FailClosed must default to false: authenticated traffic fails open by design")
	}
}
