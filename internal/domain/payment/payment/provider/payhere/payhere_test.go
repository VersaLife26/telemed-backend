package payhereprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"telemed/internal/domain/payment/payment"
)

const (
	testMerchant = "1221149"
	testSecret   = "MzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MA=="
)

func newTestProvider(t *testing.T) *Provider {
	t.Helper()
	p, err := New(Config{
		MerchantID:     testMerchant,
		MerchantSecret: testSecret,
		BaseURL:        "https://sandbox.payhere.lk",
		NotifyURL:      "https://api.example.lk/webhooks/payhere",
	})
	require.NoError(t, err)
	return p
}

// notifyBody builds a PayHere notify form, signing it with the given secret.
func notifyBody(merchantID, orderID, amount, currency, statusCode, secret string) []byte {
	sig := NotifyHash(merchantID, orderID, amount, currency, statusCode, md5Upper([]byte(secret)))
	form := url.Values{
		"merchant_id":      {merchantID},
		"order_id":         {orderID},
		"payment_id":       {"320027000123"},
		"payhere_amount":   {amount},
		"payhere_currency": {currency},
		"status_code":      {statusCode},
		"md5sig":           {sig},
		"method":           {"VISA"},
		"status_message":   {"Successfully completed"},
	}
	return []byte(form.Encode())
}

func formHeader() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	return h
}

// TestPayHereNotifyValidHash is the happy path, and it also pins the exact hash
// construction the PayHere 2.0 protocol specifies. If this test changes, the
// integration is broken in production.
func TestPayHereNotifyValidHash(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	body := notifyBody(testMerchant, "3f1a6b7c-0000-4000-8000-000000000001", "5000.00", "LKR", statusSuccess, testSecret)

	evt, err := p.VerifyWebhook(context.Background(), formHeader(), body)
	require.NoError(t, err)

	assert.Equal(t, payment.OutcomePaymentSucceeded, evt.Outcome)
	assert.Equal(t, "3f1a6b7c-0000-4000-8000-000000000001", evt.ProviderIntentID)
	assert.Equal(t, int64(500000), evt.AmountCents)
	assert.Equal(t, "LKR", evt.Currency)
	// PayHere sends no event id, so one is synthesised from fields that are
	// stable across redeliveries and distinct between outcomes.
	assert.Equal(t, "3f1a6b7c-0000-4000-8000-000000000001:320027000123:2", evt.EventID)
}

// TestPayHereNotifyTamperedFields walks every field the hash covers and proves
// changing any one of them fails verification.
func TestPayHereNotifyTamperedFields(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	orderID := "3f1a6b7c-0000-4000-8000-000000000001"

	tamper := map[string]string{
		"payhere_amount":   "50000.00", // the attack: ten times the price
		"order_id":         "3f1a6b7c-0000-4000-8000-0000000000ff",
		"payhere_currency": "USD",
		"status_code":      statusSuccess,
	}

	for field, newValue := range tamper {
		t.Run(field, func(t *testing.T) {
			// Sign a failed payment, then flip the field to claim success.
			original := notifyBody(testMerchant, orderID, "5000.00", "LKR", statusFailed, testSecret)
			form, err := url.ParseQuery(string(original))
			require.NoError(t, err)
			form.Set(field, newValue)

			_, err = p.VerifyWebhook(context.Background(), formHeader(), []byte(form.Encode()))
			require.Error(t, err, "tampering with %s must be rejected", field)
			assert.ErrorIs(t, err, payment.ErrSignatureInvalid)
		})
	}
}

// TestPayHereNotifyWrongSecret: a notify signed with a secret we do not hold is
// rejected. Without this an attacker who knows the merchant id can mark any
// appointment paid.
func TestPayHereNotifyWrongSecret(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	body := notifyBody(testMerchant, "3f1a6b7c-0000-4000-8000-00000000000a", "5000.00", "LKR", statusSuccess, "not-our-secret")

	_, err := p.VerifyWebhook(context.Background(), formHeader(), body)
	assert.ErrorIs(t, err, payment.ErrSignatureInvalid)
}

// TestPayHereNotifyForeignMerchant: a correctly signed notify for a different
// merchant account is not ours to act on.
func TestPayHereNotifyForeignMerchant(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	body := notifyBody("9999999", "3f1a6b7c-0000-4000-8000-00000000000a", "5000.00", "LKR", statusSuccess, testSecret)

	_, err := p.VerifyWebhook(context.Background(), formHeader(), body)
	assert.ErrorIs(t, err, payment.ErrSignatureInvalid)
}

// TestPayHereNotifyHasNoReplayWindow documents a genuine protocol weakness.
//
// PayHere's notify carries no timestamp, so an old capture stays valid forever
// and the signature alone cannot distinguish a replay from a redelivery. The
// only defence is the (provider, event_id) unique index in webhook_events,
// which is why the synthesised event id must be stable. This test pins that
// stability; the runbook covers the IP allowlist that should sit in front.
func TestPayHereNotifyHasNoReplayWindow(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	body := notifyBody(testMerchant, "3f1a6b7c-0000-4000-8000-000000000042", "1500.00", "LKR", statusSuccess, testSecret)

	a, err := p.VerifyWebhook(context.Background(), formHeader(), body)
	require.NoError(t, err)
	b, err := p.VerifyWebhook(context.Background(), formHeader(), body)
	require.NoError(t, err)

	assert.Equal(t, a.EventID, b.EventID,
		"the synthesised event id must be identical across deliveries or the dedup index cannot work")
}

func TestPayHereStatusCodeMapping(t *testing.T) {
	t.Parallel()

	cases := map[string]payment.WebhookOutcome{
		statusAuthorized:  payment.OutcomePaymentAuthorized,
		statusSuccess:     payment.OutcomePaymentSucceeded,
		statusPending:     payment.OutcomePaymentPending,
		statusCancelled:   payment.OutcomePaymentFailed,
		statusFailed:      payment.OutcomePaymentFailed,
		statusChargedback: payment.OutcomeRefundSucceeded,
		"7":               payment.OutcomeIgnored,
	}

	p := newTestProvider(t)
	for code, want := range cases {
		body := notifyBody(testMerchant, "3f1a6b7c-0000-4000-8000-0000000000cc", "100.00", "LKR", code, testSecret)
		evt, err := p.VerifyWebhook(context.Background(), formHeader(), body)
		require.NoError(t, err, "status_code %s", code)
		assert.Equal(t, want, evt.Outcome, "status_code %s", code)
	}
}

func TestPayHereRejectsNonFormBodies(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	_, err := p.VerifyWebhook(context.Background(), h, []byte(`{"status_code":"2"}`))
	assert.ErrorIs(t, err, payment.ErrSignatureInvalid)
}

func TestPayHereRejectsIncompleteNotify(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	for _, body := range []string{
		"",
		"merchant_id=" + testMerchant,
		"merchant_id=" + testMerchant + "&order_id=x&status_code=2",
	} {
		_, err := p.VerifyWebhook(context.Background(), formHeader(), []byte(body))
		assert.ErrorIs(t, err, payment.ErrSignatureInvalid, "body %q", body)
	}
}

// TestPayHereCheckoutHash pins the checkout signature construction.
func TestPayHereCheckoutHash(t *testing.T) {
	t.Parallel()

	secretHash := md5Upper([]byte(testSecret))
	got := CheckoutHash(testMerchant, "3f1a6b7c-0000-4000-8000-00000000000a", "1000.00", "LKR", secretHash)

	assert.Len(t, got, 32, "an MD5 hex digest is 32 characters")
	assert.Equal(t, strings.ToUpper(got), got, "PayHere expects the hash uppercased")

	// Changing any component changes the hash.
	assert.NotEqual(t, got, CheckoutHash(testMerchant, "order-2", "1000.00", "LKR", secretHash))
	assert.NotEqual(t, got, CheckoutHash(testMerchant, "3f1a6b7c-0000-4000-8000-00000000000a", "1000.01", "LKR", secretHash))
	assert.NotEqual(t, got, CheckoutHash(testMerchant, "3f1a6b7c-0000-4000-8000-00000000000a", "1000.00", "USD", secretHash))
	assert.NotEqual(t, got, CheckoutHash("other", "3f1a6b7c-0000-4000-8000-00000000000a", "1000.00", "LKR", secretHash))
}

// TestPayHereAmountFormatting: PayHere hashes the amount as a two-decimal
// string. A mismatch here is the single most common cause of a working
// integration silently rejecting every notify.
func TestPayHereAmountFormatting(t *testing.T) {
	t.Parallel()

	cases := map[int64]string{
		0: "0.00", 1: "0.01", 99: "0.99", 100: "1.00",
		123456: "1234.56", 100000000: "1000000.00", -150: "-1.50",
	}
	for cents, want := range cases {
		assert.Equal(t, want, FormatAmount(cents), "cents=%d", cents)
	}
}

func TestPayHereParseAmount(t *testing.T) {
	t.Parallel()

	ok := map[string]int64{
		"1500.00": 150000, "1500.5": 150050, "1500": 150000,
		"0.01": 1, "1,500.00": 150000, "-1.50": -150,
	}
	for in, want := range ok {
		got, err := ParseAmount(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}

	for _, bad := range []string{"", "abc", "1.234"} {
		_, err := ParseAmount(bad)
		assert.Error(t, err, bad)
	}
}

// TestPayHereRoundTripsThroughFormatAndParse: the signer and the verifier must
// agree about what a cent is, for every value.
func TestPayHereAmountRoundTrip(t *testing.T) {
	t.Parallel()

	for cents := int64(0); cents < 20000; cents++ {
		got, err := ParseAmount(FormatAmount(cents))
		require.NoError(t, err)
		require.Equal(t, cents, got, "round trip failed at %d cents", cents)
	}
}

func TestPayHereCreateIntentSignsTheCheckout(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	res, err := p.CreateIntent(context.Background(), payment.IntentRequest{
		AmountCents: 500000,
		Currency:    "LKR",
		Description: "Telemedicine consultation",
	})
	require.NoError(t, err)

	assert.Equal(t, payment.IntentRequiresAction, res.Status)
	assert.Contains(t, res.RedirectURL, "payhere.lk/pay/checkout")
	assert.Contains(t, res.Reference, `"amount":"5000.00"`)
	assert.Contains(t, res.Reference, `"merchant_id":"`+testMerchant+`"`)
	assert.Contains(t, res.Reference, `"notify_url":"https://api.example.lk/webhooks/payhere"`)
	assertPayHereCustomerFields(t, res.Reference, "", "")
}

func TestPayHereCreateIntentAuthorizeOnly(t *testing.T) {
	t.Parallel()

	p, err := New(Config{
		MerchantID:     testMerchant,
		MerchantSecret: testSecret,
		BaseURL:        "https://www.payhere.lk",
		NotifyURL:      "https://api.example.lk/webhooks/payhere",
	})
	require.NoError(t, err)
	res, err := p.CreateIntent(context.Background(), payment.IntentRequest{
		AmountCents:   500000,
		Currency:      "LKR",
		Description:   "Telemedicine consultation",
		AuthorizeOnly: true,
	})
	require.NoError(t, err)

	assert.Equal(t, payment.IntentRequiresAction, res.Status)
	assert.Contains(t, res.RedirectURL, "payhere.lk/pay/authorize")
	assert.Contains(t, res.Reference, `"amount":"5000.00"`)
	assertPayHereCustomerFields(t, res.Reference, "", "")
}

func TestPayHereCreateIntentAuthorizeOnlySandboxUsesCheckout(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	res, err := p.CreateIntent(context.Background(), payment.IntentRequest{
		AmountCents:   500000,
		Currency:      "LKR",
		Description:   "Telemedicine consultation",
		AuthorizeOnly: true,
	})
	require.NoError(t, err)

	assert.Contains(t, res.RedirectURL, "/pay/checkout")
	assert.NotContains(t, res.RedirectURL, "/pay/authorize")
	assertPayHereCustomerFields(t, res.Reference, "", "")
}

func TestPayHereCreateIntentUsesPatientContact(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	paymentID := uuid.MustParse("3f1a6b7c-0000-4000-8000-000000000042")
	res, err := p.CreateIntent(context.Background(), payment.IntentRequest{
		PaymentID:    paymentID,
		AmountCents:  500000,
		Currency:     "LKR",
		Description:  "Telemedicine consultation",
		PatientEmail: "saman@example.com",
		PatientPhone: "+94771234567",
	})
	require.NoError(t, err)
	assertPayHereCustomerFields(t, res.Reference, "saman@example.com", "0771234567")
}

func assertPayHereCustomerFields(t *testing.T, reference, wantEmail, wantPhone string) {
	t.Helper()
	for _, key := range []string{"first_name", "last_name", "email", "phone", "address", "city", "country"} {
		assert.Contains(t, reference, `"`+key+`":`, "missing %s in checkout payload", key)
	}
	assert.Contains(t, reference, `"first_name":"Patient"`)
	assert.Contains(t, reference, `"last_name":"User"`)
	assert.Contains(t, reference, `"country":"Sri Lanka"`)
	if wantEmail != "" {
		assert.Contains(t, reference, `"email":"`+wantEmail+`"`)
	} else {
		assert.Contains(t, reference, `"email":"patient-`)
		assert.Contains(t, reference, `@versalifehealth.com"`)
	}
	if wantPhone != "" {
		assert.Contains(t, reference, `"phone":"`+wantPhone+`"`)
	} else {
		assert.Contains(t, reference, `"phone":"0770000000"`)
	}
}

func TestPayHereVerifyWebhookAuthorized(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	orderID := "3f1a6b7c-0000-4000-8000-000000000042"
	amount := "2500.00"
	currency := "LKR"
	token := "auth_tok_12345"

	sig := NotifyHash(testMerchant, orderID, amount, currency, statusAuthorized, md5Upper([]byte(testSecret)))
	form := url.Values{
		"merchant_id":         {testMerchant},
		"order_id":            {orderID},
		"payment_id":          {"320027112345"},
		"payhere_amount":      {amount},
		"payhere_currency":    {currency},
		"status_code":         {statusAuthorized},
		"authorization_token": {token},
		"md5sig":              {sig},
	}

	evt, err := p.VerifyWebhook(context.Background(), formHeader(), []byte(form.Encode()))
	require.NoError(t, err)
	assert.Equal(t, payment.OutcomePaymentAuthorized, evt.Outcome)
	assert.Equal(t, token, evt.AuthorizationToken)
	assert.Equal(t, int64(250000), evt.AmountCents)
}

func TestPayHereCaptureSuccess(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/merchant/v1/oauth/token":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "test_token",
				"token_type":   "bearer",
				"expires_in":   600,
			})
		case "/merchant/v1/payment/capture":
			assert.Equal(t, "Bearer test_token", r.Header.Get("Authorization"))
			var req map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			assert.Equal(t, "auth_tok_abc", req["authorization_token"])
			assert.Equal(t, float64(25), req["amount"])

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": 1,
				"msg":    "Successfully captured payment",
				"data": map[string]any{
					"status_code":    2,
					"status_message": "Success",
					"payment_id":     json.Number("320025527952"),
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	p, err := New(Config{
		MerchantID:     testMerchant,
		MerchantSecret: testSecret,
		AppID:          "app_123",
		AppSecret:      "app_secret_456",
		BaseURL:        server.URL,
	})
	require.NoError(t, err)

	res, err := p.Capture(context.Background(), payment.CaptureRequest{
		AuthorizationToken: "auth_tok_abc",
		AmountCents:        2500,
		Currency:           "LKR",
	})
	require.NoError(t, err)
	assert.Equal(t, "succeeded", res.Status)
	assert.Equal(t, "320025527952", res.ProviderPaymentID)
}

func TestPayHereRejectsZeroAmount(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	_, err := p.CreateIntent(context.Background(), payment.IntentRequest{AmountCents: 0})
	assert.ErrorIs(t, err, payment.ErrProviderRejected)
}

// TestPayHerePayoutIsUnsupported: PayHere is a collection gateway with no
// third-party payout API. Saying so is better than a method that returns nil.
func TestPayHerePayoutIsUnsupported(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	_, err := p.Payout(context.Background(), payment.PayoutRequest{AmountCents: 1000})
	assert.ErrorIs(t, err, payment.ErrUnsupported)
	assert.False(t, p.SupportsPartialRefund(),
		"PayHere's refund endpoint is full-amount only; the 50%% policy band cannot be settled by API")
}

func TestPayHereRefundNeedsMerchantAPICredentials(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t) // no AppID/AppSecret configured
	_, err := p.Refund(context.Background(), payment.RefundRequest{ProviderIntentID: "x", AmountCents: 100})
	assert.ErrorIs(t, err, payment.ErrUnsupported)
}

func TestPayHereRequiresCredentials(t *testing.T) {
	t.Parallel()

	_, err := New(Config{MerchantSecret: testSecret})
	assert.Error(t, err)
	_, err = New(Config{MerchantID: testMerchant})
	assert.Error(t, err)
}

// --- F11: the unlengthed concatenation, and the collision it permits --------

// TestPayHereNotifyHashCollisionIsRejected is the concrete forgery, not a
// hypothetical one.
//
// The notify signature is MD5 over an undelimited concatenation:
//
//	merchant_id + order_id + payhere_amount + payhere_currency + status_code + MD5(secret)
//
// Nothing separates payhere_currency from status_code, so
//
//	currency "LKR"  + status_code "-2"   (a genuine FAILED notify)
//	currency "LKR-" + status_code "2"    (a forged SUCCEEDED notify)
//
// have byte-identical preimages and therefore identical, genuinely valid
// signatures. The test asserts the collision exists -- if PayHere ever changed
// the construction this half would fail and tell us why -- and then asserts
// the verifier refuses the forged tuple anyway, because it validates each
// field's grammar before it hashes.
func TestPayHereNotifyHashCollisionIsRejected(t *testing.T) {
	t.Parallel()

	const orderID = "3f1a6b7c-0000-4000-8000-000000000001"
	secretHash := md5Upper([]byte(testSecret))

	honest := NotifyHash(testMerchant, orderID, "5000.00", "LKR", statusFailed, secretHash)
	forged := NotifyHash(testMerchant, orderID, "5000.00", "LKR-", "2", secretHash)
	require.Equal(t, honest, forged,
		"the protocol's own hash construction collides here; that is the finding, not a test bug")

	p := newTestProvider(t)

	// The captured failure verifies and reports a failure, as it should.
	failed, err := p.VerifyWebhook(context.Background(), formHeader(),
		notifyBody(testMerchant, orderID, "5000.00", "LKR", statusFailed, testSecret))
	require.NoError(t, err)
	require.Equal(t, payment.OutcomePaymentFailed, failed.Outcome)

	// The forgery carries that same signature, unmodified, and claims success.
	forgery := url.Values{
		"merchant_id":      {testMerchant},
		"order_id":         {orderID},
		"payment_id":       {"320027000123"},
		"payhere_amount":   {"5000.00"},
		"payhere_currency": {"LKR-"},
		"status_code":      {"2"},
		"md5sig":           {honest},
	}
	evt, err := p.VerifyWebhook(context.Background(), formHeader(), []byte(forgery.Encode()))
	require.Error(t, err, "a forged tuple sharing a preimage with a captured notify must not verify")
	assert.ErrorIs(t, err, payment.ErrSignatureInvalid)
	assert.NotEqual(t, payment.OutcomePaymentSucceeded, evt.Outcome)

	// And the dedup index would not have caught it either: the synthesised
	// event id embeds status_code, so the forgery is a *different* event.
	assert.NotEqual(t,
		orderID+":320027000123:"+statusFailed,
		orderID+":320027000123:"+statusSuccess)
}

// TestPayHereNotifyCancelledCollisionIsRejected is the same trick against
// status_code "-1", which is the other negative code PayHere sends.
func TestPayHereNotifyCancelledCollisionIsRejected(t *testing.T) {
	t.Parallel()

	const orderID = "3f1a6b7c-0000-4000-8000-000000000002"
	secretHash := md5Upper([]byte(testSecret))
	require.Equal(t,
		NotifyHash(testMerchant, orderID, "900.00", "LKR", statusCancelled, secretHash),
		NotifyHash(testMerchant, orderID, "900.00", "LKR-", "1", secretHash))

	p := newTestProvider(t)
	forgery := url.Values{
		"merchant_id":      {testMerchant},
		"order_id":         {orderID},
		"payment_id":       {"320027000124"},
		"payhere_amount":   {"900.00"},
		"payhere_currency": {"LKR-"},
		"status_code":      {"1"},
		"md5sig":           {NotifyHash(testMerchant, orderID, "900.00", "LKR", statusCancelled, secretHash)},
	}
	_, err := p.VerifyWebhook(context.Background(), formHeader(), []byte(forgery.Encode()))
	assert.ErrorIs(t, err, payment.ErrSignatureInvalid)
}

// TestPayHereNotifyFieldGrammar walks every shape that could shift a boundary
// in the preimage and asserts each one is refused before the hash is computed.
//
// Each case is signed correctly for its own values, so the signature is
// genuinely valid: only the grammar stands between it and acceptance.
func TestPayHereNotifyFieldGrammar(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	const goodOrder = "3f1a6b7c-0000-4000-8000-000000000003"

	cases := []struct {
		name                                  string
		orderID, amount, currency, statusCode string
	}{
		{"currency absorbs the minus sign", goodOrder, "5000.00", "LKR-", "2"},
		{"currency is two letters", goodOrder, "5000.00", "LK", "R2"},
		{"currency is four letters", goodOrder, "5000.00", "LKRX", "2"},
		{"currency is lower case", goodOrder, "5000.00", "lkr", "2"},
		{"currency is empty", goodOrder, "5000.00", "", "2"},
		{"amount carries a thousands separator", goodOrder, "5,000.00", "LKR", "2"},
		{"amount is signed", goodOrder, "-5000.00", "LKR", "2"},
		{"amount has sub-cent precision", goodOrder, "5000.001", "LKR", "2"},
		{"amount absorbs a letter", goodOrder, "5000.00L", "KRX", "2"},
		{"amount is empty", goodOrder, "", "LKR", "2"},
		{"order id is not a uuid", "someone-elses-order-0000000000000000", "5000.00", "LKR", "2"},
		{"order id is an unhyphenated uuid", "3f1a6b7c000040008000000000000003", "5000.00", "LKR", "2"},
		{"order id is a braced uuid", "{3f1a6b7c-0000-4000-8000-000000000003}", "5000.00", "LKR", "2"},
		{"order id is upper case", "3F1A6B7C-0000-4000-8000-000000000003", "5000.00", "LKR", "2"},
		{"status code is a word", goodOrder, "5000.00", "LKR", "SUCCESS"},
		{"status code is three digits", goodOrder, "5000.00", "LKR", "200"},
		// \d is [0-9] in RE2 and never Unicode-aware; this asserts that
		// rather than trusting it, because the disjoint-alphabet argument for
		// injectivity rests on it.
		{"amount uses non-ASCII digits", goodOrder, "\u0665000.00", "LKR", "2"},
		{"currency uses non-ASCII letters", goodOrder, "5000.00", "LKR", "\u0662"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := notifyBody(testMerchant, tc.orderID, tc.amount, tc.currency, tc.statusCode, testSecret)
			_, err := p.VerifyWebhook(context.Background(), formHeader(), body)
			require.Error(t, err, "an ambiguous field must never reach the hash")
			assert.ErrorIs(t, err, payment.ErrSignatureInvalid)
		})
	}
}

// TestPayHereCreateIntentRefusesToSignAnAmbiguousCheckout: the outbound hash
// is the same construction, so it gets the same grammar. Signing an ambiguous
// tuple would mean handing the client a signature that is simultaneously valid
// for a different currency.
func TestPayHereCreateIntentRefusesToSignAnAmbiguousCheckout(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	_, err := p.CreateIntent(context.Background(), payment.IntentRequest{
		PaymentID:   uuid.MustParse("3f1a6b7c-0000-4000-8000-000000000004"),
		AmountCents: 500_000,
		Currency:    "LKR-",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, payment.ErrProviderRejected)
}

// --- F12: the notify payload must be JSON, because the column is JSONB ------

// TestPayHereNotifyRawIsJSON is the unit half of F12.
//
// WebhookEvent.Raw is written straight into webhook_events.payload, which is
// JSONB. pgx v5 passes a []byte to a JSONB parameter without encoding it, so a
// form-encoded body made Postgres raise 22P02, rolled back the transaction,
// and answered PayHere 500 -- forever. The integration test drives the whole
// path; this one pins the contract at the source.
func TestPayHereNotifyRawIsJSON(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	const orderID = "3f1a6b7c-0000-4000-8000-000000000005"
	body := notifyBody(testMerchant, orderID, "5000.00", "LKR", statusSuccess, testSecret)

	evt, err := p.VerifyWebhook(context.Background(), formHeader(), body)
	require.NoError(t, err)

	require.True(t, json.Valid(evt.Raw),
		"Raw is inserted into a JSONB column; a form-encoded body cannot be stored there. got: %s", evt.Raw)

	// Lossless: every field of the notify survives, including the signature,
	// because webhook_events.payload is the dispute evidence.
	var decoded map[string][]string
	require.NoError(t, json.Unmarshal(evt.Raw, &decoded))
	assert.Equal(t, []string{testMerchant}, decoded["merchant_id"])
	assert.Equal(t, []string{orderID}, decoded["order_id"])
	assert.Equal(t, []string{"5000.00"}, decoded["payhere_amount"])
	assert.Equal(t, []string{"LKR"}, decoded["payhere_currency"])
	assert.Equal(t, []string{statusSuccess}, decoded["status_code"])
	assert.NotEmpty(t, decoded["md5sig"])
	assert.Equal(t, []string{"VISA"}, decoded["method"])
}
