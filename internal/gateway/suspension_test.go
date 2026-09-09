package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/platform/httpx"
	platmw "telemed/internal/platform/middleware"
)

// suspend puts a user on the denylist exactly the way user-service does --
// through the shared helper, not a hand-written key -- so this test would fail
// if the two ever drifted on the string.
func suspend(t *testing.T, c *fakeCache, userID uuid.UUID) {
	t.Helper()
	if err := platmw.MarkSuspended(context.Background(), c, userID); err != nil {
		t.Fatalf("MarkSuspended: %v", err)
	}
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return e.Code
}

// TestRouter_SuspendedUserIsRefusedImmediately is the gateway half of Gap 4.
//
// Access tokens are RS256 JWTs verified offline, so an issued token cannot be
// un-issued: refusing refresh (user-service) stops the NEXT token, not the one
// the client is already holding. Without this check, an administrator's
// suspension would take up to the access-token TTL -- fifteen minutes -- to
// have any effect, which is not what "suspend this account" means when the
// reason is a doctor behaving dangerously on a live call.
func TestRouter_SuspendedUserIsRefusedImmediately(t *testing.T) {
	ti := newTestIdentity(t)
	upSrv, echo := newEchoUpstream(t)
	r, _, c := buildTestRouter(t, ti, testConfig(), upSrv.URL, nil)

	userID := uuid.New()
	// The token was minted BEFORE the suspension and is still perfectly valid:
	// correct signature, unexpired, right audience and issuer.
	token := ti.sign(t, userID, []platmw.Role{platmw.RolePatient})
	suspend(t, c, userID)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/users/me", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec.Body.Bytes()); got != string(httpx.CodeAccountSuspended) {
		t.Errorf("error code = %q, want %q -- a client must be able to tell "+
			"'your account is stopped' from 'not for you', or it retries forever",
			got, httpx.CodeAccountSuspended)
	}
	if echo.headers() != nil {
		t.Error("the request reached the upstream: the suspension did nothing")
	}
}

// TestRouter_SuspensionAppliesToTheAdminSurfaceToo. A suspended ops account
// with an admin role and an allowlisted IP is still a suspended account.
func TestRouter_SuspensionAppliesToTheAdminSurface(t *testing.T) {
	ti := newTestIdentity(t)
	upSrv, echo := newEchoUpstream(t)
	cfg := testConfig()
	cfg.AdminIPAllowlist = []string{"192.0.2.0/24"}
	r, _, c := buildTestRouter(t, ti, cfg, upSrv.URL, nil)

	userID := uuid.New()
	token := ti.sign(t, userID, []platmw.Role{platmw.RoleAdmin})
	suspend(t, c, userID)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/admin/doctors/pending", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)
	req.RemoteAddr = "192.0.2.10:5555"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec.Body.Bytes()); got != string(httpx.CodeAccountSuspended) {
		t.Errorf("error code = %q, want %q", got, httpx.CodeAccountSuspended)
	}
	if echo.headers() != nil {
		t.Error("a suspended admin reached the admin surface")
	}
}

// TestRouter_ReinstatedUserWorksAgain: the denylist is a cache of a decision,
// not a second source of truth. Clearing it must actually let the user back
// in, or reinstatement is a button that does nothing.
func TestRouter_ReinstatedUserWorksAgain(t *testing.T) {
	ti := newTestIdentity(t)
	upSrv, echo := newEchoUpstream(t)
	r, _, c := buildTestRouter(t, ti, testConfig(), upSrv.URL, nil)

	userID := uuid.New()
	token := ti.sign(t, userID, []platmw.Role{platmw.RolePatient})
	suspend(t, c, userID)
	if err := platmw.ClearSuspended(context.Background(), c, userID); err != nil {
		t.Fatalf("ClearSuspended: %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/users/me", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if echo.headers() == nil {
		t.Fatal("upstream was never called")
	}
}

// TestRouter_SuspensionOfOneUserDoesNotAffectAnother guards the obvious
// catastrophe: a key namespace bug that denies everyone.
func TestRouter_SuspensionOfOneUserDoesNotAffectAnother(t *testing.T) {
	ti := newTestIdentity(t)
	upSrv, echo := newEchoUpstream(t)
	r, _, c := buildTestRouter(t, ti, testConfig(), upSrv.URL, nil)

	suspend(t, c, uuid.New())

	other := uuid.New()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/users/me", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+ti.sign(t, other, []platmw.Role{platmw.RolePatient}))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if echo.headers() == nil {
		t.Fatal("upstream was never called")
	}
}

// TestRouter_PublicRoutesAreNotSuspensionChecked. OTP login is how a
// reinstated user gets back in, and a public route has no principal to check
// in any case. Applying the denylist there would be checking a key derived
// from nothing.
func TestRouter_PublicRoutesAreNotSuspensionChecked(t *testing.T) {
	ti := newTestIdentity(t)
	upSrv, echo := newEchoUpstream(t)
	r, _, c := buildTestRouter(t, ti, testConfig(), upSrv.URL, nil)

	userID := uuid.New()
	suspend(t, c, userID)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/otp/send", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+ti.sign(t, userID, []platmw.Role{platmw.RolePatient}))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if echo.headers() == nil {
		t.Fatal("upstream was never called")
	}
}

// TestRejectSuspended_FailsOpenWhenRedisIsDown is the deliberate trade,
// asserted so nobody "fixes" it into a fail-closed check by accident.
//
// A fail-closed denylist turns a Redis blip into a total platform outage:
// every patient locked out of every consultation to enforce a state a handful
// of accounts are in. Failing open degrades suspension to "takes effect within
// the access-token TTL" -- exactly where the platform stood before the
// denylist existed -- while refresh refusal and session revocation, which live
// in Postgres, are untouched.
func TestRejectSuspended_FailsOpenWhenRedisIsDown(t *testing.T) {
	c := newFakeCache()
	userID := uuid.New()
	suspend(t, c, userID)
	c.failGet = true // Exists goes through the same simulated outage

	reached := false
	h := platmw.RejectSuspended(c, zerolog.Nop())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))

	req := httptest.NewRequestWithContext(
		platmw.WithPrincipal(t.Context(), platmw.Principal{UserID: userID}),
		http.MethodGet, "/api/v1/users/me", http.NoBody)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !reached {
		t.Error("an unreachable denylist must not lock the platform out; the durable " +
			"mechanisms in user-service still hold")
	}
}

// TestRejectSuspended_NoPrincipalPassesThrough: the middleware reads a
// verified principal and has nothing to say without one.
func TestRejectSuspended_NoPrincipalPassesThrough(t *testing.T) {
	reached := false
	h := platmw.RejectSuspended(newFakeCache(), zerolog.Nop())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", http.NoBody))
	if !reached {
		t.Error("an unauthenticated request must fall through to whatever rejects it next")
	}
}

// TestRejectSuspended_NilCacheIsANoOp: a deployment without Redis still
// suspends accounts, just not instantly.
func TestRejectSuspended_NilCacheIsANoOp(t *testing.T) {
	reached := false
	h := platmw.RejectSuspended(nil, zerolog.Nop())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))
	req := httptest.NewRequestWithContext(
		platmw.WithPrincipal(t.Context(), platmw.Principal{UserID: uuid.New()}),
		http.MethodGet, "/x", http.NoBody)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !reached {
		t.Error("a nil cache must disable the check, not deny every request")
	}
}
