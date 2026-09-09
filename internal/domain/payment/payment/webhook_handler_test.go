package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"telemed/internal/platform/middleware"
)

// webhookServer wires the real webhook routes over the test harness. The rate
// limiter is passed nil because it needs Redis; the limiter's own behaviour is
// the platform package's test, and what matters here is that the route is
// reachable without a JWT.
func webhookServer(t *testing.T, h *harness) http.Handler {
	t.Helper()
	return NewWebhookHandler(h.svc, zerolog.Nop()).Routes(nil)
}

func post(t *testing.T, srv http.Handler, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// TestWebhookRouteNeedsNoJWT is the design assertion: Stripe does not hold a
// Keycloak token, so these routes authenticate by signature instead. If someone
// later mounts them behind RequireAuth, every webhook starts 401ing and
// payments silently stop settling -- this test fails first.
func TestWebhookRouteNeedsNoJWT(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.pendingPayment(t, 250_000)
	h.provider.setEvent(WebhookEvent{
		EventID: "evt_http_1", Type: "payment.succeeded", Outcome: OutcomePaymentSucceeded,
		ProviderIntentID: p.ProviderIntentID, Raw: []byte(`{}`),
	})

	rec := post(t, webhookServer(t, h), "/mock", []byte(`{"id":"evt_http_1"}`))
	require.Equal(t, http.StatusOK, rec.Code, "no Authorization header was sent, and none is required")

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, true, body["received"])
	assert.Equal(t, false, body["replayed"])

	settled, err := h.store.GetPayment(t.Context(), p.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, settled.Status)
}

// TestWebhookReplayAnswers200 is the contract with the provider: a redelivery
// must be acknowledged, not 409'd, or the provider retries forever.
func TestWebhookReplayAnswers200(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.pendingPayment(t, 250_000)
	h.provider.setEvent(WebhookEvent{
		EventID: "evt_http_2", Outcome: OutcomePaymentSucceeded,
		ProviderIntentID: p.ProviderIntentID, Raw: []byte(`{}`),
	})
	srv := webhookServer(t, h)

	require.Equal(t, http.StatusOK, post(t, srv, "/mock", []byte(`{}`)).Code)

	rec := post(t, srv, "/mock", []byte(`{}`))
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, true, body["replayed"])
}

// TestWebhookBadSignatureIs401AndLeaksNothing checks the status, the code, and
// that the response repeats nothing the caller sent.
func TestWebhookBadSignatureIs401AndLeaksNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.provider.mu.Lock()
	h.provider.verifyErr = ErrSignatureInvalid
	h.provider.mu.Unlock()

	rec := post(t, webhookServer(t, h), "/mock", []byte(`{"forged":true}`))
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "SIGNATURE_INVALID", body["code"])
	// The response must not describe why verification failed; that is a hint an
	// attacker can iterate against.
	assert.Equal(t, "signature verification failed", body["message"])
	assert.NotContains(t, rec.Body.String(), "forged")
}

// TestWebhookExpiredDeliveryIs401: an old but correctly signed delivery is a
// replay attempt and is refused exactly like a forgery.
func TestWebhookExpiredDeliveryIs401(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.provider.mu.Lock()
	h.provider.verifyErr = ErrEventTooOld
	h.provider.mu.Unlock()

	rec := post(t, webhookServer(t, h), "/mock", []byte(`{}`))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestWebhookUnmatchedPaymentIs404: the provider must retry, because the usual
// cause is that the webhook overtook appointment.created.
func TestWebhookUnmatchedPaymentIs404(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.provider.setEvent(WebhookEvent{
		EventID: "evt_orphan", Outcome: OutcomePaymentSucceeded,
		ProviderIntentID: "pi_nothing_here", Raw: []byte(`{}`),
	})

	rec := post(t, webhookServer(t, h), "/mock", []byte(`{}`))
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"a 4xx the provider retries, not a 200 that would drop the event on the floor")
}

// TestWebhookBodyIsSizeCapped: the signature is computed over the whole body,
// so an unbounded body is an unbounded allocation before we can reject it.
func TestWebhookBodyIsSizeCapped(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	oversized := []byte(`{"pad":"` + strings.Repeat("A", int(MaxWebhookBody)+1024) + `"}`)

	rec := post(t, webhookServer(t, h), "/mock", oversized)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Empty(t, h.store.webhooks, "an oversized body must not reach the database")
}

// TestWebhookRoutesExistForEveryRail catches a rail being added to the registry
// but not to the router, which would leave its callbacks 404ing silently.
func TestWebhookRoutesExistForEveryRail(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	srv := webhookServer(t, h)

	for _, path := range []string{"/stripe", "/payhere", "/dialog", "/mock"} {
		rec := post(t, srv, path, []byte(`{}`))
		assert.NotEqual(t, http.StatusNotFound, rec.Code, "route %s must be mounted", path)
		assert.NotEqual(t, http.StatusMethodNotAllowed, rec.Code, "route %s must accept POST", path)
	}
}

func TestWebhookRejectsGet(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/mock", http.NoBody)
	rec := httptest.NewRecorder()
	webhookServer(t, h).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// TestWebhookAmountMismatchIs409AndLeaksNothing pins the HTTP half of F10.
//
// 409 rather than 200 because a 200 would make the disagreement invisible on
// both sides: the rail would consider the delivery successful and stop, and
// nobody would look. 409 rather than 5xx because "we broke, try again" is
// untrue and buys a retry storm. And the body must not tell a caller probing
// for a payment's price what that price is.
func TestWebhookAmountMismatchIs409AndLeaksNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.pendingPayment(t, 500_000)
	h.provider.setEvent(WebhookEvent{
		EventID: "evt_http_mismatch", Type: "payment.succeeded",
		Outcome: OutcomePaymentSucceeded, ProviderIntentID: p.ProviderIntentID,
		AmountCents: 1, Currency: CurrencyLKR, Raw: []byte(`{}`),
	})

	srv := webhookServer(t, h)
	rec := post(t, srv, "/mock", []byte(`{"id":"evt_http_mismatch"}`))
	require.Equal(t, http.StatusConflict, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "WEBHOOK_AMOUNT_MISMATCH", body["code"])
	assert.NotContains(t, rec.Body.String(), "500000",
		"the expected amount must not be echoed to a caller probing for it")
	assert.NotContains(t, rec.Body.String(), p.ID.String())

	settled, err := h.store.GetPayment(t.Context(), p.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusRequiresAction, settled.Status, "nothing settled")

	// The rail's retry deduplicates rather than re-alarming.
	again := post(t, srv, "/mock", []byte(`{"id":"evt_http_mismatch"}`))
	assert.Equal(t, http.StatusOK, again.Code)
}

// --- F9: the Dialog IP allowlist -------------------------------------------

// behindRealIP wraps the webhook routes the way server.New does in
// production, so the allowlist sees the address the platform's trusted-proxy
// walk resolved rather than the raw peer. Testing the gate without RealIP in
// front of it would test a different program.
func behindRealIP(h http.Handler) http.Handler {
	return middleware.RealIP(middleware.DefaultTrustedProxies())(h)
}

// dialogRequest builds a callback arriving through the gateway, i.e. with the
// client address in X-Forwarded-For and a trusted peer, which is how every
// request actually reaches this service.
func dialogRequest(clientIP string) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/dialog",
		strings.NewReader(`{"statusCode":"S1000","internalTrxId":"TRX-1"}`))
	req.RemoteAddr = "10.0.0.7:41234" // the gateway pod, inside the default trusted ranges
	req.Header.Set("X-Forwarded-For", clientIP)
	return req
}

// TestDialogWebhookIsIPAllowlisted is the control the design documents promised
// and the code did not have.
//
// Ideamart signs nothing natively. The provider comment and the boot log both
// asserted an IP-allowlist fallback; middleware.IPAllowlist appeared nowhere in
// this service, there was no DIALOG_WEBHOOK_CIDRS, and /webhooks/dialog was
// mounted with a rate limiter and nothing else.
func TestDialogWebhookIsIPAllowlisted(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	srv := behindRealIP(NewWebhookHandler(h.svc, zerolog.Nop()).
		WithDialogAllowlist([]string{"203.94.64.0/18", "2402:4000::/32"}).
		Routes(nil))

	t.Run("an address outside the carrier's ranges is refused", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, dialogRequest("198.51.100.9"))
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("an address inside them reaches the rail", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, dialogRequest("203.94.90.4"))
		// The mock rail is what is registered in the harness, so Dialog is not
		// configured and the request fails inside the service -- which is the
		// point: it got past the gate.
		assert.NotEqual(t, http.StatusForbidden, rec.Code)
	})

	t.Run("IPv6 inside the range reaches the rail", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, dialogRequest("2402:4000:1000::5"))
		assert.NotEqual(t, http.StatusForbidden, rec.Code)
	})

	t.Run("the other rails are not gated, because they sign", func(t *testing.T) {
		p := h.pendingPayment(t, 250_000)
		h.provider.setEvent(WebhookEvent{
			EventID: "evt_not_gated", Type: "payment.succeeded",
			Outcome: OutcomePaymentSucceeded, ProviderIntentID: p.ProviderIntentID,
			Raw: []byte(`{}`),
		})
		rec := post(t, srv, "/mock", []byte(`{"id":"evt_not_gated"}`))
		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

// TestDialogWebhookFailsClosedWithNoAllowlist is the difference between this
// gate and middleware.IPAllowlist.
//
// The shared middleware treats an empty list as "not configured" and calls
// next, which is the right default for the admin surface -- locking staff out
// of a running platform over a missing variable is worse than the exposure --
// and exactly the wrong one for an unauthenticated endpoint that marks
// consultations paid. Settings.Validate refuses to boot the rail without a
// list; this is what happens if it is ever mounted anyway.
func TestDialogWebhookFailsClosedWithNoAllowlist(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	srv := behindRealIP(NewWebhookHandler(h.svc, zerolog.Nop()).Routes(nil)) // no allowlist at all

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, dialogRequest("203.94.90.4"))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"an unconfigured allowlist on an unauthenticated money endpoint must deny, not permit")
}

// TestDialogWebhookIgnoresAnUntrustedForwardedFor: the allowlist is only worth
// anything if the address it matches cannot be chosen by the caller. The
// platform's RealIP walks X-Forwarded-For from the right and stops at the first
// untrusted hop, so a client-supplied header is not believed.
func TestDialogWebhookIgnoresAnUntrustedForwardedFor(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	srv := behindRealIP(NewWebhookHandler(h.svc, zerolog.Nop()).
		WithDialogAllowlist([]string{"203.94.64.0/18"}).
		Routes(nil))

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/dialog",
		strings.NewReader(`{"statusCode":"S1000","internalTrxId":"TRX-1"}`))
	// The connection itself comes from the public internet, and it claims to
	// be Dialog. It is not a trusted proxy, so the claim is discarded.
	req.RemoteAddr = "198.51.100.9:52000"
	req.Header.Set("X-Forwarded-For", "203.94.90.4")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"a caller must not be able to name its own source address into the allowlist")
}
