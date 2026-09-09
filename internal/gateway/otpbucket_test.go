package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestOTP_SendAndVerifyDoNotShareOneBudget pins a defect that made the only
// login path on the platform fail for ordinary use.
//
// Both /auth/otp/send and /auth/otp/verify carried the strict_otp tier, and
// buildRateLimiters derived the bucket key from the tier alone -- no route
// discriminator anywhere in it. Both routes therefore incremented the SAME
// per-phone key, and the same per-IP key.
//
// The per-phone budget is 3 an hour, and it exists to bound SMS cost. Sharing
// it with verification spends it on requests that send no SMS at all: one
// successful login costs 2 (send + verify), and a single mistyped digit costs
// the third. The fourth request in the hour is a 429, from the gateway, before
// user-service is ever consulted -- so the platform's documented "5 attempts
// per code, then the code is destroyed" policy was unreachable, and a user who
// fat-fingered one digit was locked out of the product for an hour.
//
// The keys are the assertion, for the same reason as in
// ratelimit_principal_test.go: a status code cannot distinguish one shared
// bucket from two separate ones until the shared one happens to fill.
func TestOTP_SendAndVerifyDoNotShareOneBudget(t *testing.T) {
	ti := newTestIdentity(t)
	upSrv, _ := newEchoUpstream(t)
	r, _, c := buildTestRouter(t, ti, testConfig(), upSrv.URL, nil)

	body := `{"phone":"+94771234567","purpose":"login"}`
	for _, path := range []string{"/api/v1/auth/otp/send", "/api/v1/auth/otp/verify"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body = %s", path, rec.Code, rec.Body.String())
		}
	}

	keys := c.rateLimitKeys()
	if len(keys) == 0 {
		t.Fatal("no rate-limit bucket was touched on the OTP routes at all")
	}

	// Every key touched by send must differ from every key touched by verify.
	// Both requests carried the same phone number and came from the same
	// address, so if the two routes are budgeted separately the ONLY thing
	// that can distinguish their buckets is a route discriminator in the key.
	unique := map[string]int{}
	for _, k := range keys {
		unique[k]++
	}
	for k, n := range unique {
		if n > 1 {
			t.Fatalf("bucket %q was incremented %d times by one send plus one verify: "+
				"the two routes share a budget, so a mistyped digit spends the SMS allowance", k, n)
		}
	}

	// And specifically: a per-phone bucket must exist for each route.
	var phoneBuckets int
	for _, k := range keys {
		if strings.Contains(k, "_phone:") {
			phoneBuckets++
		}
	}
	if phoneBuckets != 2 {
		t.Fatalf("expected one per-phone bucket per OTP route, got %d across %q", phoneBuckets, keys)
	}
}

// TestOTP_VerifyBudgetAdmitsEveryAttemptTheServicePolicyAllows states the
// number the split is chosen against, so a later tightening has to argue with
// a test rather than with a comment.
//
// user-service issues at most otpSendLimitPerPhone codes an hour and allows
// otpVerifyMaxAttempts guesses against each one before destroying it. The
// gateway's per-phone verify budget must therefore admit their product;
// anything less makes the service's own policy unreachable from outside, which
// is what the shared bucket did.
func TestOTP_VerifyBudgetAdmitsEveryAttemptTheServicePolicyAllows(t *testing.T) {
	if got, want := otpPhoneVerifySpec.Requests, otpPhoneSendSpec.Requests*otpVerifyAttemptsPerCode; got != want {
		t.Fatalf("per-phone verify budget = %d, want %d (%d codes an hour x %d attempts each)",
			got, want, otpPhoneSendSpec.Requests, otpVerifyAttemptsPerCode)
	}
	if otpPhoneVerifySpec.Window != otpPhoneSendSpec.Window {
		t.Fatalf("send and verify windows differ (%s vs %s); the derivation above assumes one window",
			otpPhoneSendSpec.Window, otpPhoneVerifySpec.Window)
	}
}

// TestOTP_TheSendBudgetIsStillThree guards the half that must NOT move: the
// send bucket bounds real SMS spend and a real PDPA surface.
func TestOTP_TheSendBudgetIsStillThree(t *testing.T) {
	ti := newTestIdentity(t)
	upSrv, _ := newEchoUpstream(t)
	r, _, _ := buildTestRouter(t, ti, testConfig(), upSrv.URL, nil)

	phone := "+9477" + uuid.New().String()[:7]
	body := `{"phone":"` + phone + `","purpose":"login"}`

	for i := 1; i <= otpPhoneSendSpec.Requests; i++ {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/otp/send", bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("send %d: status = %d, want 200", i, rec.Code)
		}
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/otp/send", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("send %d: status = %d, want 429 -- the per-phone SMS budget must still bite",
			otpPhoneSendSpec.Requests+1, rec.Code)
	}
}
