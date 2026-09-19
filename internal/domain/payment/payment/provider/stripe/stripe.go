// Package stripeprovider implements payment.PaymentProvider on top of
// stripe-go v86.
//
// It is the only package in this repository that imports stripe-go. Everything
// above it -- the service, the payout job, the handlers -- speaks the
// PaymentProvider interface and would not notice if this file were replaced by
// an Adyen adapter tomorrow.
//
// # Notes on v86 versus the v76 the platform documentation names
//
// The documentation was written against stripe-go v76 and its examples do not
// compile against v86. The differences that matter:
//
//   - Package-level entrypoints (paymentintent.New, refund.New) and the
//     client.API aggregate are deprecated. The current shape is
//     stripe.NewClient(key) returning a *stripe.Client whose services hang off
//     it as V1PaymentIntents, V1Refunds, V1Transfers.
//   - Every call takes a context.Context as its first argument. In v76 the
//     context travelled inside Params.Context.
//   - Params structs are split per operation: PaymentIntentCreateParams rather
//     than one PaymentIntentParams shared by create, update and confirm.
//   - Webhook verification moved from the webhook subpackage's
//     ConstructEvent(payload, header, secret) to the root package's
//     stripe.ConstructEvent(payload, header, secret, opts ...WebhookOption),
//     with the tolerance expressed as an option rather than a separate
//     function.
//   - stripe.String is generic over ~string, so a typed constant such as
//     stripe.CurrencyLKR can be passed without a conversion.
//   - ConstructEvent now rejects an event whose API version is from a
//     different release train than the SDK's pinned stripe.APIVersion.
package stripeprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	stripe "github.com/stripe/stripe-go/v86"

	"telemed/internal/domain/payment/payment"
)

// Config configures the Stripe rail.
type Config struct {
	// SecretKey is the restricted API key. It is never logged.
	SecretKey string
	// WebhookSecret is the endpoint signing secret (whsec_...).
	WebhookSecret string
	// Tolerance is the replay window for webhook timestamps. Stripe's default
	// is five minutes; anything older is treated as a replay and rejected.
	Tolerance time.Duration
	// IgnoreAPIVersionMismatch relaxes stripe-go's insistence that the event's
	// API version share a release train with the SDK.
	//
	// Default true, deliberately. Our webhook handling reads a handful of
	// scalar fields out of the raw JSON with our own structs rather than
	// deserialising Stripe's object graph, so a version skew is harmless to
	// us -- while refusing every webhook because the dashboard endpoint is
	// pinned to last year's version is an outage. Set false once the account
	// and the SDK are known to agree.
	IgnoreAPIVersionMismatch bool
}

// Provider is the Stripe implementation of payment.PaymentProvider.
type Provider struct {
	client    *stripe.Client
	cfg       Config
	tolerance time.Duration
}

var (
	_ payment.PaymentProvider = (*Provider)(nil)
)

// New builds the provider. It fails fast on a missing key: a service that
// starts without a payment credential only discovers the problem when a
// patient tries to pay.
func New(cfg Config) (*Provider, error) {
	if strings.TrimSpace(cfg.SecretKey) == "" {
		return nil, errors.New("stripe: STRIPE_SECRET_KEY is required")
	}
	if strings.TrimSpace(cfg.WebhookSecret) == "" {
		return nil, errors.New("stripe: STRIPE_WEBHOOK_SECRET is required; unverified webhooks are not accepted")
	}
	tol := cfg.Tolerance
	if tol <= 0 {
		tol = stripe.WebhookDefaultTolerance
	}
	return &Provider{
		client:    stripe.NewClient(cfg.SecretKey),
		cfg:       cfg,
		tolerance: tol,
	}, nil
}

// NewWithClient injects a pre-built client. Used by tests that drive a stub
// backend, and by any future deployment that needs a custom HTTP transport.
func NewWithClient(client *stripe.Client, cfg Config) *Provider {
	tol := cfg.Tolerance
	if tol <= 0 {
		tol = stripe.WebhookDefaultTolerance
	}
	return &Provider{client: client, cfg: cfg, tolerance: tol}
}

// Name identifies the rail.
func (p *Provider) Name() string { return string(payment.ProviderStripe) }

// CreateIntent creates a PaymentIntent.
//
// Two things make this safe to retry: the idempotency key, which is derived
// from our payment id and is therefore stable across every retry of the same
// logical request, and the appointment_id metadata, which lets a webhook be
// correlated back to a payment even if the intent id never reached our
// database because the response was lost.
func (p *Provider) CreateIntent(ctx context.Context, req payment.IntentRequest) (payment.IntentResult, error) {
	params := &stripe.PaymentIntentCreateParams{
		Amount:      stripe.Int64(req.AmountCents),
		Currency:    stripe.String(currencyOf(req.Currency)),
		Description: stripe.String(req.Description),
		Metadata: map[string]string{
			"payment_id":     req.PaymentID.String(),
			"appointment_id": req.AppointmentID.String(),
			"patient_id":     req.PatientID.String(),
			"doctor_id":      req.DoctorID.String(),
		},
		AutomaticPaymentMethods: &stripe.PaymentIntentCreateAutomaticPaymentMethodsParams{
			Enabled: stripe.Bool(true),
		},
	}
	// In v86 the idempotency key still lives on the embedded Params struct and
	// is sent as a header, not a form field.
	params.IdempotencyKey = stripe.String(req.IdempotencyKey)

	pi, err := p.client.V1PaymentIntents.Create(ctx, params)
	if err != nil {
		return payment.IntentResult{}, classify(err, "create payment intent")
	}

	return payment.IntentResult{
		ProviderIntentID: pi.ID,
		ClientSecret:     pi.ClientSecret,
		Status:           mapIntentStatus(pi.Status),
	}, nil
}

// VerifyWebhook authenticates a Stripe callback.
//
// The tolerance window is the replay defence: Stripe signs the timestamp along
// with the body, so an attacker who captures a valid delivery cannot replay it
// once the window has passed, even though the signature itself stays valid
// forever. Rejecting on age is not optional.
func (p *Provider) VerifyWebhook(_ context.Context, headers http.Header, body []byte) (payment.WebhookEvent, error) {
	sig := headers.Get("Stripe-Signature")
	if sig == "" {
		return payment.WebhookEvent{}, fmt.Errorf("%w: missing Stripe-Signature header", payment.ErrSignatureInvalid)
	}

	opts := []stripe.WebhookOption{stripe.WithTolerance(p.tolerance)}
	if p.cfg.IgnoreAPIVersionMismatch {
		opts = append(opts, stripe.WithIgnoreAPIVersionMismatch())
	}

	evt, err := stripe.ConstructEvent(body, sig, p.cfg.WebhookSecret, opts...)
	if err != nil {
		switch {
		case errors.Is(err, stripe.ErrWebhookTooOld):
			return payment.WebhookEvent{}, fmt.Errorf("%w: %w", payment.ErrEventTooOld, err)
		case errors.Is(err, stripe.ErrWebhookNotSigned),
			errors.Is(err, stripe.ErrWebhookInvalidHeader),
			errors.Is(err, stripe.ErrWebhookNoValidSignature):
			return payment.WebhookEvent{}, fmt.Errorf("%w: %w", payment.ErrSignatureInvalid, err)
		default:
			// Anything else -- an unparseable body, an API version the SDK
			// refuses -- is also a reason not to act on the event.
			return payment.WebhookEvent{}, fmt.Errorf("%w: %w", payment.ErrSignatureInvalid, err)
		}
	}

	out := payment.WebhookEvent{
		EventID:    evt.ID,
		Type:       string(evt.Type),
		OccurredAt: time.Unix(evt.Created, 0).UTC(),
		Raw:        body,
		Outcome:    payment.OutcomeIgnored,
	}

	switch evt.Type {
	case stripe.EventTypePaymentIntentSucceeded:
		out.Outcome = payment.OutcomePaymentSucceeded
		hydrateIntent(&out, evt.Data)
	case stripe.EventTypePaymentIntentPaymentFailed:
		out.Outcome = payment.OutcomePaymentFailed
		hydrateIntent(&out, evt.Data)
	case stripe.EventTypePaymentIntentProcessing, stripe.EventTypePaymentIntentRequiresAction:
		out.Outcome = payment.OutcomePaymentPending
		hydrateIntent(&out, evt.Data)
	case stripe.EventTypeSetupIntentSucceeded:
		out.Outcome = payment.OutcomeSetupSucceeded
		hydrateSetupIntent(&out, evt.Data)
	case stripe.EventTypeChargeRefunded:
		out.Outcome = payment.OutcomeRefundSucceeded
		hydrateCharge(&out, evt.Data)
	case stripe.EventTypeRefundUpdated, stripe.EventTypeChargeRefundUpdated:
		hydrateRefund(&out, evt.Data)
	}
	return out, nil
}

// The webhook payload is read through these narrow structs rather than
// stripe-go's full object graph. It keeps a Stripe schema change from breaking
// deserialisation of the four fields we actually use, and it is why an API
// version skew is survivable.
type intentPayload struct {
	ID       string            `json:"id"`
	Amount   int64             `json:"amount"`
	Currency string            `json:"currency"`
	Status   string            `json:"status"`
	Metadata map[string]string `json:"metadata"`
	LastErr  *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"last_payment_error"`
}

type chargePayload struct {
	ID            string            `json:"id"`
	PaymentIntent string            `json:"payment_intent"`
	Amount        int64             `json:"amount"`
	AmountRefund  int64             `json:"amount_refunded"`
	Currency      string            `json:"currency"`
	Metadata      map[string]string `json:"metadata"`
	Refunds       *struct {
		Data []struct {
			ID     string `json:"id"`
			Amount int64  `json:"amount"`
			Status string `json:"status"`
		} `json:"data"`
	} `json:"refunds"`
}

type refundPayload struct {
	ID            string `json:"id"`
	PaymentIntent string `json:"payment_intent"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	Status        string `json:"status"`
	FailureReason string `json:"failure_reason"`
}

func hydrateIntent(out *payment.WebhookEvent, data *stripe.EventData) {
	if data == nil {
		return
	}
	var pi intentPayload
	if err := json.Unmarshal(data.Raw, &pi); err != nil {
		return
	}
	out.ProviderIntentID = pi.ID
	out.AmountCents = pi.Amount
	out.Currency = strings.ToUpper(pi.Currency)
	out.AppointmentID = pi.Metadata["appointment_id"]
	out.PaymentID = pi.Metadata["payment_id"]
	if pi.LastErr != nil {
		// The message is Stripe's, written for a cardholder; it carries no PHI
		// and is safe to surface to the patient.
		out.FailureReason = pi.LastErr.Message
	}
}

// hydrateSetupIntent pulls only the setup intent's id off a
// setup_intent.succeeded delivery.
//
// Deliberately nothing else: the payment method id and the owning patient are
// read back from the API afterwards rather than trusted from the webhook body,
// because a webhook payload is a snapshot that can be stale and the vault's
// ownership check has to be made against the current object.
func hydrateSetupIntent(out *payment.WebhookEvent, data *stripe.EventData) {
	if data == nil {
		return
	}
	var si struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data.Raw, &si); err != nil {
		return
	}
	out.ProviderIntentID = si.ID
}

func hydrateCharge(out *payment.WebhookEvent, data *stripe.EventData) {
	if data == nil {
		return
	}
	var ch chargePayload
	if err := json.Unmarshal(data.Raw, &ch); err != nil {
		return
	}
	out.ProviderIntentID = ch.PaymentIntent
	out.AmountCents = ch.AmountRefund
	out.Currency = strings.ToUpper(ch.Currency)
	out.AppointmentID = ch.Metadata["appointment_id"]
	out.PaymentID = ch.Metadata["payment_id"]
	if ch.Refunds != nil && len(ch.Refunds.Data) > 0 {
		last := ch.Refunds.Data[len(ch.Refunds.Data)-1]
		out.ProviderRefundID = last.ID
	}
}

func hydrateRefund(out *payment.WebhookEvent, data *stripe.EventData) {
	if data == nil {
		return
	}
	var rf refundPayload
	if err := json.Unmarshal(data.Raw, &rf); err != nil {
		return
	}
	out.ProviderRefundID = rf.ID
	out.ProviderIntentID = rf.PaymentIntent
	out.AmountCents = rf.Amount
	out.Currency = strings.ToUpper(rf.Currency)
	out.FailureReason = rf.FailureReason
	switch rf.Status {
	case "succeeded":
		out.Outcome = payment.OutcomeRefundSucceeded
	case "failed", "canceled":
		out.Outcome = payment.OutcomeRefundFailed
	default:
		out.Outcome = payment.OutcomeIgnored
	}
}

// Refund returns money against a PaymentIntent.
func (p *Provider) Refund(ctx context.Context, req payment.RefundRequest) (payment.RefundResult, error) {
	if req.ProviderIntentID == "" {
		return payment.RefundResult{}, fmt.Errorf("%w: no Stripe intent recorded for payment %s",
			payment.ErrProviderRejected, req.PaymentID)
	}
	params := &stripe.RefundCreateParams{
		PaymentIntent: stripe.String(req.ProviderIntentID),
		Amount:        stripe.Int64(req.AmountCents),
		Reason:        stripe.String(mapRefundReason(req.Reason)),
		Metadata: map[string]string{
			"payment_id": req.PaymentID.String(),
			"refund_id":  req.RefundID.String(),
			"reason":     string(req.Reason),
		},
	}
	params.IdempotencyKey = stripe.String(req.IdempotencyKey)

	rf, err := p.client.V1Refunds.Create(ctx, params)
	if err != nil {
		return payment.RefundResult{}, classify(err, "create refund")
	}
	return payment.RefundResult{
		ProviderRefundID: rf.ID,
		Status:           mapRefundStatus(rf.Status),
		FailureReason:    string(rf.FailureReason),
	}, nil
}

// Capture charges previously authorized funds on Stripe.
func (p *Provider) Capture(ctx context.Context, req payment.CaptureRequest) (payment.CaptureResult, error) {
	if req.ProviderIntentID == "" {
		return payment.CaptureResult{}, fmt.Errorf("%w: no Stripe intent recorded for capture", payment.ErrProviderRejected)
	}
	params := &stripe.PaymentIntentCaptureParams{
		AmountToCapture: stripe.Int64(req.AmountCents),
	}
	params.IdempotencyKey = stripe.String(req.IdempotencyKey)
	pi, err := p.client.V1PaymentIntents.Capture(ctx, req.ProviderIntentID, params)
	if err != nil {
		return payment.CaptureResult{}, classify(err, "capture payment intent")
	}
	return payment.CaptureResult{
		ProviderPaymentID: pi.ID,
		Status:            "succeeded",
	}, nil
}

// mapRefundReason narrows our richer set of reasons onto the three Stripe
// accepts. Our own reason is preserved in the metadata, so nothing is lost.
func mapRefundReason(r payment.RefundReason) string {
	switch r {
	case payment.ReasonDuplicate:
		return string(stripe.RefundReasonDuplicate)
	default:
		return string(stripe.RefundReasonRequestedByCustomer)
	}
}

func mapRefundStatus(s stripe.RefundStatus) payment.RefundStatus {
	switch s {
	case stripe.RefundStatusSucceeded:
		return payment.RefundSucceeded
	case stripe.RefundStatusFailed, stripe.RefundStatusCanceled:
		return payment.RefundFailed
	default:
		return payment.RefundPending
	}
}

// Payout settles a doctor over Stripe Connect.
func (p *Provider) Payout(ctx context.Context, req payment.PayoutRequest) (payment.PayoutResult, error) {
	if req.DestinationAccount == "" {
		return payment.PayoutResult{}, fmt.Errorf("%w: doctor %s has no Stripe Connect account",
			payment.ErrProviderRejected, req.DoctorID)
	}
	params := &stripe.TransferCreateParams{
		Amount:      stripe.Int64(req.AmountCents),
		Currency:    stripe.String(currencyOf(req.Currency)),
		Destination: stripe.String(req.DestinationAccount),
		Description: stripe.String(req.Description),
		Metadata: map[string]string{
			"payout_id": req.PayoutID.String(),
			"doctor_id": req.DoctorID.String(),
		},
	}
	params.IdempotencyKey = stripe.String(req.IdempotencyKey)

	tr, err := p.client.V1Transfers.Create(ctx, params)
	if err != nil {
		return payment.PayoutResult{}, classify(err, "create transfer")
	}
	return payment.PayoutResult{TransferID: tr.ID, Status: payment.PayoutPaid}, nil
}

func currencyOf(c string) string {
	if c == "" {
		return string(stripe.CurrencyLKR)
	}
	return strings.ToLower(c)
}

func mapIntentStatus(s stripe.PaymentIntentStatus) payment.IntentStatus {
	switch s {
	case stripe.PaymentIntentStatusSucceeded:
		return payment.IntentSucceeded
	case stripe.PaymentIntentStatusRequiresAction,
		stripe.PaymentIntentStatusRequiresConfirmation,
		stripe.PaymentIntentStatusRequiresPaymentMethod:
		return payment.IntentRequiresAction
	case stripe.PaymentIntentStatusCanceled:
		return payment.IntentFailed
	default:
		return payment.IntentPending
	}
}

// classify turns a Stripe error into one of our two retry classes.
//
// The distinction is the whole point: a card decline must fail the payment
// immediately, while a 500 from Stripe must be retried with the same
// idempotency key. Getting it backwards either double-charges patients or
// tells them their working card was declined.
func classify(err error, op string) error {
	var se *stripe.Error
	if errors.As(err, &se) {
		switch se.Type {
		case stripe.ErrorTypeCard, stripe.ErrorTypeInvalidRequest:
			return fmt.Errorf("%w: %s: %s", payment.ErrProviderRejected, op, se.Code)
		case stripe.ErrorTypeIdempotency:
			// The same key was reused with different parameters. That is our
			// bug, not the patient's, and retrying will not fix it.
			return fmt.Errorf("%w: %s: idempotency key reused with different parameters", payment.ErrProviderRejected, op)
		default:
			return fmt.Errorf("%w: %s: %s", payment.ErrProviderUnavailable, op, se.Type)
		}
	}
	// Transport-level failures: timeout, connection reset, DNS. Never a
	// decision, always retryable.
	return fmt.Errorf("%w: %s: %w", payment.ErrProviderUnavailable, op, err)
}
