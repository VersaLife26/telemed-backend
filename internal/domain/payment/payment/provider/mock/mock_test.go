package mockprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"telemed/internal/domain/payment/payment"
)

const secret = "whsec_mock_fixed_for_tests"

func newProvider(t *testing.T) *Provider {
	t.Helper()
	return New(Config{WebhookSecret: secret, Tolerance: 5 * time.Minute})
}

func signedBody(t *testing.T, ev MockEvent, at time.Time, withSecret string) (http.Header, []byte) {
	t.Helper()
	body, err := json.Marshal(ev)
	require.NoError(t, err)
	h := http.Header{}
	h.Set(SignatureHeader, Sign(withSecret, at, body))
	return h, body
}

func TestMockWebhookValidSignature(t *testing.T) {
	t.Parallel()

	p := newProvider(t)
	headers, body := signedBody(t, MockEvent{
		ID: "evt_1", Type: "payment.succeeded", ProviderIntentID: "pi_1",
		AmountCents: 150000, Currency: "LKR",
	}, time.Now(), secret)

	evt, err := p.VerifyWebhook(context.Background(), headers, body)
	require.NoError(t, err)
	assert.Equal(t, "evt_1", evt.EventID)
	assert.Equal(t, payment.OutcomePaymentSucceeded, evt.Outcome)
	assert.Equal(t, int64(150000), evt.AmountCents)
}

func TestMockWebhookTamperedBody(t *testing.T) {
	t.Parallel()

	p := newProvider(t)
	headers, body := signedBody(t, MockEvent{
		ID: "evt_2", Type: "payment.succeeded", AmountCents: 100,
	}, time.Now(), secret)

	tampered := []byte(strings.Replace(string(body), `"amount_cents":100`, `"amount_cents":999`, 1))
	require.NotEqual(t, string(body), string(tampered))

	_, err := p.VerifyWebhook(context.Background(), headers, tampered)
	assert.ErrorIs(t, err, payment.ErrSignatureInvalid)
}

func TestMockWebhookWrongSecret(t *testing.T) {
	t.Parallel()

	p := newProvider(t)
	headers, body := signedBody(t, MockEvent{ID: "evt_3", Type: "payment.succeeded"},
		time.Now(), "a-different-secret")

	_, err := p.VerifyWebhook(context.Background(), headers, body)
	assert.ErrorIs(t, err, payment.ErrSignatureInvalid)
}

func TestMockWebhookExpiredTimestamp(t *testing.T) {
	t.Parallel()

	p := newProvider(t)
	headers, body := signedBody(t, MockEvent{ID: "evt_4", Type: "payment.succeeded"},
		time.Now().Add(-time.Hour), secret)

	_, err := p.VerifyWebhook(context.Background(), headers, body)
	assert.ErrorIs(t, err, payment.ErrEventTooOld)
}

func TestMockWebhookMissingOrMalformedHeader(t *testing.T) {
	t.Parallel()

	p := newProvider(t)
	body := []byte(`{"id":"evt_5","type":"payment.succeeded"}`)

	_, err := p.VerifyWebhook(context.Background(), http.Header{}, body)
	assert.ErrorIs(t, err, payment.ErrSignatureInvalid)

	for _, raw := range []string{"garbage", "t=123", "v1=abc", "t=abc,v1=def"} {
		h := http.Header{}
		h.Set(SignatureHeader, raw)
		_, err := p.VerifyWebhook(context.Background(), h, body)
		assert.Error(t, err, "header %q", raw)
	}
}

func TestMockWebhookRequiresAnEventID(t *testing.T) {
	t.Parallel()

	p := newProvider(t)
	headers, body := signedBody(t, MockEvent{Type: "payment.succeeded"}, time.Now(), secret)

	_, err := p.VerifyWebhook(context.Background(), headers, body)
	// Still rejected -- an event with no id cannot be deduplicated, so it must
	// not be accepted. But it is a MALFORMED BODY, not a signature failure:
	// the signature verified fine. Reporting it as ErrSignatureInvalid sent a
	// debugging session after the crypto for an hour when the fault was a
	// field name.
	assert.ErrorIs(t, err, payment.ErrWebhookMalformed,
		"an event with no id cannot be deduplicated, so it must not be accepted")
	assert.NotErrorIs(t, err, payment.ErrSignatureInvalid,
		"a correctly signed body must never be reported as a signature failure")
}

// TestMockIntentIdempotency: the same key returns the same intent, exactly as a
// real rail does. Without this the local dev flow would not exercise the retry
// path at all.
func TestMockIntentIdempotency(t *testing.T) {
	t.Parallel()

	p := newProvider(t)
	req := payment.IntentRequest{
		PaymentID: uuid.New(), AmountCents: 100000, IdempotencyKey: "stable-key",
	}

	first, err := p.CreateIntent(context.Background(), req)
	require.NoError(t, err)
	second, err := p.CreateIntent(context.Background(), req)
	require.NoError(t, err)

	assert.Equal(t, first.ProviderIntentID, second.ProviderIntentID)
	assert.Equal(t, first.ClientSecret, second.ClientSecret)
	assert.Equal(t, 1, p.storedIntents(),
		"a second call with the same key must reuse the stored intent, not create another")
}

func (p *Provider) storedIntents() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.intents)
}

func TestMockPayoutIdempotency(t *testing.T) {
	t.Parallel()

	p := newProvider(t)
	req := payment.PayoutRequest{
		PayoutID: uuid.New(), AmountCents: 500000, IdempotencyKey: "payout-key",
	}

	a, err := p.Payout(context.Background(), req)
	require.NoError(t, err)
	b, err := p.Payout(context.Background(), req)
	require.NoError(t, err)

	assert.Equal(t, a.TransferID, b.TransferID, "the same key must not move money twice")
	assert.Equal(t, 1, p.TransferCount())
}

func TestMockRefundIdempotency(t *testing.T) {
	t.Parallel()

	p := newProvider(t)
	req := payment.RefundRequest{
		PaymentID: uuid.New(), RefundID: uuid.New(), AmountCents: 1000, IdempotencyKey: "refund-key",
	}

	a, err := p.Refund(context.Background(), req)
	require.NoError(t, err)
	b, err := p.Refund(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, a.ProviderRefundID, b.ProviderRefundID)
}

func TestMockPINFlow(t *testing.T) {
	t.Parallel()

	p := newProvider(t)
	challenge, err := p.SendPIN(context.Background(), payment.IntentRequest{PatientPhone: "+94771234567"})
	require.NoError(t, err)
	assert.NotEmpty(t, challenge.Reference)
	assert.NotContains(t, challenge.MaskedMSISDN, "9477123",
		"the masked number must not reveal the subscriber")

	_, err = p.ConfirmPIN(context.Background(), payment.PINConfirmation{
		Reference: challenge.Reference, PIN: "000000", AmountCents: 1000,
	})
	assert.ErrorIs(t, err, payment.ErrProviderRejected)

	res, err := p.ConfirmPIN(context.Background(), payment.PINConfirmation{
		Reference: challenge.Reference, PIN: "123456", AmountCents: 1000,
	})
	require.NoError(t, err)
	assert.Equal(t, payment.IntentSucceeded, res.Status)
}

func TestMockAutoSucceedAppliesTheFee(t *testing.T) {
	t.Parallel()

	p := New(Config{WebhookSecret: secret, AutoSucceed: true, FeeBps: 300})
	res, err := p.CreateIntent(context.Background(), payment.IntentRequest{
		PaymentID: uuid.New(), AmountCents: 500000, IdempotencyKey: "k",
	})
	require.NoError(t, err)
	assert.Equal(t, payment.IntentSucceeded, res.Status)
	assert.Equal(t, int64(15000), res.ProviderFeeCents, "3% of LKR 5,000.00")
}

// TestMockGuardRefusesProduction is the safety catch. A mock rail in production
// would mark consultations paid that nobody paid for.
func TestMockGuardRefusesProduction(t *testing.T) {
	t.Parallel()

	assert.ErrorIs(t, Guard("prod"), ErrProductionUse)
	assert.ErrorIs(t, Guard("production"), ErrProductionUse)
	assert.NoError(t, Guard("dev"))
	assert.NoError(t, Guard("staging"))
}

func TestMockOutcomeMapping(t *testing.T) {
	t.Parallel()

	p := newProvider(t)
	cases := map[string]payment.WebhookOutcome{
		"payment.succeeded": payment.OutcomePaymentSucceeded,
		"payment.failed":    payment.OutcomePaymentFailed,
		"payment.pending":   payment.OutcomePaymentPending,
		"refund.succeeded":  payment.OutcomeRefundSucceeded,
		"refund.failed":     payment.OutcomeRefundFailed,
		"something.else":    payment.OutcomeIgnored,
	}
	for evType, want := range cases {
		headers, body := signedBody(t, MockEvent{ID: "evt_" + evType, Type: evType}, time.Now(), secret)
		evt, err := p.VerifyWebhook(context.Background(), headers, body)
		require.NoError(t, err, evType)
		assert.Equal(t, want, evt.Outcome, evType)
	}
}
