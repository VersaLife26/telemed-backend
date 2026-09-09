package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestOTPPhoneBucket_OneNumberIsOneBucket pins the rate-limit fix.
//
// The per-phone OTP budget is 3 sends an hour. Keying it on the raw string the
// client typed made every spelling of one number its own budget, so a caller
// who alternated formats got 3 x (number of spellings they could think of)
// SMS messages -- each one costing real money and landing on a real
// subscriber's handset. Sri Lankan numbers have at least four everyday
// spellings, which is where the multiplier comes from.
func TestOTPPhoneBucket_OneNumberIsOneBucket(t *testing.T) {
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/otp/send", http.NoBody)

	spellings := []string{
		"+94771234567",
		"94771234567",
		"0771234567",
		"077 123 4567",
		"077-123-4567",
		"+94 (77) 123 4567",
	}

	want := otpPhoneBucket(spellings[0], r)
	for _, s := range spellings[1:] {
		if got := otpPhoneBucket(s, r); got != want {
			t.Errorf("otpPhoneBucket(%q) = %q, want %q -- this spelling gets its own "+
				"3-per-hour SMS budget", s, got, want)
		}
	}

	// Distinct numbers must still be distinct, or the fix would be a limiter
	// that throttles the whole platform onto one bucket.
	if otpPhoneBucket("+94771234567", r) == otpPhoneBucket("+94771234568", r) {
		t.Fatal("two different numbers collided into one bucket")
	}
}

// TestOTPPhoneBucket_KeyDoesNotContainTheNumber pins the second half: the
// bucket key is written to Redis, where it is visible to SCAN, MONITOR and
// every RDB/AOF snapshot. A phone number is personal data under the PDPA and
// has no business being retained in the clear by a rate limiter.
func TestOTPPhoneBucket_KeyDoesNotContainTheNumber(t *testing.T) {
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/otp/send", http.NoBody)

	for _, spelling := range []string{"+94771234567", "0771234567", "077 123 4567"} {
		key := otpPhoneBucket(spelling, r)
		for _, leak := range []string{"771234567", "0771234567", "+94771234567", "94771234567"} {
			if strings.Contains(key, leak) {
				t.Errorf("bucket key %q for input %q contains the subscriber number %q",
					key, spelling, leak)
			}
		}
	}
}

// TestOTPPhoneBucket_UnparseableStillSpendsBudget guards the obvious wrong
// fix: returning "" or skipping the limiter for a value that does not parse
// would make garbage input the way past the limiter.
func TestOTPPhoneBucket_UnparseableStillSpendsBudget(t *testing.T) {
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/otp/send", http.NoBody)
	r.RemoteAddr = "198.51.100.9:1234"

	key := otpPhoneBucket("not-a-phone-number", r)
	if key == "" {
		t.Fatal("an unparseable value produced an empty bucket key")
	}
	if !strings.HasPrefix(key, "phone-invalid:") {
		t.Fatalf("key = %q, want it scoped to the caller so one bad value does not "+
			"share a bucket with every other bad value on the platform", key)
	}
	if key == otpPhoneBucket("also-not-a-phone", httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/x", http.NoBody)) {
		t.Fatal("two different callers' malformed values share one bucket")
	}
}

// TestShedRouteLabel_IsBoundedAndCarriesNoIdentifier pins the metrics-label
// fix. /metrics is served unauthenticated on the listener the public ingress
// routes to, so a resource UUID in a label value is a resource UUID on the
// internet -- and an attacker-chosen label value is unbounded Prometheus
// cardinality, which is a memory-exhaustion DoS on top.
func TestShedRouteLabel_IsBoundedAndCarriesNoIdentifier(t *testing.T) {
	const (
		pattern = "/api/v1/records/{recordID}"
		id      = "b0a9f4c6-1f2e-4f3a-9c1d-2e5a7b8c9d01"
	)

	router := chi.NewRouter()
	var label string
	router.Get(pattern, func(w http.ResponseWriter, r *http.Request) {
		label = shedRouteLabel(r)
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/records/"+id, http.NoBody)
	router.ServeHTTP(httptest.NewRecorder(), req)

	if label != pattern {
		t.Fatalf("shed label = %q, want the route pattern %q", label, pattern)
	}
	if strings.Contains(label, id) {
		t.Fatalf("shed label %q carries the resource identifier onto an unauthenticated /metrics", label)
	}
}

// TestShedRouteLabel_UnroutedRequestStillGetsABoundedLabel covers the path
// that matters most for cardinality: a request that matched no route at all is
// exactly the one an attacker controls completely.
func TestShedRouteLabel_UnroutedRequestStillGetsABoundedLabel(t *testing.T) {
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/"+strings.Repeat("a", 512), http.NoBody)
	if got := shedRouteLabel(req); got != "unmatched" {
		t.Fatalf("shed label for an unrouted request = %q, want the constant %q", got, "unmatched")
	}
}
