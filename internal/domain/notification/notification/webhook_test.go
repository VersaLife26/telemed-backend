package notification

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// webhookRouter mounts the delivery callback exactly as cmd/server does, but
// over a nil repository.
//
// The nil repository is the assertion, not a shortcut: every case below is one
// the endpoint must refuse BEFORE it reaches the database. If any of them ever
// gets through, the test panics rather than quietly passing.
func webhookRouter(t *testing.T, secret string) http.Handler {
	t.Helper()
	h := NewHandler(nil, nil, zerolog.Nop())
	r := chi.NewRouter()
	r.Route("/webhooks", func(r chi.Router) {
		h.WebhookRoutes(r, secret, nil)
	})
	return r
}

const testWebhookSecret = "0123456789abcdef0123456789abcdef"

// POST /webhooks/delivery/{provider} was mounted bare: no signature, no
// allowlist, no rate limit, no body cap. What it reaches is
// `UPDATE notifications ... WHERE provider = $1 AND provider_message_id = $2`
// with no user scoping, and a provider_message_id is a Twilio SID -- not a
// secret, and shorter than a UUID. Anyone who learned or guessed one could
// mark another user's notification delivered, destroying the delivery
// evidence, or write chosen text into notifications.last_error, which the
// retention job never prunes.
func TestDeliveryWebhookRequiresTheSharedSecret(t *testing.T) {
	t.Parallel()

	body := `{"message_id":"SM0123456789","status":"delivered"}`

	cases := []struct {
		name    string
		mutate  func(*http.Request)
		wantErr bool
	}{
		{"no token at all", func(*http.Request) {}, true},
		{"an empty token", func(r *http.Request) { r.Header.Set(webhookTokenHeader, "") }, true},
		{"a wrong token", func(r *http.Request) { r.Header.Set(webhookTokenHeader, "not-the-secret") }, true},
		{"a token that is a prefix of the real one", func(r *http.Request) {
			r.Header.Set(webhookTokenHeader, testWebhookSecret[:8])
		}, true},
		{"the token in a query parameter, which is the documented form", func(r *http.Request) {
			q := r.URL.Query()
			q.Set("token", testWebhookSecret)
			r.URL.RawQuery = q.Encode()
		}, false},
		{"the token in the header", func(r *http.Request) {
			r.Header.Set(webhookTokenHeader, testWebhookSecret)
		}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(context.Background(),
				http.MethodPost, "/webhooks/delivery/dialog", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			tc.mutate(req)

			rec := httptest.NewRecorder()
			if !tc.wantErr {
				// A correct token must get past the gate. It then panics on
				// the nil repository, which is exactly the proof that the gate
				// was the only thing in the way.
				require.Panics(t, func() { webhookRouter(t, testWebhookSecret).ServeHTTP(rec, req) },
					"a correctly-authenticated callback did not reach the repository")
				return
			}
			webhookRouter(t, testWebhookSecret).ServeHTTP(rec, req)
			require.Equal(t, http.StatusUnauthorized, rec.Code,
				"an unauthenticated caller updated a notification row: %s", rec.Body.String())
		})
	}
}

// A build that somehow reaches the middleware with no secret must refuse
// everything rather than accept everything.
func TestDeliveryWebhookWithNoSecretConfiguredRefusesEveryone(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequestWithContext(context.Background(),
		http.MethodPost, "/webhooks/delivery/dialog", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	webhookRouter(t, "").ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
}

// notifications.last_error is written verbatim from the callback and the
// retention job never prunes it, so it outlives the message body by design.
func TestScrubContactRemovesRecipientIdentifiers(t *testing.T) {
	t.Parallel()

	cases := []struct{ name, in, mustNotContain string }{
		{"an SMTP bounce quoting the address",
			`550 5.1.1 <priyanka.fernando@example.com>: Recipient address rejected: User unknown`,
			"priyanka.fernando@example.com"},
		{"twilio error 21211",
			`The 'To' number +94771234567 is not a valid phone number`,
			"+94771234567"},
		{"a spaced phone number",
			`destination +94 77 123 4567 unreachable`,
			"77 123 4567"},
		{"a local-format number",
			`0771234567 rejected by carrier`,
			"0771234567"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ScrubContact(tc.in)
			require.NotContains(t, got, tc.mustNotContain,
				"a recipient identifier was persisted to notifications.last_error: %q", got)
		})
	}

	// It must stay useful: the diagnostic part survives.
	require.Contains(t, ScrubContact(`550 5.1.1 <x@example.com>: Recipient address rejected`), "550 5.1.1")

	// And it still caps.
	require.LessOrEqual(t, len(ScrubContact(strings.Repeat("a", 2000))), maxStoredErrLen)
}
