package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestPhoneRateLimit_BoundaryAndHeaders(t *testing.T) {
	c := newFakeCache()
	spec := tierSpec{Requests: 3, Window: time.Minute}
	mw := phoneRateLimit(c, "otp_send_phone", "phone", spec, zerolog.Nop())

	var upstreamCalls int
	var lastBody []byte
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		b, _ := io.ReadAll(r.Body)
		lastBody = b
		w.WriteHeader(http.StatusOK)
	})
	handler := mw(next)

	body := `{"phone":"+94771234567","purpose":"login"}`

	// First 3 requests (== the limit) must succeed.
	for i := 1; i <= 3; i++ {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/otp/send", bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, rec.Code)
		}
		wantRemaining := strconv.Itoa(3 - i)
		if got := rec.Header().Get("X-RateLimit-Remaining"); got != wantRemaining {
			t.Fatalf("request %d: X-RateLimit-Remaining = %q, want %q", i, got, wantRemaining)
		}
		if got := rec.Header().Get("X-RateLimit-Limit"); got != "3" {
			t.Fatalf("request %d: X-RateLimit-Limit = %q, want 3", i, got)
		}
		// The body must reach the upstream unmodified, even though the
		// middleware read it to find the phone number.
		if string(lastBody) != body {
			t.Fatalf("request %d: upstream saw body %q, want %q", i, lastBody, body)
		}
	}

	// The 4th request from the SAME phone must be rejected.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/otp/send", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("4th request: status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 response must carry Retry-After")
	}

	if upstreamCalls != 3 {
		t.Fatalf("upstream was called %d times, want 3 (the 4th must never reach it)", upstreamCalls)
	}
}

func TestPhoneRateLimit_DifferentPhonesAreIndependentBuckets(t *testing.T) {
	c := newFakeCache()
	spec := tierSpec{Requests: 1, Window: time.Minute}
	mw := phoneRateLimit(c, "otp_send_phone", "phone", spec, zerolog.Nop())
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := mw(next)

	for _, phone := range []string{"+94771234567", "+94779999999"} {
		body := `{"phone":"` + phone + `"}`
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/otp/send", bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("phone %s: status = %d, want 200 (each phone gets its own bucket)", phone, rec.Code)
		}
	}
}

func TestPhoneRateLimit_MalformedBodyFallsBackToIPBucket(t *testing.T) {
	c := newFakeCache()
	spec := tierSpec{Requests: 1, Window: time.Minute}
	mw := phoneRateLimit(c, "otp_send_phone", "phone", spec, zerolog.Nop())
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := mw(next)

	// Not JSON at all: must still consume rate-limit budget rather than
	// bypassing the limiter.
	req1 := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/otp/send", bytes.NewBufferString("not json"))
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first malformed request: status = %d, want 200", rec1.Code)
	}

	req2 := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/otp/send", bytes.NewBufferString("still not json"))
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second malformed request: status = %d, want 429 (fallback bucket must still enforce the limit)", rec2.Code)
	}
}

func TestBuildRateLimiters_StrictOTPYieldsTwoLimiters(t *testing.T) {
	mws := buildRateLimiters(RateLimitStrictOTP, "phone", newFakeCache(), zerolog.Nop())
	if len(mws) != 2 {
		t.Fatalf("strict_otp yielded %d middlewares, want 2 (per-IP and per-phone)", len(mws))
	}
}

func TestBuildRateLimiters_OtherTiersYieldOneLimiter(t *testing.T) {
	for _, tier := range []RateLimitTier{RateLimitModerate, RateLimitGenerous, RateLimitAdmin, RateLimitWebhook} {
		mws := buildRateLimiters(tier, "", newFakeCache(), zerolog.Nop())
		if len(mws) != 1 {
			t.Fatalf("tier %s yielded %d middlewares, want 1", tier, len(mws))
		}
	}
}
