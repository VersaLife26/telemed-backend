package stripeprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	stripe "github.com/stripe/stripe-go/v86"

	"telemed/internal/domain/payment/payment"
)

const testSecret = "whsec_test_D6r7GAVDLzGdKmDbBmvpqCRLLDdSAOh1"

// signedEvent builds a webhook body and the Stripe-Signature header for it,
// using stripe-go's own signer so the test exercises the real verification path
// rather than a reimplementation of it.
func signedEvent(t *testing.T, body []byte, at time.Time, secret string) (string, []byte) {
	t.Helper()
	sp := stripe.GenerateTestSignedPayload(&stripe.UnsignedPayload{
		Payload:   body,
		Secret:    secret,
		Timestamp: at,
	})
	return sp.Header, body
}

func intentEventBody(t *testing.T, eventID, eventType, intentID string, amount int64) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"id":          eventID,
		"object":      "event",
		"api_version": stripe.APIVersion,
		"created":     time.Now().Unix(),
		"type":        eventType,
		"data": map[string]any{
			"object": map[string]any{
				"id":       intentID,
				"object":   "payment_intent",
				"amount":   amount,
				"currency": "lkr",
				"status":   "succeeded",
				"metadata": map[string]string{
					"appointment_id": "3f1a6b7c-0000-4000-8000-000000000001",
					"payment_id":     "3f1a6b7c-0000-4000-8000-000000000002",
				},
			},
		},
	})
	require.NoError(t, err)
	return body
}

func newTestProvider(t *testing.T) *Provider {
	t.Helper()
	p, err := New(Config{
		SecretKey:                "sk_test_not_a_real_key",
		WebhookSecret:            testSecret,
		Tolerance:                5 * time.Minute,
		IgnoreAPIVersionMismatch: true,
	})
	require.NoError(t, err)
	return p
}

// TestStripeWebhookValidSignature is the happy path: a correctly signed,
// recent event verifies and normalises.
func TestStripeWebhookValidSignature(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	body := intentEventBody(t, "evt_valid", "payment_intent.succeeded", "pi_abc123", 500000)
	header, body := signedEvent(t, body, time.Now(), testSecret)

	evt, err := p.VerifyWebhook(context.Background(), header2(header), body)
	require.NoError(t, err)

	assert.Equal(t, "evt_valid", evt.EventID)
	assert.Equal(t, "payment_intent.succeeded", evt.Type)
	assert.Equal(t, payment.OutcomePaymentSucceeded, evt.Outcome)
	assert.Equal(t, "pi_abc123", evt.ProviderIntentID)
	assert.Equal(t, int64(500000), evt.AmountCents)
	assert.Equal(t, "LKR", evt.Currency)
	assert.Equal(t, "3f1a6b7c-0000-4000-8000-000000000001", evt.AppointmentID)
	assert.Equal(t, body, evt.Raw, "the raw body must be preserved byte for byte for dispute evidence")
}

// TestStripeWebhookTamperedBody: the signature was valid for the original
// bytes, so changing a single character must fail verification. This is the
// attack that turns a LKR 100 consultation into a LKR 100,000 one.
func TestStripeWebhookTamperedBody(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	original := intentEventBody(t, "evt_tamper", "payment_intent.succeeded", "pi_abc123", 10000)
	header, _ := signedEvent(t, original, time.Now(), testSecret)

	tampered := []byte(strings.Replace(string(original), `"amount":10000`, `"amount":9999`, 1))
	require.NotEqual(t, string(original), string(tampered), "the fixture must actually have changed")

	_, err := p.VerifyWebhook(context.Background(), header2(header), tampered)
	require.Error(t, err)
	assert.ErrorIs(t, err, payment.ErrSignatureInvalid)
}

// TestStripeWebhookWrongSecret: a body signed with someone else's secret is
// rejected. This is what stops an attacker who has seen a real webhook from
// replaying its shape with their own signature.
func TestStripeWebhookWrongSecret(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	body := intentEventBody(t, "evt_wrong_secret", "payment_intent.succeeded", "pi_abc123", 10000)
	header, body := signedEvent(t, body, time.Now(), "whsec_a_completely_different_secret")

	_, err := p.VerifyWebhook(context.Background(), header2(header), body)
	require.Error(t, err)
	assert.ErrorIs(t, err, payment.ErrSignatureInvalid)
}

// TestStripeWebhookExpiredTimestamp: a signature stays cryptographically valid
// forever, so age is the only thing that stops a captured delivery being
// replayed. Rejecting on age is the replay defence.
func TestStripeWebhookExpiredTimestamp(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	body := intentEventBody(t, "evt_old", "payment_intent.succeeded", "pi_abc123", 10000)
	// Correctly signed -- just an hour old, against a five minute tolerance.
	header, body := signedEvent(t, body, time.Now().Add(-time.Hour), testSecret)

	_, err := p.VerifyWebhook(context.Background(), header2(header), body)
	require.Error(t, err)
	assert.ErrorIs(t, err, payment.ErrEventTooOld,
		"an old but correctly signed delivery must be reported as stale, not as forged")
}

func TestStripeWebhookMissingHeader(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	body := intentEventBody(t, "evt_nosig", "payment_intent.succeeded", "pi_abc", 100)
	_, err := p.VerifyWebhook(context.Background(), http.Header{}, body)
	assert.ErrorIs(t, err, payment.ErrSignatureInvalid)
}

func TestStripeWebhookMalformedHeader(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	body := intentEventBody(t, "evt_bad", "payment_intent.succeeded", "pi_abc", 100)
	for _, h := range []string{"garbage", "t=,v1=", "v1=deadbeef", "t=notanumber,v1=deadbeef"} {
		_, err := p.VerifyWebhook(context.Background(), header2(h), body)
		require.Error(t, err, "header %q must be rejected", h)
		// A header with no t= parses as a zero timestamp, which stripe-go
		// reports as "too old" rather than "unsigned". Both are refusals; what
		// matters is that neither ever returns a usable event.
		rejected := errors.Is(err, payment.ErrSignatureInvalid) || errors.Is(err, payment.ErrEventTooOld)
		assert.True(t, rejected, "header %q produced an unexpected error: %v", h, err)
	}
}

// TestStripeWebhookOutcomeMapping covers the event types the service acts on.
func TestStripeWebhookOutcomeMapping(t *testing.T) {
	t.Parallel()

	cases := map[string]payment.WebhookOutcome{
		"payment_intent.succeeded":       payment.OutcomePaymentSucceeded,
		"payment_intent.payment_failed":  payment.OutcomePaymentFailed,
		"payment_intent.processing":      payment.OutcomePaymentPending,
		"payment_intent.requires_action": payment.OutcomePaymentPending,
		"customer.created":               payment.OutcomeIgnored,
	}

	p := newTestProvider(t)
	for eventType, want := range cases {
		body := intentEventBody(t, "evt_"+eventType, eventType, "pi_x", 1000)
		header, body := signedEvent(t, body, time.Now(), testSecret)
		evt, err := p.VerifyWebhook(context.Background(), header2(header), body)
		require.NoError(t, err, eventType)
		assert.Equal(t, want, evt.Outcome, "event type %s", eventType)
	}
}

// TestStripeWebhookAPIVersionMismatch documents the v86 behaviour the v76-era
// documentation does not mention: ConstructEvent rejects an event from a
// different API release train unless told not to.
func TestStripeWebhookAPIVersionMismatch(t *testing.T) {
	t.Parallel()

	body, err := json.Marshal(map[string]any{
		"id":          "evt_old_version",
		"object":      "event",
		"api_version": "2020-08-27", // no release train suffix at all
		"created":     time.Now().Unix(),
		"type":        "payment_intent.succeeded",
		"data":        map[string]any{"object": map[string]any{"id": "pi_x", "object": "payment_intent"}},
	})
	require.NoError(t, err)
	header, body := signedEvent(t, body, time.Now(), testSecret)

	strict, err := New(Config{
		SecretKey: "sk_test_x", WebhookSecret: testSecret,
		IgnoreAPIVersionMismatch: false,
	})
	require.NoError(t, err)
	_, err = strict.VerifyWebhook(context.Background(), header2(header), body)
	assert.ErrorIs(t, err, payment.ErrSignatureInvalid,
		"strict mode refuses an event from a foreign API release train")

	relaxed := newTestProvider(t)
	evt, err := relaxed.VerifyWebhook(context.Background(), header2(header), body)
	require.NoError(t, err, "relaxed mode accepts it, which is the default and why the default exists")
	assert.Equal(t, "evt_old_version", evt.EventID)
}

func TestStripeRequiresCredentials(t *testing.T) {
	t.Parallel()

	_, err := New(Config{WebhookSecret: testSecret})
	assert.Error(t, err, "a missing API key must fail at boot, not at the till")

	_, err = New(Config{SecretKey: "sk_test_x"})
	assert.Error(t, err, "a missing webhook secret must fail at boot; unverified webhooks are not acceptable")
}

func TestStripeRefundRequiresAnIntent(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	_, err := p.Refund(context.Background(), payment.RefundRequest{AmountCents: 100})
	assert.ErrorIs(t, err, payment.ErrProviderRejected)
}

func TestStripePayoutRequiresADestination(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	_, err := p.Payout(context.Background(), payment.PayoutRequest{AmountCents: 100})
	assert.ErrorIs(t, err, payment.ErrProviderRejected)
}

// TestStripeErrorClassification is the difference between "your card was
// declined" and "try again in a moment". Getting it backwards either
// double-charges a patient or tells them a working card failed.
func TestStripeErrorClassification(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want error
	}{
		{"card declined", &stripe.Error{Type: stripe.ErrorTypeCard, Code: "card_declined"}, payment.ErrProviderRejected},
		{"invalid request", &stripe.Error{Type: stripe.ErrorTypeInvalidRequest}, payment.ErrProviderRejected},
		{"idempotency misuse", &stripe.Error{Type: stripe.ErrorTypeIdempotency}, payment.ErrProviderRejected},
		{"stripe api error", &stripe.Error{Type: stripe.ErrorTypeAPI}, payment.ErrProviderUnavailable},
		{"transport failure", fmt.Errorf("dial tcp: i/o timeout"), payment.ErrProviderUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.ErrorIs(t, classify(tc.err, "test"), tc.want)
		})
	}
}

func TestStripeIntentStatusMapping(t *testing.T) {
	t.Parallel()

	assert.Equal(t, payment.IntentSucceeded, mapIntentStatus(stripe.PaymentIntentStatusSucceeded))
	assert.Equal(t, payment.IntentRequiresAction, mapIntentStatus(stripe.PaymentIntentStatusRequiresAction))
	assert.Equal(t, payment.IntentRequiresAction, mapIntentStatus(stripe.PaymentIntentStatusRequiresPaymentMethod))
	assert.Equal(t, payment.IntentFailed, mapIntentStatus(stripe.PaymentIntentStatusCanceled))
	assert.Equal(t, payment.IntentPending, mapIntentStatus(stripe.PaymentIntentStatusProcessing))
}

func header2(sig string) http.Header {
	h := http.Header{}
	h.Set("Stripe-Signature", sig)
	return h
}
