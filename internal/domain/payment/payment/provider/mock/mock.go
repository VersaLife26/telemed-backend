// Package mockprovider is a credential-free payment rail for local development
// and tests.
//
// It is a real implementation of the interface, not a stub: it signs its own
// webhooks with HMAC-SHA256, enforces a replay tolerance window, honours
// idempotency keys, and returns the same normalised outcomes the real rails do.
// That matters, because a mock that skips verification lets the verification
// path rot untested, and the verification path is the one that stops an
// attacker marking appointments paid.
//
// It is registered only when PAYMENT_ENABLE_MOCK is set, and refuses to start
// in a production environment.
package mockprovider

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"telemed/internal/domain/payment/payment"
)

// Config configures the mock rail.
type Config struct {
	// WebhookSecret signs and verifies mock webhooks. A default is generated
	// when empty so `make run` works with no configuration at all.
	WebhookSecret string
	// Tolerance is the replay window, matching Stripe's five minutes.
	Tolerance time.Duration
	// AutoSucceed makes CreateIntent settle immediately, which is what a
	// developer wants when they are working on the booking flow and do not care
	// about payment. Off by default: the interesting default is the one that
	// exercises the webhook path.
	AutoSucceed bool
	// FeeBps applies a synthetic processing fee so the fee arithmetic is
	// exercised locally.
	FeeBps int
}

// Provider is the mock rail.
type Provider struct {
	cfg Config

	mu sync.Mutex
	// intents remembers what was created, keyed by idempotency key, so a
	// retried CreateIntent returns the same intent exactly as a real rail does.
	intents map[string]payment.IntentResult
	refunds map[string]payment.RefundResult
	payouts map[string]payment.PayoutResult

	// vaultOnce/vaultState hold the saved-card side. It is lazily built so a
	// deployment that never touches saved cards allocates nothing for them.
	vaultOnce  sync.Once
	vaultStore *vaultState
}

// vault returns the lazily built card vault.
func (p *Provider) vault() *vaultState {
	p.vaultOnce.Do(func() { p.vaultStore = newVaultState() })
	return p.vaultStore
}

var (
	_ payment.PaymentProvider = (*Provider)(nil)
	_ payment.PINProvider     = (*Provider)(nil)
)

// New builds the mock rail.
func New(cfg Config) *Provider {
	if cfg.WebhookSecret == "" {
		cfg.WebhookSecret = "whsec_mock_" + uuid.NewString()
	}
	if cfg.Tolerance <= 0 {
		cfg.Tolerance = 5 * time.Minute
	}
	return &Provider{
		cfg:     cfg,
		intents: map[string]payment.IntentResult{},
		refunds: map[string]payment.RefundResult{},
		payouts: map[string]payment.PayoutResult{},
	}
}

// Name identifies the rail.
func (p *Provider) Name() string { return string(payment.ProviderMock) }

// WebhookSecret exposes the signing secret so the local dev script and the
// tests can produce valid deliveries.
func (p *Provider) WebhookSecret() string { return p.cfg.WebhookSecret }

// CreateIntent records an intent, honouring the idempotency key.
func (p *Provider) CreateIntent(_ context.Context, req payment.IntentRequest) (payment.IntentResult, error) {
	if req.AmountCents <= 0 {
		return payment.IntentResult{}, fmt.Errorf("%w: amount must be positive", payment.ErrProviderRejected)
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if existing, ok := p.intents[req.IdempotencyKey]; ok {
		return existing, nil
	}
	res := payment.IntentResult{
		ProviderIntentID: "pi_mock_" + strings.ReplaceAll(req.PaymentID.String(), "-", "")[:24],
		ClientSecret:     "cs_mock_" + uuid.NewString(),
		Status:           payment.IntentRequiresAction,
	}
	if p.cfg.AutoSucceed {
		res.Status = payment.IntentSucceeded
		res.ProviderFeeCents = req.AmountCents * int64(p.cfg.FeeBps) / payment.BasisPointDenominator
	}
	p.intents[req.IdempotencyKey] = res
	return res, nil
}

// SendPIN pretends to send an SMS.
func (p *Provider) SendPIN(_ context.Context, req payment.IntentRequest) (payment.ProviderPINChallenge, error) {
	return payment.ProviderPINChallenge{
		Reference:    "ref_mock_" + uuid.NewString(),
		ExpiresAt:    time.Now().Add(5 * time.Minute).UTC(),
		MaskedMSISDN: "*******" + lastFour(req.PatientPhone),
	}, nil
}

// ConfirmPIN accepts the fixed development PIN and rejects everything else, so
// the failure branch is reachable without configuration.
func (p *Provider) ConfirmPIN(_ context.Context, req payment.PINConfirmation) (payment.IntentResult, error) {
	if strings.TrimSpace(req.PIN) != "123456" {
		return payment.IntentResult{}, fmt.Errorf("%w: the PIN was not correct", payment.ErrProviderRejected)
	}
	return payment.IntentResult{
		ProviderIntentID: "pi_mock_pin_" + uuid.NewString(),
		Reference:        req.Reference,
		Status:           payment.IntentSucceeded,
		ProviderFeeCents: req.AmountCents * int64(p.cfg.FeeBps) / payment.BasisPointDenominator,
	}, nil
}

// Refund records a refund, honouring the idempotency key.
func (p *Provider) Refund(_ context.Context, req payment.RefundRequest) (payment.RefundResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, ok := p.refunds[req.IdempotencyKey]; ok {
		return existing, nil
	}
	res := payment.RefundResult{
		ProviderRefundID: "re_mock_" + uuid.NewString(),
		Status:           payment.RefundSucceeded,
	}
	p.refunds[req.IdempotencyKey] = res
	return res, nil
}

// Capture simulates capturing previously held funds.
func (p *Provider) Capture(_ context.Context, req payment.CaptureRequest) (payment.CaptureResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fee := req.AmountCents * int64(p.cfg.FeeBps) / payment.BasisPointDenominator
	return payment.CaptureResult{
		ProviderPaymentID: "capt_mock_" + uuid.NewString(),
		Status:            "succeeded",
		ProviderFeeCents:  fee,
	}, nil
}

// Payout records a transfer, honouring the idempotency key. This is what makes
// the payout re-run test meaningful: a second call with the same key returns
// the first transfer instead of moving money again.
func (p *Provider) Payout(_ context.Context, req payment.PayoutRequest) (payment.PayoutResult, error) {
	if req.AmountCents <= 0 {
		return payment.PayoutResult{}, fmt.Errorf("%w: payout amount must be positive", payment.ErrProviderRejected)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, ok := p.payouts[req.IdempotencyKey]; ok {
		return existing, nil
	}
	res := payment.PayoutResult{
		TransferID: "tr_mock_" + uuid.NewString(),
		Status:     payment.PayoutPaid,
	}
	p.payouts[req.IdempotencyKey] = res
	return res, nil
}

// TransferCount reports how many distinct transfers were actually made. The
// payout re-run test asserts on this: a doctor paid twice shows up here as two.
func (p *Provider) TransferCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.payouts)
}

// MockEvent is the mock webhook body.
type MockEvent struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	ProviderIntentID string `json:"provider_intent_id,omitempty"`
	PaymentID        string `json:"payment_id,omitempty"`
	AppointmentID    string `json:"appointment_id,omitempty"`
	AmountCents      int64  `json:"amount_cents,omitempty"`
	Currency         string `json:"currency,omitempty"`
	FailureReason    string `json:"failure_reason,omitempty"`
	ProviderFeeCents int64  `json:"provider_fee_cents,omitempty"`
	ProviderRefundID string `json:"provider_refund_id,omitempty"`
}

// SignatureHeader is the header the mock rail signs into.
const SignatureHeader = "X-Mock-Signature"

// Sign produces the header value for a body at a given time. Its shape mirrors
// Stripe's -- "t=<unix>,v1=<hex hmac of t.body>" -- so the verification logic
// under test is the same shape as the one that runs in production.
func Sign(secret string, at time.Time, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(at.Unix(), 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", at.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

// VerifyWebhook authenticates a mock delivery.
func (p *Provider) VerifyWebhook(_ context.Context, headers http.Header, body []byte) (payment.WebhookEvent, error) {
	header := strings.TrimSpace(headers.Get(SignatureHeader))
	if header == "" {
		return payment.WebhookEvent{}, fmt.Errorf("%w: missing %s", payment.ErrSignatureInvalid, SignatureHeader)
	}

	var tsPart, sigPart string
	for _, kv := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			tsPart = v
		case "v1":
			sigPart = v
		}
	}
	if tsPart == "" || sigPart == "" {
		return payment.WebhookEvent{}, fmt.Errorf("%w: malformed %s", payment.ErrSignatureInvalid, SignatureHeader)
	}

	secs, err := strconv.ParseInt(tsPart, 10, 64)
	if err != nil {
		return payment.WebhookEvent{}, fmt.Errorf("%w: timestamp is not a unix time", payment.ErrSignatureInvalid)
	}

	mac := hmac.New(sha256.New, []byte(p.cfg.WebhookSecret))
	mac.Write([]byte(tsPart))
	mac.Write([]byte("."))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(expected), []byte(strings.ToLower(sigPart))) != 1 {
		return payment.WebhookEvent{}, fmt.Errorf("%w: signature mismatch", payment.ErrSignatureInvalid)
	}

	// The signature check passes before the age check on purpose: an attacker
	// should not be able to learn anything from the difference between "old"
	// and "forged".
	age := time.Since(time.Unix(secs, 0))
	if age < 0 {
		age = -age
	}
	if age > p.cfg.Tolerance {
		return payment.WebhookEvent{}, fmt.Errorf("%w: delivery is %s old", payment.ErrEventTooOld, age.Truncate(time.Second))
	}

	var ev MockEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return payment.WebhookEvent{}, fmt.Errorf("%w: unparseable JSON", payment.ErrWebhookMalformed)
	}
	if ev.ID == "" {
		return payment.WebhookEvent{}, fmt.Errorf("%w: no \"id\" field", payment.ErrWebhookMalformed)
	}

	out := payment.WebhookEvent{
		EventID:          ev.ID,
		Type:             ev.Type,
		ProviderIntentID: ev.ProviderIntentID,
		PaymentID:        ev.PaymentID,
		AppointmentID:    ev.AppointmentID,
		AmountCents:      ev.AmountCents,
		Currency:         ev.Currency,
		FailureReason:    ev.FailureReason,
		ProviderFeeCents: ev.ProviderFeeCents,
		ProviderRefundID: ev.ProviderRefundID,
		OccurredAt:       time.Unix(secs, 0).UTC(),
		Raw:              body,
	}
	switch ev.Type {
	case "payment.succeeded":
		out.Outcome = payment.OutcomePaymentSucceeded
	case "payment.failed":
		out.Outcome = payment.OutcomePaymentFailed
	case "payment.pending":
		out.Outcome = payment.OutcomePaymentPending
	case "refund.succeeded":
		out.Outcome = payment.OutcomeRefundSucceeded
	case "refund.failed":
		out.Outcome = payment.OutcomeRefundFailed
	default:
		out.Outcome = payment.OutcomeIgnored
	}
	return out, nil
}

// ErrProductionUse is returned when the mock is enabled in a prod environment.
var ErrProductionUse = errors.New("mock: the mock payment provider must never be enabled in production")

// Guard refuses to construct the mock in a production environment.
func Guard(env string) error {
	if env == "prod" || env == "production" {
		return ErrProductionUse
	}
	return nil
}

func lastFour(s string) string {
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}
