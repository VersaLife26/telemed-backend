package dialogprovider

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"telemed/internal/domain/payment/payment"
)

const testWebhookSecret = "dialog-shared-secret-not-a-real-one"

func newTestProvider(t *testing.T, baseURL string) *Provider {
	t.Helper()
	p, err := New(Config{
		ApplicationID: "APP_000001",
		Password:      "app-password",
		BaseURL:       baseURL,
		WebhookSecret: testWebhookSecret,
		Tolerance:     5 * time.Minute,
	})
	require.NoError(t, err)
	return p
}

func signCallback(secret string, at time.Time, body []byte) http.Header {
	ts := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)

	h := http.Header{}
	h.Set("X-Ideamart-Signature", hex.EncodeToString(mac.Sum(nil)))
	h.Set("X-Ideamart-Timestamp", ts)
	return h
}

func callbackBody(t *testing.T, status, internalTrx, externalTrx, amount string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]string{
		"statusCode":    status,
		"statusDetail":  "callback",
		"internalTrxId": internalTrx,
		"externalTrxId": externalTrx,
		"amount":        amount,
		"currencyCode":  "LKR",
	})
	require.NoError(t, err)
	return b
}

func TestDialogCallbackValidSignature(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, "https://api.example.invalid")
	body := callbackBody(t, statusSuccess, "TRX-100", "ext-1", "1500.00")

	evt, err := p.VerifyWebhook(context.Background(), signCallback(testWebhookSecret, time.Now(), body), body)
	require.NoError(t, err)

	assert.Equal(t, payment.OutcomePaymentSucceeded, evt.Outcome)
	assert.Equal(t, "TRX-100:S1000", evt.EventID)
	assert.Equal(t, "TRX-100", evt.ProviderIntentID)
	assert.Equal(t, int64(150000), evt.AmountCents)
	assert.Equal(t, "LKR", evt.Currency)
}

func TestDialogCallbackTamperedBody(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, "https://api.example.invalid")
	original := callbackBody(t, statusInsufficientFunds, "TRX-100", "ext-1", "1500.00")
	headers := signCallback(testWebhookSecret, time.Now(), original)

	// Flip the failure into a success while keeping the original signature.
	tampered := callbackBody(t, statusSuccess, "TRX-100", "ext-1", "1500.00")

	_, err := p.VerifyWebhook(context.Background(), headers, tampered)
	assert.ErrorIs(t, err, payment.ErrSignatureInvalid)
}

func TestDialogCallbackWrongSecret(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, "https://api.example.invalid")
	body := callbackBody(t, statusSuccess, "TRX-100", "ext-1", "1500.00")

	_, err := p.VerifyWebhook(context.Background(), signCallback("some-other-secret", time.Now(), body), body)
	assert.ErrorIs(t, err, payment.ErrSignatureInvalid)
}

// TestDialogCallbackExpiredTimestamp: Ideamart signs nothing, so the timestamp
// window is ours. Without it a captured callback replays forever.
func TestDialogCallbackExpiredTimestamp(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, "https://api.example.invalid")
	body := callbackBody(t, statusSuccess, "TRX-100", "ext-1", "1500.00")
	old := time.Now().Add(-2 * time.Hour)

	_, err := p.VerifyWebhook(context.Background(), signCallback(testWebhookSecret, old, body), body)
	assert.ErrorIs(t, err, payment.ErrEventTooOld)
}

// TestDialogCallbackFutureTimestamp: a clock far ahead is as suspicious as one
// far behind, and is how an attacker would try to buy an unlimited window.
func TestDialogCallbackFutureTimestamp(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, "https://api.example.invalid")
	body := callbackBody(t, statusSuccess, "TRX-100", "ext-1", "1500.00")
	future := time.Now().Add(2 * time.Hour)

	_, err := p.VerifyWebhook(context.Background(), signCallback(testWebhookSecret, future, body), body)
	assert.ErrorIs(t, err, payment.ErrEventTooOld)
}

func TestDialogCallbackMissingSignature(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, "https://api.example.invalid")
	body := callbackBody(t, statusSuccess, "TRX-100", "ext-1", "1500.00")

	_, err := p.VerifyWebhook(context.Background(), http.Header{}, body)
	assert.ErrorIs(t, err, payment.ErrSignatureInvalid)
}

// TestDialogRefusesToStartWithoutASecret: an unsigned webhook endpoint is an
// open door, so it must be opted into explicitly and loudly.
func TestDialogRefusesToStartWithoutASecret(t *testing.T) {
	t.Parallel()

	_, err := New(Config{ApplicationID: "APP", Password: "p"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DIALOG_WEBHOOK_SECRET")

	relaxed, err := New(Config{ApplicationID: "APP", Password: "p", AllowUnsigned: true})
	require.NoError(t, err, "the downgrade is available, but only by explicit configuration, "+
		"and the deployment must also supply DIALOG_WEBHOOK_CIDRS before it will boot")

	body := callbackBody(t, statusSuccess, "TRX", "ext", "10.00")
	evt, err := relaxed.VerifyWebhook(context.Background(), http.Header{}, body)
	require.NoError(t, err)
	assert.Equal(t, payment.OutcomePaymentSucceeded, evt.Outcome)
}

func TestDialogRequiresCredentials(t *testing.T) {
	t.Parallel()

	_, err := New(Config{Password: "p", WebhookSecret: "s"})
	assert.Error(t, err)
	_, err = New(Config{ApplicationID: "APP", WebhookSecret: "s"})
	assert.Error(t, err)
}

// --- the two-step PIN flow -------------------------------------------------

// ideamartStub is a fake Ideamart CaaS endpoint. It records what it was asked
// so the test can assert the flow really is two steps against the carrier.
type ideamartStub struct {
	otpRequests  int
	otpVerifies  int
	debits       int
	lastDebit    debitBody
	otpStatus    string
	verifyStatus string
	debitStatus  string
}

func (s *ideamartStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/caas/direct/otp/request", func(w http.ResponseWriter, r *http.Request) {
		s.otpRequests++
		writeJSON(t, w, otpRequestResponse{
			StatusCode: orDefault(s.otpStatus, statusSuccess), ReferenceNo: "REF-9001",
		})
	})
	mux.HandleFunc("/caas/direct/otp/verify", func(w http.ResponseWriter, r *http.Request) {
		s.otpVerifies++
		writeJSON(t, w, otpVerifyResponse{
			StatusCode:   orDefault(s.verifyStatus, statusSuccess),
			SubscriberID: "tel:+94771234567",
		})
	})
	mux.HandleFunc("/caas/direct/debit", func(w http.ResponseWriter, r *http.Request) {
		s.debits++
		require.NoError(t, json.NewDecoder(r.Body).Decode(&s.lastDebit))
		writeJSON(t, w, debitResponse{
			StatusCode:    orDefault(s.debitStatus, statusSuccess),
			InternalTrxID: "TRX-55555",
			ExternalTrxID: s.lastDebit.ExternalTrxID,
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(v))
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// TestDialogPINFlowIsTwoExplicitSteps is the heart of carrier billing.
//
// CreateIntent sends the PIN and reports requires_pin -- no money has moved.
// Only after the patient proves possession of the handset does the debit
// happen. Modelling this as one call would either block a request for minutes
// or lose the state entirely.
func TestDialogPINFlowIsTwoExplicitSteps(t *testing.T) {
	t.Parallel()

	stub := &ideamartStub{}
	srv := stub.server(t)
	p := newTestProvider(t, srv.URL)

	paymentID := uuid.New()

	// Step one: send the PIN.
	intent, err := p.CreateIntent(context.Background(), payment.IntentRequest{
		PaymentID:      paymentID,
		AmountCents:    150000,
		Currency:       "LKR",
		PatientPhone:   "+94771234567",
		IdempotencyKey: "appointment:" + paymentID.String(),
	})
	require.NoError(t, err)

	assert.Equal(t, payment.IntentRequiresPIN, intent.Status,
		"no money may have moved after step one")
	assert.Equal(t, "REF-9001", intent.Reference)
	require.NotNil(t, intent.ExpiresAt)
	assert.Equal(t, 1, stub.otpRequests)
	assert.Equal(t, 0, stub.debits, "the debit must not happen until the PIN is confirmed")

	// Step two: the patient types the PIN.
	result, err := p.ConfirmPIN(context.Background(), payment.PINConfirmation{
		PaymentID:      paymentID,
		Reference:      intent.Reference,
		PIN:            "482913",
		AmountCents:    150000,
		Currency:       "LKR",
		IdempotencyKey: "appointment:" + paymentID.String(),
	})
	require.NoError(t, err)

	assert.Equal(t, payment.IntentSucceeded, result.Status)
	assert.Equal(t, "TRX-55555", result.ProviderIntentID)
	assert.Equal(t, 1, stub.otpVerifies)
	assert.Equal(t, 1, stub.debits)

	// The debit must carry our idempotency key as externalTrxId, which is what
	// stops a retry charging the phone bill twice.
	assert.Equal(t, "appointment:"+paymentID.String(), stub.lastDebit.ExternalTrxID)
	assert.Equal(t, "1500.00", stub.lastDebit.Amount)
	assert.Equal(t, "Mobile Account", stub.lastDebit.PaymentInstrumentName)
	assert.Equal(t, "tel:+94771234567", stub.lastDebit.SubscriberID)
}

func TestDialogWrongPINDoesNotDebit(t *testing.T) {
	t.Parallel()

	stub := &ideamartStub{verifyStatus: statusInvalidOTP}
	srv := stub.server(t)
	p := newTestProvider(t, srv.URL)

	_, err := p.ConfirmPIN(context.Background(), payment.PINConfirmation{
		Reference: "REF-9001", PIN: "000000", AmountCents: 1000, IdempotencyKey: "k",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, payment.ErrProviderRejected)
	assert.Contains(t, err.Error(), "PIN was not correct")
	assert.Equal(t, 0, stub.debits, "a failed PIN must never reach the debit endpoint")
}

func TestDialogInsufficientBalanceIsARejection(t *testing.T) {
	t.Parallel()

	stub := &ideamartStub{debitStatus: statusInsufficientFunds}
	srv := stub.server(t)
	p := newTestProvider(t, srv.URL)

	_, err := p.ConfirmPIN(context.Background(), payment.PINConfirmation{
		Reference: "REF", PIN: "123456", AmountCents: 1000, IdempotencyKey: "k",
	})
	assert.ErrorIs(t, err, payment.ErrProviderRejected,
		"an empty phone account is a decision, not a transient failure; retrying will not help")
}

func TestDialogCarrierOutageIsRetryable(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	_, err := p.SendPIN(context.Background(), payment.IntentRequest{PatientPhone: "+94771234567"})
	assert.ErrorIs(t, err, payment.ErrProviderUnavailable)
}

func TestDialogNeedsAPhoneNumber(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, "https://api.example.invalid")
	_, err := p.CreateIntent(context.Background(), payment.IntentRequest{AmountCents: 1000})
	assert.ErrorIs(t, err, payment.ErrProviderRejected)
	assert.Contains(t, err.Error(), "mobile number")
}

func TestDialogConfirmPINNeedsAChallenge(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, "https://api.example.invalid")
	_, err := p.ConfirmPIN(context.Background(), payment.PINConfirmation{PIN: "123456"})
	assert.ErrorIs(t, err, payment.ErrProviderRejected)

	_, err = p.ConfirmPIN(context.Background(), payment.PINConfirmation{Reference: "REF"})
	assert.ErrorIs(t, err, payment.ErrProviderRejected)
}

func TestDialogRefundAndPayoutAreUnsupported(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, "https://api.example.invalid")

	_, err := p.Refund(context.Background(), payment.RefundRequest{AmountCents: 100})
	assert.ErrorIs(t, err, payment.ErrUnsupported,
		"a charge to a phone bill cannot be reversed through the CaaS API")

	_, err = p.Payout(context.Background(), payment.PayoutRequest{AmountCents: 100})
	assert.ErrorIs(t, err, payment.ErrUnsupported)
}

func TestDialogSubscriberID(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "tel:+94771234567", SubscriberID("+94771234567"))
	assert.Equal(t, "tel:+94771234567", SubscriberID("tel:+94771234567"))
	assert.Equal(t, "", SubscriberID(""))
}

func TestDialogAmountFormatting(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "1500.00", FormatAmount(150000))
	assert.Equal(t, "0.01", FormatAmount(1))
	assert.Equal(t, "0.00", FormatAmount(0))

	got, err := ParseAmount("1500.5")
	require.NoError(t, err)
	assert.Equal(t, int64(150050), got)

	_, err = ParseAmount("1.234")
	assert.Error(t, err)
}

// TestDialogCallbackReasonsCarryNoSubscriberData: a carrier's own status detail
// can include the MSISDN, and that must never reach a patient-facing message
// or a log line.
func TestDialogCallbackReasonsCarryNoSubscriberData(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, "https://api.example.invalid")
	body, err := json.Marshal(map[string]string{
		"statusCode":    statusInsufficientFunds,
		"statusDetail":  "subscriber tel:+94771234567 has no balance",
		"internalTrxId": "TRX-1",
		"amount":        "10.00",
	})
	require.NoError(t, err)

	evt, err := p.VerifyWebhook(context.Background(), signCallback(testWebhookSecret, time.Now(), body), body)
	require.NoError(t, err)
	assert.Equal(t, "insufficient balance on the mobile account", evt.FailureReason)
	assert.NotContains(t, evt.FailureReason, "94771234567")
}

// --- F9: the two ways a configured secret used to protect nothing ----------

// TestDialogRefusesASecretAlongsideTheUnsignedDowngrade.
//
// AllowUnsigned used to mean two different things depending on whether a secret
// was set: "no secret configured" (the documented meaning) and "a secret is
// configured but the signature header is optional" (the accidental one). In the
// second, an attacker simply omitted the header and no verification ran at all
// -- a deployment that believes it is signed, and is not.
//
// The combination is now refused at construction, so the ambiguous state cannot
// be configured rather than merely being discouraged.
func TestDialogRefusesASecretAlongsideTheUnsignedDowngrade(t *testing.T) {
	t.Parallel()

	_, err := New(Config{
		ApplicationID: "APP", Password: "p",
		WebhookSecret: testWebhookSecret, AllowUnsigned: true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unset one of them")
}

// TestDialogSignatureIsRequiredWheneverASecretExists: with a secret configured,
// omitting the header must be a rejection and never a bypass.
func TestDialogSignatureIsRequiredWheneverASecretExists(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, "https://api.example.invalid")
	body := callbackBody(t, statusSuccess, "TRX-200", "ext-2", "1500.00")

	// No signature header at all: the shape the bypass used.
	_, err := p.VerifyWebhook(context.Background(), http.Header{}, body)
	require.Error(t, err)
	assert.ErrorIs(t, err, payment.ErrSignatureInvalid)

	// An empty signature header is the same thing spelled differently.
	h := http.Header{}
	h.Set("X-Ideamart-Signature", "   ")
	_, err = p.VerifyWebhook(context.Background(), h, body)
	assert.ErrorIs(t, err, payment.ErrSignatureInvalid)
}

// TestDialogSignedCallbackWithoutATimestampIsRejected.
//
// The HMAC covers timestamp + "." + body, so a callback with no timestamp is
// signed over the body alone -- which means a captured callback stays valid for
// ever. verifySignature used to return nil for exactly this case whenever
// AllowUnsigned was set, and a replayed carrier-billing success marks a
// consultation paid every single time it is delivered.
func TestDialogSignedCallbackWithoutATimestampIsRejected(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, "https://api.example.invalid")
	body := callbackBody(t, statusSuccess, "TRX-300", "ext-3", "1500.00")

	// Correctly signed for the no-timestamp construction, so the HMAC itself
	// verifies. Only the replay bound is missing.
	mac := hmac.New(sha256.New, []byte(testWebhookSecret))
	mac.Write(body)
	h := http.Header{}
	h.Set("X-Ideamart-Signature", hex.EncodeToString(mac.Sum(nil)))

	_, err := p.VerifyWebhook(context.Background(), h, body)
	require.Error(t, err, "a callback with no timestamp has no replay window and must not be accepted")
	assert.ErrorIs(t, err, payment.ErrSignatureInvalid)
	assert.Contains(t, err.Error(), "timestamp")
}
