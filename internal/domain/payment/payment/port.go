package payment

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// This file is the seam between our money rules and someone else's API.
//
// Nothing below imports stripe-go, and nothing in service.go, handler.go or
// payout.go does either. When Stripe changes its Go SDK for the ninth time, or
// PayHere is replaced, or Dialog's Ideamart gateway is retired, the blast
// radius is one directory under provider/ and this file does not move.

// Provider errors. Callers switch on these rather than on provider-specific
// error strings, which is what lets three rails share one service layer.
var (
	// ErrProviderUnavailable means the request never reached a decision: a
	// timeout, a 5xx, a DNS failure. Safe to retry with the same idempotency
	// key.
	ErrProviderUnavailable = errors.New("payment: provider unavailable")

	// ErrProviderRejected means the provider decided, and the answer was no.
	// Retrying will not help; the payment fails.
	ErrProviderRejected = errors.New("payment: provider rejected the request")

	// ErrSignatureInvalid means a webhook did not authenticate. This is the
	// only outcome that must never be retried or logged with its body.
	ErrSignatureInvalid = errors.New("payment: webhook signature invalid")

	// ErrEventTooOld means the signature verified but the timestamp is outside
	// the replay window. Treated exactly like an invalid signature.
	ErrEventTooOld = errors.New("payment: webhook timestamp outside tolerance")

	// ErrWebhookMalformed is a body we authenticated but cannot read: the
	// signature was good, the JSON was not.
	//
	// It is deliberately NOT ErrSignatureInvalid. Reporting a payload problem
	// as a signature failure sends whoever is debugging it after the crypto --
	// rotating secrets, re-checking HMAC construction, suspecting clock skew --
	// when the actual fault is a field name. That cost an hour on this
	// platform's own mock rail, and it would cost far more against a live
	// provider at 2am.
	ErrWebhookMalformed = errors.New("payment: webhook body could not be read")

	// ErrProviderNotConfigured means this deployment has no credentials for
	// that rail. It is a deployment fault, not a caller fault, and it will not
	// resolve on retry -- so the handler answers 501 rather than 500. A 5xx
	// that looks transient makes Stripe retry a permanently broken endpoint
	// with backoff for days.
	ErrProviderNotConfigured = errors.New("payment: provider is not configured")

	// ErrUnsupported means this rail does not implement the operation, e.g.
	// Dialog carrier billing cannot pay a doctor.
	ErrUnsupported = errors.New("payment: operation not supported by this provider")
)

// IntentStatus is the provider-reported state of a freshly created intent.
type IntentStatus string

const (
	IntentPending        IntentStatus = "pending"
	IntentRequiresAction IntentStatus = "requires_action"
	IntentRequiresPIN    IntentStatus = "requires_pin"
	IntentSucceeded      IntentStatus = "succeeded"
	IntentFailed         IntentStatus = "failed"
)

// Status maps a provider intent status onto our payment status.
func (s IntentStatus) Status() Status {
	switch s {
	case IntentRequiresAction:
		return StatusRequiresAction
	case IntentRequiresPIN:
		return StatusRequiresPIN
	case IntentSucceeded:
		return StatusSucceeded
	case IntentFailed:
		return StatusFailed
	default:
		return StatusPending
	}
}

// IntentRequest asks a provider to begin collecting money.
type IntentRequest struct {
	PaymentID     uuid.UUID
	AppointmentID uuid.UUID
	PatientID     uuid.UUID
	DoctorID      uuid.UUID

	AmountCents int64
	Currency    string

	// IdempotencyKey is stable for the lifetime of the payment. Sending the
	// same key twice must produce the same intent, not a second charge.
	IdempotencyKey string

	Description string
	// PatientPhone is E.164 and is only populated for carrier billing, which
	// cannot work without it. It is never logged unmasked.
	PatientPhone string
	ReturnURL    string
	// AuthorizeOnly asks the provider to pre-authorize / hold funds rather than
	// immediately charging. Supported by PayHere Hold on Card.
	AuthorizeOnly bool
	// ScheduledStartAt is the booked consultation time. Used to decide whether
	// the hold window (e.g. PayHere's 7-day max) is safe.
	ScheduledStartAt *time.Time
}

// IntentResult is what the provider gave back.
type IntentResult struct {
	ProviderIntentID string
	// ClientSecret is Stripe's browser-side confirmation token. It is a
	// short-lived secret: returned to the owning patient, never logged.
	ClientSecret string
	// RedirectURL is PayHere's hosted checkout, if the rail uses one.
	RedirectURL string
	// Reference is the provider's own correlation id, e.g. Dialog's
	// referenceNo for a PIN challenge.
	Reference string

	Status IntentStatus
	// ProviderFeeCents is the rail's own fee when it reports one up front.
	// Zero means "unknown"; the commission rule's modelled fee is used instead.
	ProviderFeeCents int64
	ExpiresAt        *time.Time
}

// WebhookEvent is a provider callback that has already been authenticated.
// A provider returns this only after the signature checked out; the service
// layer never sees an unverified event.
type WebhookEvent struct {
	// EventID is the provider's unique id for this delivery. It is the value
	// the (provider, event_id) unique index deduplicates on, so it must be
	// stable across redeliveries of the same event.
	EventID string
	// Type is the provider's own event name, kept verbatim for the audit row.
	Type string

	// Outcome is the normalised meaning: what this event says happened.
	Outcome WebhookOutcome

	// Correlation. At least one of these must be set or the event cannot be
	// matched to a payment.
	ProviderIntentID string
	AppointmentID    string
	PaymentID        string

	AmountCents   int64
	Currency      string
	FailureReason string
	// ProviderFeeCents is the real fee, when the rail reports it on the
	// settlement event. It supersedes the modelled fee.
	ProviderFeeCents int64
	// ProviderRefundID correlates a refund.* event back to our refunds row.
	ProviderRefundID string
	// AuthorizationToken carries the token issued on a successful card hold
	// (e.g. PayHere Hold on Card status_code 3).
	AuthorizationToken string

	OccurredAt time.Time
	// Raw is the exact bytes received, stored for dispute evidence.
	Raw []byte
}

// WebhookOutcome is the normalised meaning of a provider event.
type WebhookOutcome string

const (
	OutcomePaymentAuthorized WebhookOutcome = "payment_authorized"
	OutcomePaymentSucceeded  WebhookOutcome = "payment_succeeded"
	OutcomePaymentFailed     WebhookOutcome = "payment_failed"
	OutcomePaymentPending    WebhookOutcome = "payment_pending"
	OutcomeRefundSucceeded   WebhookOutcome = "refund_succeeded"
	OutcomeRefundFailed      WebhookOutcome = "refund_failed"
	OutcomePayoutPaid        WebhookOutcome = "payout_paid"
	OutcomePayoutFailed      WebhookOutcome = "payout_failed"
	// OutcomeSetupSucceeded is a card-on-file setup the patient completed.
	// It carries no payment: ProviderIntentID names the setup intent, and the
	// vault resolves ownership from the metadata the rail echoes back.
	OutcomeSetupSucceeded WebhookOutcome = "setup_succeeded"
	// OutcomeIgnored is a well-formed, authentic event this service has no
	// rule for. It is recorded and acknowledged, never retried.
	OutcomeIgnored WebhookOutcome = "ignored"
)

// CaptureRequest asks a provider to programmatically capture held funds.
type CaptureRequest struct {
	PaymentID          uuid.UUID
	ProviderIntentID   string
	AuthorizationToken string
	AmountCents        int64
	Currency           string
	Description        string
	IdempotencyKey     string
}

// CaptureResult is the provider's answer to a capture request.
type CaptureResult struct {
	ProviderPaymentID string
	Status            string
	ProviderFeeCents  int64
	FailureReason     string
}

// RefundRequest asks a provider to return money.
type RefundRequest struct {
	PaymentID        uuid.UUID
	RefundID         uuid.UUID
	ProviderIntentID string
	AmountCents      int64
	Currency         string
	Reason           RefundReason
	IdempotencyKey   string
}

// RefundResult is the provider's answer.
type RefundResult struct {
	ProviderRefundID string
	Status           RefundStatus
	FailureReason    string
}

// PayoutRequest asks a provider to settle a doctor.
type PayoutRequest struct {
	PayoutID uuid.UUID
	DoctorID uuid.UUID
	// DestinationAccount is the rail-specific account handle: a Stripe Connect
	// account id, a PayHere merchant reference, a bank token. This service does
	// not store bank details; it stores the opaque handle the rail issued.
	DestinationAccount string
	AmountCents        int64
	Currency           string
	Description        string
	IdempotencyKey     string
}

// PayoutResult is the provider's answer.
type PayoutResult struct {
	TransferID    string
	Status        PayoutStatus
	FailureReason string
}

// PaymentProvider is the one interface every payment rail implements.
//
// The signature is deliberately uniform across rails that are not uniform:
// Stripe is a card processor, PayHere is a hosted checkout, Dialog is a
// telco billing a phone account. Anything a rail cannot do returns
// ErrUnsupported rather than pretending.
type PaymentProvider interface {
	// CreateIntent begins collection. It must be idempotent on
	// IntentRequest.IdempotencyKey.
	CreateIntent(ctx context.Context, req IntentRequest) (IntentResult, error)

	// VerifyWebhook authenticates a raw callback and normalises it. It returns
	// ErrSignatureInvalid or ErrEventTooOld and nothing else on failure -- in
	// particular it never returns a partially parsed event alongside an error,
	// because a caller that ignores the error would then act on unverified data.
	VerifyWebhook(ctx context.Context, headers http.Header, body []byte) (WebhookEvent, error)

	// Capture programmatically charges previously authorized/held funds.
	Capture(ctx context.Context, req CaptureRequest) (CaptureResult, error)

	// Refund returns money against a previously captured intent.
	Refund(ctx context.Context, req RefundRequest) (RefundResult, error)

	// Payout settles a doctor.
	Payout(ctx context.Context, req PayoutRequest) (PayoutResult, error)

	// Name is the rail identifier used in the database and in metrics.
	Name() string
}

// ProviderPINChallenge is what a carrier-billing rail returns after sending a
// PIN. It is the provider's answer, not our record of it: the persisted state
// -- with its expiry, attempt counter and status -- is payment.PINChallenge in
// pin.go. Keeping them as two types stops a provider field silently becoming
// a database column.
type ProviderPINChallenge struct {
	Reference string
	ExpiresAt time.Time
	// MaskedMSISDN is the phone number the PIN went to, already masked. The
	// unmasked number never leaves the provider package.
	MaskedMSISDN string
}

// PINConfirmation carries the code the patient typed back.
type PINConfirmation struct {
	PaymentID      uuid.UUID
	Reference      string
	PIN            string
	AmountCents    int64
	Currency       string
	IdempotencyKey string
}

// PINProvider is implemented by rails whose flow is genuinely two-step:
// send a PIN by SMS, then debit once the patient proves possession of the
// handset. Dialog Ideamart carrier billing works this way, and it is the rail
// that matters most in Sri Lanka because a large share of patients have a
// phone account and no card.
//
// It is a separate interface rather than two more methods on PaymentProvider
// so that Stripe and PayHere are not forced to carry two permanently
// unsupported methods.
type PINProvider interface {
	PaymentProvider

	// SendPIN issues the challenge. CreateIntent on a PINProvider calls this
	// and reports IntentRequiresPIN.
	SendPIN(ctx context.Context, req IntentRequest) (ProviderPINChallenge, error)

	// ConfirmPIN verifies the code and performs the debit in one step, because
	// the Ideamart API couples them and pretending otherwise would invent a
	// state that does not exist at the carrier.
	ConfirmPIN(ctx context.Context, req PINConfirmation) (IntentResult, error)
}

// Registry resolves a provider by name. It is populated once at boot from
// configuration; an unconfigured rail is simply absent, which is why a
// misconfigured deployment fails at the first request with a clear error
// rather than silently charging through the wrong gateway.
type Registry struct {
	providers map[ProviderName]PaymentProvider
	def       ProviderName
}

// NewRegistry builds a registry. The default provider is used when a request
// does not name one.
func NewRegistry(def ProviderName) *Registry {
	return &Registry{providers: map[ProviderName]PaymentProvider{}, def: def}
}

// Register adds a provider. Registering the same name twice replaces it, which
// only happens in tests.
func (r *Registry) Register(p PaymentProvider) {
	r.providers[ProviderName(p.Name())] = p
}

// Get returns the named provider, or the default when name is empty.
func (r *Registry) Get(name ProviderName) (PaymentProvider, error) {
	if name == "" {
		name = r.def
	}
	p, ok := r.providers[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrProviderNotConfigured, name)
	}
	return p, nil
}

// Default returns the name of the fallback provider.
func (r *Registry) Default() ProviderName { return r.def }

// Names lists the configured rails, for the readiness endpoint and the logs.
func (r *Registry) Names() []ProviderName {
	out := make([]ProviderName, 0, len(r.providers))
	for n := range r.providers {
		out = append(out, n)
	}
	return out
}

// --- tokenised payment methods ---------------------------------------------

// VaultCustomerRequest asks a rail for a stable customer handle.
//
// It carries the patient's UUID and nothing else. Not a name, not a phone
// number, not an email: the rail does not need them to hold a token, and a
// breach of the payment account must not be a breach of who this platform's
// patients are.
type VaultCustomerRequest struct {
	PatientID      uuid.UUID
	IdempotencyKey string
}

// VaultSetupRequest asks a rail to begin collecting a card for future use.
type VaultSetupRequest struct {
	PatientID          uuid.UUID
	ProviderCustomerID string
	IdempotencyKey     string
}

// VaultSetupResult is what the client's SDK sheet needs to finish the setup.
type VaultSetupResult struct {
	SetupIntentID string
	// ClientSecret is short-lived and scoped to this one setup. Returned to
	// the owning patient, never logged.
	ClientSecret string
	// EphemeralKey lets the provider's sheet list the patient's already-saved
	// cards. Empty when the rail has no such concept.
	EphemeralKey string
}

// VaultSetupStatus is the rail's view of a setup after the patient confirmed
// it. PatientID comes from the metadata the rail echoed back, which is what
// lets the unauthenticated webhook path establish ownership.
type VaultSetupStatus struct {
	SetupIntentID      string
	Succeeded          bool
	ProviderToken      string
	ProviderCustomerID string
	PatientID          uuid.UUID
}

// VaultMethod is the display metadata a rail reports about a token.
//
// Every field here is the provider's answer, never a client's assertion. A
// client that could name its own brand and last4 could attach somebody else's
// token and label it convincingly.
type VaultMethod struct {
	Type     string // card | wallet | bank
	Brand    string
	Last4    string
	ExpMonth int
	ExpYear  int
}

// validate refuses anything that does not look like display metadata.
//
// The Last4 check is the load-bearing one and it is deliberately paranoid: it
// is the only numeric field on a saved method, so it is the only place a full
// card number could ever be written by mistake. Four digits, or nothing.
func (m VaultMethod) validate() error {
	switch m.Type {
	case "card", "wallet", "bank":
	case "":
		return errors.New("payment: the rail reported a token with no method type")
	default:
		return fmt.Errorf("payment: unknown saved method type %q", m.Type)
	}
	if m.Last4 != "" {
		if len(m.Last4) != 4 {
			return errors.New("payment: last4 must be exactly four digits; refusing to store anything longer")
		}
		for _, r := range m.Last4 {
			if r < '0' || r > '9' {
				return errors.New("payment: last4 must be digits only")
			}
		}
	}
	if m.ExpMonth < 0 || m.ExpMonth > 12 {
		return fmt.Errorf("payment: expiry month %d is out of range", m.ExpMonth)
	}
	if m.ExpYear != 0 && (m.ExpYear < 2000 || m.ExpYear > 2100) {
		return fmt.Errorf("payment: expiry year %d is out of range", m.ExpYear)
	}
	return nil
}

// VaultProvider is implemented by rails that can tokenise an instrument for
// re-use. It is a separate interface from PaymentProvider for the same reason
// PINProvider is: a carrier billing a phone account has no cards to vault, and
// forcing Dialog to carry four permanently unsupported methods would make
// "unsupported" the normal case for the interface.
//
// Nothing in this interface takes a card number, and nothing returns one.
type VaultProvider interface {
	PaymentProvider

	// EnsureCustomer returns the rail's stable handle for a patient,
	// creating it if necessary. It must be idempotent on IdempotencyKey.
	EnsureCustomer(ctx context.Context, req VaultCustomerRequest) (string, error)

	// CreateSetupIntent begins a card-on-file collection. The card itself is
	// entered inside the rail's own SDK sheet; this only issues the secret
	// that sheet is scoped to.
	CreateSetupIntent(ctx context.Context, req VaultSetupRequest) (VaultSetupResult, error)

	// GetSetupIntent reports whether a setup completed and, if so, which
	// token it produced. It is the authority on ownership: the patient id it
	// returns comes from metadata the rail stored at creation, not from the
	// caller.
	GetSetupIntent(ctx context.Context, setupIntentID string) (VaultSetupStatus, error)

	// DescribeMethod returns display metadata for a token.
	DescribeMethod(ctx context.Context, token string) (VaultMethod, error)

	// DetachMethod makes a token unusable at the rail. Detaching one that is
	// already gone returns ErrNotFound, which callers treat as success.
	DetachMethod(ctx context.Context, token string) error
}
