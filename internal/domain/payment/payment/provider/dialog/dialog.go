// Package dialogprovider implements carrier billing over Dialog Axiata's
// Ideamart CaaS (Charging as a Service) v2 API.
//
// This is the rail that matters most in Sri Lanka and the one the source
// documentation gives the least detail about. A large share of patients have a
// prepaid or postpaid mobile account and no card at all; oDoc built its
// consumer business on exactly this. A telemedicine platform that only takes
// cards is a platform for Colombo.
//
// # The two-step flow is modelled as two states, not one call
//
// Ideamart's direct debit is authorised by a PIN sent to the subscriber's
// handset. The documentation describes this as "send PIN, patient enters PIN,
// call debit" and then shows it as a single function. It is not one call and
// it must not be modelled as one:
//
//   - The patient may take a minute, or five, to read an SMS.
//   - The PIN may never arrive, or may be entered wrongly three times.
//   - The handset may be off.
//
// Collapsing that into one synchronous call means either a request that blocks
// for minutes or a payment whose state exists only in the memory of whichever
// pod happened to serve it. So the payment moves pending -> requires_pin ->
// succeeded|failed, each transition durable in Postgres, and an operator can
// see a patient parked at requires_pin and understand exactly what happened.
package dialogprovider

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"telemed/internal/domain/payment/payment"
	"telemed/internal/platform/logger"
)

// Ideamart status codes. S1000 is the universal success marker across the CaaS
// family; the rest are the failures worth distinguishing.
const (
	statusSuccess           = "S1000"
	statusInvalidOTP        = "E1313"
	statusInsufficientFunds = "E1309"
	statusSubscriberBlocked = "E1312"
)

// Config configures the Dialog rail.
type Config struct {
	// ApplicationID and Password are issued by Ideamart per application.
	ApplicationID string
	Password      string
	// BaseURL is https://api.ideamart.io (production) or the sandbox host.
	BaseURL string

	// WebhookSecret authenticates Dialog's status callbacks.
	//
	// Ideamart does not sign its callbacks. This is our own hardening: Dialog
	// is configured to send an HMAC-SHA256 of the body in a header, and we
	// verify it in constant time. Where that cannot be arranged, AllowUnsigned
	// downgrades the endpoint to IP-allowlist-only protection -- a real
	// downgrade, refused at boot unless DIALOG_WEBHOOK_CIDRS is set, because
	// the compensating control has to exist before the thing it compensates
	// for is allowed.
	//
	// AllowUnsigned means "no secret is configured", and nothing else. It is
	// NOT "a secret is configured but the header is optional": that shape let
	// an attacker simply omit the signature header and have no verification
	// run at all, on an endpoint that marks consultations paid. New() refuses
	// the combination outright rather than documenting the trap.
	WebhookSecret   string
	SignatureHeader string
	TimestampHeader string
	Tolerance       time.Duration
	AllowUnsigned   bool

	HTTPClient *http.Client
}

// Provider is the Dialog Ideamart implementation.
type Provider struct {
	cfg    Config
	client *http.Client
}

var (
	_ payment.PaymentProvider = (*Provider)(nil)
	_ payment.PINProvider     = (*Provider)(nil)
)

// New builds the provider.
func New(cfg Config) (*Provider, error) {
	if strings.TrimSpace(cfg.ApplicationID) == "" || strings.TrimSpace(cfg.Password) == "" {
		return nil, errors.New("dialog: DIALOG_APPLICATION_ID and DIALOG_PASSWORD are required")
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		cfg.BaseURL = "https://api.ideamart.io"
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.SignatureHeader == "" {
		cfg.SignatureHeader = "X-Ideamart-Signature"
	}
	if cfg.TimestampHeader == "" {
		cfg.TimestampHeader = "X-Ideamart-Timestamp"
	}
	if cfg.Tolerance <= 0 {
		cfg.Tolerance = 5 * time.Minute
	}
	if !cfg.AllowUnsigned && strings.TrimSpace(cfg.WebhookSecret) == "" {
		return nil, errors.New("dialog: DIALOG_WEBHOOK_SECRET is required unless DIALOG_ALLOW_UNSIGNED_WEBHOOKS is set")
	}
	// The dangerous middle state, refused structurally rather than handled.
	// With both set, VerifyWebhook used to skip verification entirely for a
	// request that simply left the header off -- a configured secret providing
	// no protection at all, which is worse than no secret, because the
	// deployment believes it is signed.
	if cfg.AllowUnsigned && strings.TrimSpace(cfg.WebhookSecret) != "" {
		return nil, errors.New("dialog: DIALOG_ALLOW_UNSIGNED_WEBHOOKS is set while DIALOG_WEBHOOK_SECRET is " +
			"configured; a configured secret is always required, so unset one of them")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Provider{cfg: cfg, client: client}, nil
}

// Name identifies the rail.
func (p *Provider) Name() string { return string(payment.ProviderDialog) }

// SubscriberID renders an E.164 mobile number in the tel: form Ideamart wants.
func SubscriberID(phone string) string {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return ""
	}
	if strings.HasPrefix(phone, "tel:") {
		return phone
	}
	return "tel:" + phone
}

// CreateIntent begins carrier billing by sending the PIN.
//
// It returns IntentRequiresPIN, never IntentSucceeded: no money has moved and
// none will until the patient proves possession of the handset.
func (p *Provider) CreateIntent(ctx context.Context, req payment.IntentRequest) (payment.IntentResult, error) {
	challenge, err := p.SendPIN(ctx, req)
	if err != nil {
		return payment.IntentResult{}, err
	}
	expires := challenge.ExpiresAt
	return payment.IntentResult{
		ProviderIntentID: challenge.Reference,
		Reference:        challenge.Reference,
		Status:           payment.IntentRequiresPIN,
		ExpiresAt:        &expires,
	}, nil
}

type otpRequestBody struct {
	ApplicationID       string `json:"applicationId"`
	Password            string `json:"password"`
	SubscriberID        string `json:"subscriberId"`
	ApplicationHash     string `json:"applicationHash,omitempty"`
	ApplicationMetaData string `json:"applicationMetaData,omitempty"`
}

type otpRequestResponse struct {
	StatusCode   string `json:"statusCode"`
	StatusDetail string `json:"statusDetail"`
	ReferenceNo  string `json:"referenceNo"`
}

// SendPIN issues the SMS challenge.
func (p *Provider) SendPIN(ctx context.Context, req payment.IntentRequest) (payment.ProviderPINChallenge, error) {
	subscriber := SubscriberID(req.PatientPhone)
	if subscriber == "" {
		return payment.ProviderPINChallenge{}, fmt.Errorf("%w: carrier billing needs the patient's mobile number", payment.ErrProviderRejected)
	}

	var res otpRequestResponse
	if err := p.post(ctx, "/caas/direct/otp/request", otpRequestBody{
		ApplicationID:       p.cfg.ApplicationID,
		Password:            p.cfg.Password,
		SubscriberID:        subscriber,
		ApplicationMetaData: "telemed consultation",
	}, &res); err != nil {
		return payment.ProviderPINChallenge{}, err
	}
	if res.StatusCode != statusSuccess || res.ReferenceNo == "" {
		return payment.ProviderPINChallenge{}, classifyStatus(res.StatusCode, "otp request")
	}

	return payment.ProviderPINChallenge{
		Reference: res.ReferenceNo,
		// Ideamart PINs are short-lived. Five minutes is the documented window
		// and is also what the patient is told in the SMS.
		ExpiresAt:    time.Now().Add(5 * time.Minute).UTC(),
		MaskedMSISDN: logger.MaskPhone(req.PatientPhone),
	}, nil
}

type otpVerifyBody struct {
	ApplicationID string `json:"applicationId"`
	Password      string `json:"password"`
	ReferenceNo   string `json:"referenceNo"`
	OTP           string `json:"otp"`
}

type otpVerifyResponse struct {
	StatusCode   string `json:"statusCode"`
	StatusDetail string `json:"statusDetail"`
	SubscriberID string `json:"subscriberId"`
}

type debitBody struct {
	ApplicationID         string `json:"applicationId"`
	Password              string `json:"password"`
	SubscriberID          string `json:"subscriberId"`
	PaymentInstrumentName string `json:"paymentInstrumentName"`
	Amount                string `json:"amount"`
	CurrencyCode          string `json:"currencyCode"`
	ExternalTrxID         string `json:"externalTrxId"`
}

type debitResponse struct {
	StatusCode    string `json:"statusCode"`
	StatusDetail  string `json:"statusDetail"`
	InternalTrxID string `json:"internalTrxId"`
	ExternalTrxID string `json:"externalTrxId"`
	TimeStamp     string `json:"timeStamp"`
}

// ConfirmPIN verifies the code and performs the debit.
//
// Verify and debit are two Ideamart calls but one logical step: a verified PIN
// is only good for the debit that immediately follows it. Splitting them into
// two of our states would invent a "PIN verified but not charged" state that
// does not exist at the carrier and could not be recovered from.
func (p *Provider) ConfirmPIN(ctx context.Context, req payment.PINConfirmation) (payment.IntentResult, error) {
	if req.Reference == "" {
		return payment.IntentResult{}, fmt.Errorf("%w: no PIN challenge is outstanding", payment.ErrProviderRejected)
	}
	if strings.TrimSpace(req.PIN) == "" {
		return payment.IntentResult{}, fmt.Errorf("%w: PIN is required", payment.ErrProviderRejected)
	}

	var verify otpVerifyResponse
	if err := p.post(ctx, "/caas/direct/otp/verify", otpVerifyBody{
		ApplicationID: p.cfg.ApplicationID,
		Password:      p.cfg.Password,
		ReferenceNo:   req.Reference,
		OTP:           strings.TrimSpace(req.PIN),
	}, &verify); err != nil {
		return payment.IntentResult{}, err
	}
	if verify.StatusCode != statusSuccess {
		return payment.IntentResult{}, classifyStatus(verify.StatusCode, "otp verify")
	}
	if verify.SubscriberID == "" {
		return payment.IntentResult{}, fmt.Errorf("%w: carrier verified the PIN without returning a subscriber", payment.ErrProviderUnavailable)
	}

	currency := strings.ToUpper(req.Currency)
	if currency == "" {
		currency = payment.CurrencyLKR
	}
	var debit debitResponse
	if err := p.post(ctx, "/caas/direct/debit", debitBody{
		ApplicationID:         p.cfg.ApplicationID,
		Password:              p.cfg.Password,
		SubscriberID:          verify.SubscriberID,
		PaymentInstrumentName: "Mobile Account",
		Amount:                FormatAmount(req.AmountCents),
		CurrencyCode:          currency,
		// The idempotency key travels as externalTrxId, which Ideamart treats
		// as the merchant's unique transaction reference. A retry with the same
		// value is rejected as a duplicate rather than charged twice.
		ExternalTrxID: req.IdempotencyKey,
	}, &debit); err != nil {
		return payment.IntentResult{}, err
	}
	if debit.StatusCode != statusSuccess {
		return payment.IntentResult{}, classifyStatus(debit.StatusCode, "debit")
	}

	return payment.IntentResult{
		ProviderIntentID: debit.InternalTrxID,
		Reference:        debit.ExternalTrxID,
		Status:           payment.IntentSucceeded,
	}, nil
}

// FormatAmount renders cents as the plain decimal Ideamart expects.
func FormatAmount(cents int64) string {
	neg := cents < 0
	if neg {
		cents = -cents
	}
	s := fmt.Sprintf("%d.%02d", cents/100, cents%100)
	if neg {
		return "-" + s
	}
	return s
}

// VerifyWebhook authenticates a Dialog status callback.
func (p *Provider) VerifyWebhook(_ context.Context, headers http.Header, body []byte) (payment.WebhookEvent, error) {
	if p.cfg.WebhookSecret != "" {
		// Unconditional. New() has already refused the AllowUnsigned+secret
		// combination, so there is no configuration in which a caller can opt
		// out of verification by omitting a header.
		sig := strings.TrimSpace(headers.Get(p.cfg.SignatureHeader))
		if sig == "" {
			return payment.WebhookEvent{}, fmt.Errorf("%w: missing %s", payment.ErrSignatureInvalid, p.cfg.SignatureHeader)
		}
		ts := strings.TrimSpace(headers.Get(p.cfg.TimestampHeader))
		if err := p.verifySignature(sig, ts, body); err != nil {
			return payment.WebhookEvent{}, err
		}
	} else if !p.cfg.AllowUnsigned {
		return payment.WebhookEvent{}, fmt.Errorf("%w: no webhook secret configured", payment.ErrSignatureInvalid)
	}

	var cb callbackPayload
	if err := json.Unmarshal(body, &cb); err != nil {
		return payment.WebhookEvent{}, fmt.Errorf("%w: unparseable callback body", payment.ErrSignatureInvalid)
	}
	if cb.ExternalTrxID == "" && cb.InternalTrxID == "" {
		return payment.WebhookEvent{}, fmt.Errorf("%w: callback identifies no transaction", payment.ErrSignatureInvalid)
	}

	amount, _ := ParseAmount(cb.Amount)
	out := payment.WebhookEvent{
		// Ideamart redelivers the same internalTrxId with the same status, so
		// the pair is a stable dedup key.
		EventID:          firstNonEmpty(cb.InternalTrxID, cb.ExternalTrxID) + ":" + cb.StatusCode,
		Type:             "dialog.caas." + cb.StatusCode,
		ProviderIntentID: cb.InternalTrxID,
		AmountCents:      amount,
		Currency:         strings.ToUpper(firstNonEmpty(cb.CurrencyCode, payment.CurrencyLKR)),
		OccurredAt:       time.Now().UTC(),
		Raw:              body,
	}
	if cb.StatusCode == statusSuccess {
		out.Outcome = payment.OutcomePaymentSucceeded
	} else {
		out.Outcome = payment.OutcomePaymentFailed
		out.FailureReason = carrierReason(cb.StatusCode)
	}
	return out, nil
}

type callbackPayload struct {
	StatusCode    string `json:"statusCode"`
	StatusDetail  string `json:"statusDetail"`
	InternalTrxID string `json:"internalTrxId"`
	ExternalTrxID string `json:"externalTrxId"`
	Amount        string `json:"amount"`
	CurrencyCode  string `json:"currencyCode"`
	TimeStamp     string `json:"timeStamp"`
}

// verifySignature checks the HMAC and the timestamp window.
func (p *Provider) verifySignature(sig, ts string, body []byte) error {
	mac := hmac.New(sha256.New, []byte(p.cfg.WebhookSecret))
	if ts != "" {
		mac.Write([]byte(ts))
		mac.Write([]byte("."))
	}
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	// Constant time, for the same reason as PayHere: a comparison that returns
	// early on the first differing byte tells an attacker how much of their
	// guess was right.
	if subtle.ConstantTimeCompare([]byte(expected), []byte(strings.ToLower(sig))) != 1 {
		return fmt.Errorf("%w: callback HMAC mismatch", payment.ErrSignatureInvalid)
	}

	// Without a timestamp there is no replay window, so a captured callback
	// stays valid forever. Always reject: this used to return nil whenever
	// AllowUnsigned was set, which made a signed-but-undated callback replayable
	// indefinitely -- and a replayed carrier-billing success marks a
	// consultation paid every time it is sent.
	if ts == "" {
		return fmt.Errorf("%w: callback carries no timestamp to bound a replay", payment.ErrSignatureInvalid)
	}
	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: callback timestamp is not a unix time", payment.ErrSignatureInvalid)
	}
	age := time.Since(time.Unix(secs, 0))
	if age < 0 {
		age = -age
	}
	if age > p.cfg.Tolerance {
		return fmt.Errorf("%w: callback is %s old", payment.ErrEventTooOld, age.Truncate(time.Second))
	}
	return nil
}

// Refund is not available on carrier billing.
//
// A charge to a phone bill cannot be reversed through the CaaS API; Dialog
// settles the reversal against the merchant's monthly reconciliation. Returning
// ErrUnsupported means the refund is recorded as pending with a clear reason
// and lands on the operator's reconciliation list, which is what actually
// happens in practice.
func (p *Provider) Refund(_ context.Context, _ payment.RefundRequest) (payment.RefundResult, error) {
	return payment.RefundResult{}, fmt.Errorf("%w: Dialog carrier billing has no refund API; reverse it in the monthly Ideamart reconciliation", payment.ErrUnsupported)
}

// Payout is not available: Ideamart bills subscribers, it does not pay
// merchants' suppliers.
func (p *Provider) Payout(_ context.Context, _ payment.PayoutRequest) (payment.PayoutResult, error) {
	return payment.PayoutResult{}, fmt.Errorf("%w: Dialog Ideamart cannot pay doctors; carrier revenue settles to the merchant under the 70/30 share", payment.ErrUnsupported)
}

// ParseAmount converts Ideamart's decimal amount string to integer cents.
func ParseAmount(s string) (int64, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", ""))
	if s == "" {
		return 0, nil
	}
	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("dialog: amount %q is not a number", s)
	}
	cents := w * 100
	if hasFrac && frac != "" {
		if len(frac) > 2 {
			return 0, fmt.Errorf("dialog: amount %q has sub-cent precision", s)
		}
		f, err := strconv.ParseInt(frac+strings.Repeat("0", 2-len(frac)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("dialog: amount %q is not a number", s)
		}
		cents += f
	}
	return cents, nil
}

// post issues one Ideamart call. Credentials go in the body because that is
// what the API requires; the body is never logged.
func (p *Provider) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("dialog: encode %s request: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: dialog %s: %w", payment.ErrProviderUnavailable, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: dialog %s: %w", payment.ErrProviderUnavailable, path, err)
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("%w: dialog %s returned %d", payment.ErrProviderUnavailable, path, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%w: dialog %s returned %d", payment.ErrProviderRejected, path, resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: dialog %s response unusable", payment.ErrProviderUnavailable, path)
	}
	return nil
}

// classifyStatus turns an Ideamart status code into one of our retry classes.
// classifyStatus turns an Ideamart status code into one of our retry classes.
//
// The carrier's own statusDetail string is deliberately NOT a parameter: it
// can contain the subscriber's MSISDN, and an error message ends up in logs.
func classifyStatus(code, op string) error {
	switch code {
	case statusInvalidOTP:
		return fmt.Errorf("%w: %s: the PIN was not correct", payment.ErrProviderRejected, op)
	case statusInsufficientFunds:
		return fmt.Errorf("%w: %s: insufficient balance on the mobile account", payment.ErrProviderRejected, op)
	case statusSubscriberBlocked:
		return fmt.Errorf("%w: %s: the mobile account cannot be charged", payment.ErrProviderRejected, op)
	case "":
		return fmt.Errorf("%w: %s: carrier returned no status", payment.ErrProviderUnavailable, op)
	}
	// E-codes are decisions; anything else is treated as a transport problem
	// and retried.
	if strings.HasPrefix(code, "E") {
		return fmt.Errorf("%w: %s: carrier status %s", payment.ErrProviderRejected, op, code)
	}
	return fmt.Errorf("%w: %s: carrier status %s", payment.ErrProviderUnavailable, op, code)
}

// carrierReason maps a status code to a message safe to show a patient. It
// deliberately does not pass the carrier's own detail string through, which can
// contain the subscriber's MSISDN.
func carrierReason(code string) string {
	switch code {
	case statusInsufficientFunds:
		return "insufficient balance on the mobile account"
	case statusInvalidOTP:
		return "the PIN was not correct"
	case statusSubscriberBlocked:
		return "this mobile account cannot be charged"
	default:
		return "the carrier declined the charge"
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
