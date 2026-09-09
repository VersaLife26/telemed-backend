package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	platmw "telemed/internal/platform/middleware"
)

// TestRateLimit_AuthenticatedRouteIsBudgetedPerUserNotPerIP is the regression
// test for a rate limiter that was keyed on the wrong thing for every
// authenticated route on the platform.
//
// buildRateLimiters asks for platmw.ByPrincipal, which returns
// "user:<uuid>" when a verified principal is on the request context and falls
// back to "ip:<addr>" when there is not one. composeMiddleware then applied
// the limiter OUTSIDE RequireAuth -- and outer middleware runs first, so
// ByPrincipal was always evaluated before any principal existed. Every route
// silently degraded to the IP fallback.
//
// That is a bypass, not a nit. The per-user budget is what stops one
// authenticated account from hammering an endpoint; keyed on source address
// instead, the same account gets a fresh budget from every address it can
// reach the gateway from -- and an IPv6 /64 is 2^64 of them. It is also an
// availability problem in the other direction: on carrier-grade NAT, which is
// how most of this platform's users reach the internet, thousands of
// subscribers share one bucket.
//
// The assertion is on the Redis key, not on the status code, because a status
// code cannot distinguish the two designs -- both answer 200 until some bucket
// fills. The key IS the control.
func TestRateLimit_AuthenticatedRouteIsBudgetedPerUserNotPerIP(t *testing.T) {
	ti := newTestIdentity(t)
	upSrv, _ := newEchoUpstream(t)
	r, _, c := buildTestRouter(t, ti, testConfig(), upSrv.URL, nil)

	// Two different patients, deliberately arriving from the SAME address --
	// which is what httptest.NewRequest always produces (192.0.2.1:1234).
	alice := uuid.New()
	bob := uuid.New()

	for _, id := range []uuid.UUID{alice, bob} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/users/me", http.NoBody)
		req.Header.Set("Authorization", "Bearer "+ti.sign(t, id, []platmw.Role{platmw.RolePatient}))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("authenticated request for %s: status = %d, body = %s", id, rec.Code, rec.Body.String())
		}
	}

	keys := c.rateLimitKeys()
	if len(keys) == 0 {
		t.Fatal("no rate-limit bucket was touched at all: the limiter is not composed on this route")
	}

	var userKeyed, ipKeyed int
	for _, k := range keys {
		switch {
		case strings.Contains(k, ":user:"):
			userKeyed++
		case strings.Contains(k, ":ip:"):
			ipKeyed++
		}
	}
	if userKeyed == 0 {
		t.Fatalf("every bucket was keyed on the source address, none on the verified user: %q\n"+
			"ByPrincipal only sees a principal if the limiter runs AFTER RequireAuth", keys)
	}

	// And the two users must not share a bucket, which is the property the
	// whole control exists for.
	aliceBucket, bobBucket := "user:"+alice.String(), "user:"+bob.String()
	var sawAlice, sawBob bool
	for _, k := range keys {
		sawAlice = sawAlice || strings.Contains(k, aliceBucket)
		sawBob = sawBob || strings.Contains(k, bobBucket)
	}
	if !sawAlice || !sawBob {
		t.Fatalf("two distinct users from one address must occupy two distinct buckets; got %q", keys)
	}
	_ = ipKeyed
}

// TestRateLimit_AnonymousCallerOnAPublicRouteStillFallsBackToIP pins the other
// half: moving the limiter behind authentication must not leave anonymous
// traffic unbudgeted. A public route has no principal to key on, so the
// documented IP fallback is the correct and only answer there.
func TestRateLimit_AnonymousCallerOnAPublicRouteStillFallsBackToIP(t *testing.T) {
	ti := newTestIdentity(t)
	upSrv, _ := newEchoUpstream(t)
	r, _, c := buildTestRouter(t, ti, testConfig(), upSrv.URL, nil)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/doctors", http.NoBody)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("public route: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	keys := c.rateLimitKeys()
	if len(keys) == 0 {
		t.Fatal("an anonymous request on a public route spent no rate-limit budget at all")
	}
	for _, k := range keys {
		if !strings.Contains(k, ":ip:") {
			t.Fatalf("anonymous bucket %q is not keyed on the source address", k)
		}
	}
}

// TestRateLimit_AnAuthenticatedRouteStillBudgetsAnAnonymousCaller guards the
// specific regression the fix could introduce: if the limiter sits behind
// RequireAuth, a request with no token is refused 401 before it is counted.
// That is acceptable only because it reaches no upstream and touches no
// database -- but the route must still refuse it, and it must not become a
// free channel to the backend.
func TestRateLimit_AnAuthenticatedRouteStillRefusesAnAnonymousCaller(t *testing.T) {
	ti := newTestIdentity(t)
	upSrv, echo := newEchoUpstream(t)
	r, _, _ := buildTestRouter(t, ti, testConfig(), upSrv.URL, nil)
	echo.reset()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/users/me", http.NoBody)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if echo.headers() != nil {
		t.Fatal("an unauthenticated request reached the upstream")
	}
}
