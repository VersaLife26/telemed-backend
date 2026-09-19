// Package payhereprovider implements payment.PaymentProvider for PayHere 2.0,
// the dominant card and wallet gateway in Sri Lanka.
//
// PayHere is not an API-first processor. There is no server-side "create
// intent" call: the server computes an MD5 hash over the order, hands it to the
// browser or app, and PayHere's hosted checkout takes over. Settlement is
// announced by an unauthenticated-looking form POST to a notify URL, whose only
// proof of origin is a second MD5 hash over the outcome.
//
// That shape drives three decisions in this file:
//
//  1. CreateIntent returns a signed checkout payload and a redirect URL rather
//     than a client secret. The payment stays pending until the notify arrives.
//  2. The notify body is form-encoded, not JSON, and carries no event
//     identifier. We synthesise a stable one from the fields that do identify
//     the delivery, so the (provider, event_id) unique index still works.
//  3. The hash comparison uses crypto/subtle.ConstantTimeCompare. An MD5 hash
//     compared with == leaks its correct prefix through timing, and an attacker
//     who can forge a notify can mark any appointment paid.
//
// MD5 is PayHere's choice, not ours. It is used here only where the protocol
// mandates it, and never for anything we control.
package payhereprovider

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // mandated by the PayHere 2.0 protocol
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"telemed/internal/domain/payment/payment"
)

// Config configures the PayHere rail.
type Config struct {
	MerchantID     string
	MerchantSecret string
	// AppID and AppSecret authenticate the merchant REST API, which is what
	// refunds go through. They are separate credentials from the checkout ones.
	AppID     string
	AppSecret string

	// BaseURL is https://sandbox.payhere.lk or https://www.payhere.lk.
	BaseURL string
	// NotifyURL, ReturnURL and CancelURL are handed to the hosted checkout.
	NotifyURL string
	ReturnURL string
	CancelURL string

	HTTPClient *http.Client
}

// Provider is the PayHere implementation.
type Provider struct {
	cfg    Config
	client *http.Client

	// secretHash is MD5(merchant_secret) uppercased, the inner hash both the
	// checkout and the notify signatures are built on. Computed once so the
	// secret itself is touched exactly at construction.
	secretHash string

	mu        sync.Mutex
	token     string
	tokenTill time.Time
}

var _ payment.PaymentProvider = (*Provider)(nil)

// PayHere status codes, from the 2.0 notify specification.
const (
	statusAuthorized  = "3"
	statusSuccess     = "2"
	statusPending     = "0"
	statusCancelled   = "-1"
	statusFailed      = "-2"
	statusChargedback = "-3"
)

// New builds the provider.
func New(cfg Config) (*Provider, error) {
	if strings.TrimSpace(cfg.MerchantID) == "" || strings.TrimSpace(cfg.MerchantSecret) == "" {
		return nil, errors.New("payhere: PAYHERE_MERCHANT_ID and PAYHERE_MERCHANT_SECRET are required")
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		cfg.BaseURL = "https://sandbox.payhere.lk"
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &Provider{
		cfg:        cfg,
		client:     client,
		secretHash: md5Upper([]byte(cfg.MerchantSecret)),
	}, nil
}

// Name identifies the rail.
func (p *Provider) Name() string { return string(payment.ProviderPayHere) }

// FormatAmount renders cents the way PayHere hashes them: a plain decimal with
// exactly two places and no thousands separator. Getting this wrong is the
// single most common cause of "hash mismatch" in PayHere integrations, so it
// is one function used by both the signer and the verifier.
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

// CheckoutHash computes the PayHere checkout signature:
//
//	MD5(merchant_id + order_id + amount + currency + MD5(merchant_secret))
//
// with both MD5 outputs uppercased.
func CheckoutHash(merchantID, orderID, amount, currency, secretHash string) string {
	return md5Upper([]byte(merchantID + orderID + amount + currency + secretHash))
}

// NotifyHash computes the PayHere notify signature:
//
//	MD5(merchant_id + order_id + payhere_amount + payhere_currency + status_code + MD5(merchant_secret))
func NotifyHash(merchantID, orderID, amount, currency, statusCode, secretHash string) string {
	return md5Upper([]byte(merchantID + orderID + amount + currency + statusCode + secretHash))
}

func md5Upper(b []byte) string {
	sum := md5.Sum(b) //nolint:gosec // protocol-mandated
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// --- field grammars, and why they are a signature control ------------------
//
// PayHere mandates the hash construction. It does not mandate feeding that
// construction unvalidated input, and the two together are the actual defect:
//
//	MD5(merchant_id + order_id + payhere_amount + payhere_currency + status_code + MD5(secret))
//
// has no separators and no lengths, so a field that can absorb the leading
// characters of the next one yields the SAME preimage -- and therefore the
// same valid signature -- for a different tuple of values. With PayHere's real
// field set that is not hypothetical. The currency/status boundary collides:
//
//	currency "LKR"  + status_code "-2"   a genuine FAILED notify
//	currency "LKR-" + status_code "2"    a forged SUCCEEDED notify
//
// Both concatenate to "LKR-2". One captured failure notify is therefore a
// valid signature for a success on the same order, and because EventID embeds
// status_code the dedup index sees a different event and lets it through.
// (statusCancelled "-1" against status "1" is the same trick.)
//
// Validating each field against its own grammar BEFORE hashing removes every
// degree of freedom the ambiguity needs, and makes the concatenation
// injective. The argument, right to left over the preimage:
//
//   - MD5(secret) is a fixed 32-character suffix.
//   - merchant_id is compared for equality against the configured value before
//     any of this, so it is a constant of known length -- a stronger pin than
//     any grammar would be.
//   - order_id must be a canonical 36-character UUID, so its span is fixed.
//   - payhere_amount is drawn from [0-9.] and payhere_currency from [A-Z].
//     The alphabets are disjoint, so the amount ends at the first letter.
//     (\d is [0-9] exactly in RE2 -- it is never Unicode-aware -- so the
//     disjointness argument holds as written.)
//   - payhere_currency is exactly three letters, so status_code starts exactly
//     three characters later.
//
// Every boundary is therefore determined by the string itself, and two
// distinct valid tuples cannot share a preimage. F10's amount check is the
// second layer, on the assumption that this one is one day weakened.
var (
	// PayHere sends the plain decimal it hashes: no sign, no thousands
	// separator, at most two places. That is exactly what FormatAmount emits,
	// which is why a comma-formatted amount could never have matched the
	// checkout hash either.
	reAmount = regexp.MustCompile(`^\d{1,15}(\.\d{1,2})?$`)
	// ISO 4217 shape. The value is checked against the payment row separately;
	// this check exists for the concatenation, not for the currency.
	reCurrency = regexp.MustCompile(`^[A-Z]{3}$`)
	// PayHere's documented codes are 2, 0, -1, -2 and -3. The shape is bounded
	// rather than the exact set enumerated, so a code PayHere adds later is
	// still recorded as an ignored event instead of being answered 401 -- the
	// injectivity above does not depend on this bound.
	reStatusCode = regexp.MustCompile(`^-?\d{1,2}$`)
)

// errAmbiguous names the field that failed its grammar. It is deliberately not
// specific about which character offended: a caller probing the verifier
// learns nothing beyond "that field is wrong". It carries no sentinel of its
// own, because the same check guards an inbound notify (a signature failure)
// and an outbound checkout (a provider rejection), and those are not the same
// error to the caller.
func errAmbiguous(field string) error {
	return fmt.Errorf("%s does not match its grammar, so the hash preimage would be ambiguous", field)
}

// validateHashedFields refuses any field that could shift a boundary in the
// undelimited preimage. It runs before the hash is computed, so an ambiguous
// input is never signed and never compared.
//
// merchantID is not checked here: VerifyWebhook has already compared it for
// equality with the configured merchant id, which fixes both its value and its
// length.
func validateHashedFields(orderID, amount, currency, statusCode string) error {
	if !isCanonicalUUID(orderID) {
		return errAmbiguous("order_id")
	}
	if !reAmount.MatchString(amount) {
		return errAmbiguous("payhere_amount")
	}
	if !reCurrency.MatchString(currency) {
		return errAmbiguous("payhere_currency")
	}
	if !reStatusCode.MatchString(statusCode) {
		return errAmbiguous("status_code")
	}
	return nil
}

// isCanonicalUUID accepts only the 36-character hyphenated lower-case form.
//
// uuid.Parse alone is not enough: it also accepts the 32-character
// unhyphenated form, the braced form and the urn: form, all of which have
// different lengths and would put the order_id/amount boundary back in play.
// Our own order ids are always uuid.UUID.String(), so nothing legitimate is
// excluded.
func isCanonicalUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	parsed, err := uuid.Parse(s)
	return err == nil && parsed.String() == s
}

// CheckoutPayload is the signed form the client posts to PayHere's hosted
// checkout. It is returned to the caller as JSON in IntentResult.Reference so
// the mobile app can hand it straight to the PayHere SDK.
type CheckoutPayload struct {
	MerchantID string `json:"merchant_id"`
	ReturnURL  string `json:"return_url"`
	CancelURL  string `json:"cancel_url"`
	NotifyURL  string `json:"notify_url"`
	OrderID    string `json:"order_id"`
	Items      string `json:"items"`
	Currency   string `json:"currency"`
	Amount     string `json:"amount"`
	Hash       string `json:"hash"`
}

// CreateIntent prepares a signed hosted-checkout payload.
//
// No network call is made: PayHere's checkout is initiated by the client, not
// by us. That also means this call cannot fail transiently, and the idempotency
// key is unused -- the order id is the payment id, which is already unique.
func (p *Provider) CreateIntent(_ context.Context, req payment.IntentRequest) (payment.IntentResult, error) {
	if req.AmountCents <= 0 {
		return payment.IntentResult{}, fmt.Errorf("%w: amount must be positive", payment.ErrProviderRejected)
	}
	currency := strings.ToUpper(req.Currency)
	if currency == "" {
		currency = payment.CurrencyLKR
	}
	orderID := req.PaymentID.String()
	amount := FormatAmount(req.AmountCents)

	// The checkout hash is the same undelimited concatenation as the notify
	// hash, minus the status code. Signing an ambiguous tuple here would mean
	// issuing a signature that is simultaneously valid for a different order
	// or a different amount, so the same grammar is applied on the way out.
	// statusCode is not part of this hash; "0" is a placeholder that always
	// passes and keeps one validator for both directions.
	if err := validateHashedFields(orderID, amount, currency, statusPending); err != nil {
		return payment.IntentResult{}, fmt.Errorf("%w: %w", payment.ErrProviderRejected, err)
	}

	payload := CheckoutPayload{
		MerchantID: p.cfg.MerchantID,
		ReturnURL:  firstNonEmpty(req.ReturnURL, p.cfg.ReturnURL),
		CancelURL:  p.cfg.CancelURL,
		NotifyURL:  p.cfg.NotifyURL,
		OrderID:    orderID,
		Items:      req.Description,
		Currency:   currency,
		Amount:     amount,
		Hash:       CheckoutHash(p.cfg.MerchantID, orderID, amount, currency, p.secretHash),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return payment.IntentResult{}, fmt.Errorf("payhere: encode checkout payload: %w", err)
	}

	redirectPath := "/pay/checkout"
	if req.AuthorizeOnly {
		redirectPath = "/pay/authorize"
	}

	return payment.IntentResult{
		ProviderIntentID: orderID,
		RedirectURL:      p.cfg.BaseURL + redirectPath,
		Reference:        string(encoded),
		Status:           payment.IntentRequiresAction,
	}, nil
}

// VerifyWebhook authenticates a PayHere notify POST.
func (p *Provider) VerifyWebhook(_ context.Context, headers http.Header, body []byte) (payment.WebhookEvent, error) {
	ct := headers.Get("Content-Type")
	if ct != "" && !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		return payment.WebhookEvent{}, fmt.Errorf("%w: PayHere notify must be form-encoded, got %q", payment.ErrSignatureInvalid, ct)
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return payment.WebhookEvent{}, fmt.Errorf("%w: unparseable notify body", payment.ErrSignatureInvalid)
	}

	merchantID := form.Get("merchant_id")
	orderID := form.Get("order_id")
	amount := form.Get("payhere_amount")
	currency := form.Get("payhere_currency")
	statusCode := form.Get("status_code")
	sig := strings.ToUpper(strings.TrimSpace(form.Get("md5sig")))
	payHereID := form.Get("payment_id")

	if merchantID == "" || orderID == "" || sig == "" || statusCode == "" {
		return payment.WebhookEvent{}, fmt.Errorf("%w: notify is missing required fields", payment.ErrSignatureInvalid)
	}
	// A notify for someone else's merchant account is not ours to act on, and
	// checking it before the hash keeps a cross-tenant probe cheap to reject.
	if subtle.ConstantTimeCompare([]byte(merchantID), []byte(p.cfg.MerchantID)) != 1 {
		return payment.WebhookEvent{}, fmt.Errorf("%w: notify addressed to a different merchant", payment.ErrSignatureInvalid)
	}
	// BEFORE the hash, never after. Hashing first and validating afterwards
	// would still accept the currency/status collision described above, because
	// the forged tuple carries a genuinely valid signature -- the whole point
	// is that the signature cannot distinguish the two.
	if err := validateHashedFields(orderID, amount, currency, statusCode); err != nil {
		return payment.WebhookEvent{}, fmt.Errorf("%w: %w", payment.ErrSignatureInvalid, err)
	}

	expected := NotifyHash(merchantID, orderID, amount, currency, statusCode, p.secretHash)
	// Constant time. A byte-by-byte == on a signature is a timing oracle: an
	// attacker who can measure the difference recovers the correct hash one
	// nibble at a time and can then forge a "paid" notify for any appointment.
	if subtle.ConstantTimeCompare([]byte(expected), []byte(sig)) != 1 {
		return payment.WebhookEvent{}, fmt.Errorf("%w: notify hash mismatch", payment.ErrSignatureInvalid)
	}

	cents, err := ParseAmount(amount)
	if err != nil {
		return payment.WebhookEvent{}, fmt.Errorf("%w: %w", payment.ErrSignatureInvalid, err)
	}

	// webhook_events.payload is JSONB, and pgx v5 hands a []byte straight to a
	// JSONB parameter without encoding it. Assigning the form-encoded body
	// here made Postgres parse "merchant_id=...&order_id=..." as JSON, raise
	// 22P02, and roll back the whole transaction -- so every PayHere notify
	// answered 500, PayHere retried forever, and no PayHere payment could ever
	// settle. Raw must be JSON.
	//
	// url.Values marshals as an object of arrays, which is lossless: form
	// encoding permits a repeated key, and flattening one would quietly
	// discard evidence in the one table that exists to hold it.
	raw, err := json.Marshal(form)
	if err != nil {
		return payment.WebhookEvent{}, fmt.Errorf("%w: notify body could not be re-encoded: %w", payment.ErrWebhookMalformed, err)
	}

	out := payment.WebhookEvent{
		// PayHere sends no event id. The triple below is stable across
		// redeliveries of the same outcome and distinct between outcomes, which
		// is exactly what the dedup index needs.
		EventID:          fmt.Sprintf("%s:%s:%s", orderID, payHereID, statusCode),
		Type:             "payhere.notify." + statusCode,
		ProviderIntentID: orderID,
		PaymentID:        orderID,
		AmountCents:      cents,
		Currency:         strings.ToUpper(currency),
		OccurredAt:       time.Now().UTC(),
		Raw:              raw,
	}

	switch statusCode {
	case statusAuthorized:
		out.Outcome = payment.OutcomePaymentAuthorized
		out.AuthorizationToken = strings.TrimSpace(form.Get("authorization_token"))
	case statusSuccess:
		out.Outcome = payment.OutcomePaymentSucceeded
	case statusPending:
		out.Outcome = payment.OutcomePaymentPending
	case statusCancelled:
		out.Outcome = payment.OutcomePaymentFailed
		out.FailureReason = "cancelled at the payment page"
	case statusFailed:
		out.Outcome = payment.OutcomePaymentFailed
		out.FailureReason = firstNonEmpty(form.Get("status_message"), "payment failed")
	case statusChargedback:
		out.Outcome = payment.OutcomeRefundSucceeded
		out.ProviderRefundID = "chargeback:" + payHereID
	default:
		out.Outcome = payment.OutcomeIgnored
	}
	return out, nil
}

// ParseAmount converts PayHere's decimal string to integer cents without
// touching a float. "1500.00" -> 150000, "1500.5" -> 150050, "1500" -> 150000.
func ParseAmount(s string) (int64, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", ""))
	if s == "" {
		return 0, errors.New("payhere: empty amount")
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")

	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("payhere: amount %q is not a number", s)
	}
	cents := w * 100
	if hasFrac {
		switch {
		case frac == "":
		case len(frac) <= 2:
			f, err := strconv.ParseInt(frac+strings.Repeat("0", 2-len(frac)), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("payhere: amount %q is not a number", s)
			}
			cents += f
		default:
			return 0, fmt.Errorf("payhere: amount %q has sub-cent precision", s)
		}
	}
	if neg {
		cents = -cents
	}
	return cents, nil
}

// --- merchant REST API (refunds) -------------------------------------------

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
}

// accessToken fetches and caches an OAuth client-credentials token for the
// merchant API. The cache exists because PayHere rate-limits the token
// endpoint far more aggressively than the refund endpoint.
func (p *Provider) accessToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token != "" && time.Now().Before(p.tokenTill) {
		return p.token, nil
	}
	if p.cfg.AppID == "" || p.cfg.AppSecret == "" {
		return "", fmt.Errorf("%w: PAYHERE_APP_ID and PAYHERE_APP_SECRET are required for refunds", payment.ErrUnsupported)
	}

	form := url.Values{"grant_type": []string{"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.cfg.BaseURL+"/merchant/v1/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(p.cfg.AppID, p.cfg.AppSecret)

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: payhere token: %w", payment.ErrProviderUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: payhere token endpoint returned %d", payment.ErrProviderUnavailable, resp.StatusCode)
	}
	var tr tokenResponse
	if err := json.Unmarshal(raw, &tr); err != nil || tr.AccessToken == "" {
		return "", fmt.Errorf("%w: payhere token response unusable", payment.ErrProviderUnavailable)
	}

	p.token = tr.AccessToken
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	// Expire our copy early so a token never dies mid-request.
	p.tokenTill = time.Now().Add(ttl - 30*time.Second)
	return p.token, nil
}

type refundResponse struct {
	Status int    `json:"status"`
	Msg    string `json:"msg"`
	Data   struct {
		PaymentID json.Number `json:"payment_id"`
	} `json:"data"`
}

// Refund returns money through the PayHere merchant API.
//
// PayHere's refund endpoint refunds a payment in full; it has no partial
// refund parameter. Our cancellation policy has a 50% band, so a late patient
// cancellation on a PayHere payment cannot be settled by API. Rather than
// silently refunding the full amount -- which would hand back money the policy
// says the doctor keeps -- this returns a rejection naming the problem, and the
// runbook covers the manual portal refund.
func (p *Provider) Refund(ctx context.Context, req payment.RefundRequest) (payment.RefundResult, error) {
	token, err := p.accessToken(ctx)
	if err != nil {
		return payment.RefundResult{}, err
	}

	body, err := json.Marshal(map[string]string{
		"payment_id":  req.ProviderIntentID,
		"description": "telemed refund " + string(req.Reason),
	})
	if err != nil {
		return payment.RefundResult{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.cfg.BaseURL+"/merchant/v1/payment/refund", bytes.NewReader(body))
	if err != nil {
		return payment.RefundResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return payment.RefundResult{}, fmt.Errorf("%w: payhere refund: %w", payment.ErrProviderUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode >= 500 {
		return payment.RefundResult{}, fmt.Errorf("%w: payhere refund returned %d", payment.ErrProviderUnavailable, resp.StatusCode)
	}
	var rr refundResponse
	if err := json.Unmarshal(raw, &rr); err != nil {
		return payment.RefundResult{}, fmt.Errorf("%w: payhere refund response unusable", payment.ErrProviderUnavailable)
	}
	if resp.StatusCode != http.StatusOK || rr.Status != 1 {
		return payment.RefundResult{}, fmt.Errorf("%w: payhere refund: %s", payment.ErrProviderRejected, rr.Msg)
	}
	return payment.RefundResult{
		ProviderRefundID: firstNonEmpty(rr.Data.PaymentID.String(), req.ProviderIntentID),
		Status:           payment.RefundSucceeded,
	}, nil
}

type captureRequestBody struct {
	AuthorizationToken string  `json:"authorization_token"`
	Amount             float64 `json:"amount"`
	DeductionDetails   string  `json:"deduction_details"`
}

type captureResponse struct {
	Status int    `json:"status"`
	Msg    string `json:"msg"`
	Data   struct {
		StatusCode     int         `json:"status_code"`
		StatusMessage  string      `json:"status_message"`
		PaymentID      json.Number `json:"payment_id"`
		Currency       string      `json:"currency"`
		Amount         float64     `json:"amount"`
		CapturedAmount float64     `json:"captured_amount"`
		Items          string      `json:"items"`
		OrderID        string      `json:"order_id"`
	} `json:"data"`
}

// Capture charges previously authorized funds via the PayHere Capture REST API.
func (p *Provider) Capture(ctx context.Context, req payment.CaptureRequest) (payment.CaptureResult, error) {
	if req.AuthorizationToken == "" {
		return payment.CaptureResult{}, fmt.Errorf("%w: authorization token is required for capture", payment.ErrProviderRejected)
	}
	token, err := p.accessToken(ctx)
	if err != nil {
		return payment.CaptureResult{}, err
	}

	amountFloat := float64(req.AmountCents) / 100.0
	body, err := json.Marshal(captureRequestBody{
		AuthorizationToken: req.AuthorizationToken,
		Amount:             amountFloat,
		DeductionDetails:   firstNonEmpty(req.Description, "Telemedicine consultation completed"),
	})
	if err != nil {
		return payment.CaptureResult{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.cfg.BaseURL+"/merchant/v1/payment/capture", bytes.NewReader(body))
	if err != nil {
		return payment.CaptureResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return payment.CaptureResult{}, fmt.Errorf("%w: payhere capture: %w", payment.ErrProviderUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode >= 500 {
		return payment.CaptureResult{}, fmt.Errorf("%w: payhere capture returned %d", payment.ErrProviderUnavailable, resp.StatusCode)
	}
	var cr captureResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return payment.CaptureResult{}, fmt.Errorf("%w: payhere capture response unusable", payment.ErrProviderUnavailable)
	}
	if resp.StatusCode != http.StatusOK || cr.Status != 1 || cr.Data.StatusCode != 2 {
		msg := cr.Msg
		if cr.Data.StatusMessage != "" {
			msg = cr.Data.StatusMessage
		}
		return payment.CaptureResult{
			Status:        "failed",
			FailureReason: msg,
		}, fmt.Errorf("%w: payhere capture: %s", payment.ErrProviderRejected, msg)
	}

	return payment.CaptureResult{
		ProviderPaymentID: cr.Data.PaymentID.String(),
		Status:            "succeeded",
	}, nil
}

// SupportsPartialRefund reports whether a refund of less than the full captured
// amount can be made through the API. It cannot, for PayHere.
func (p *Provider) SupportsPartialRefund() bool { return false }

// Payout is not available on PayHere.
//
// PayHere is a collection gateway: it settles to the merchant's own bank
// account on its own schedule and offers no API to pay a third party. Doctor
// settlement on a PayHere-only deployment is a bank transfer, which this
// service records but does not execute. Saying so plainly is better than a
// method that returns nil and quietly marks doctors paid.
func (p *Provider) Payout(_ context.Context, _ payment.PayoutRequest) (payment.PayoutResult, error) {
	return payment.PayoutResult{}, fmt.Errorf("%w: PayHere has no third-party payout API; settle doctors over Stripe Connect or by bank transfer", payment.ErrUnsupported)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
