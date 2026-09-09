package stripeprovider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	stripe "github.com/stripe/stripe-go/v86"

	"telemed/internal/domain/payment/payment"
)

// The Stripe card vault.
//
// # What crosses the boundary
//
// Outbound: a patient UUID, an amount of nothing, and an idempotency key. No
// name, no phone number, no email, no diagnosis, no appointment detail. A
// Stripe Customer created here is a bare object with one metadata key, so a
// compromise of the Stripe account discloses that some UUIDs pay for things
// and nothing else about who they are.
//
// Inbound: a PaymentMethod id and four display fields. The card number never
// exists in this process -- the patient types it into Stripe's own sheet on
// their handset, which talks to Stripe directly using the client secret this
// file issues.
//
// # Why the patient id lives in metadata
//
// The setup_intent.succeeded webhook has no authenticated caller. Ownership
// has to come from somewhere the client cannot forge, and metadata written by
// us at creation time and echoed back by Stripe is that place. A design where
// the client posted "this setup is mine" would let any patient claim any
// completed setup by guessing an id.

var _ payment.VaultProvider = (*Provider)(nil)

const metadataPatientID = "patient_id"

// EnsureCustomer creates the rail-side customer a saved card attaches to.
//
// Stripe has no "get or create" primitive, so idempotency is carried entirely
// by the key: the same key returns the same Customer instead of a second one.
// Callers still persist the returned handle, because an idempotency key is
// only honoured for 24 hours.
func (p *Provider) EnsureCustomer(ctx context.Context, req payment.VaultCustomerRequest) (string, error) {
	params := &stripe.CustomerCreateParams{
		// Description, not Name: a name would be personal data we have no
		// reason to send. This string is what an operator sees in the Stripe
		// dashboard when they need to correlate a charge back to a patient.
		Description: stripe.String("telemed patient " + req.PatientID.String()),
		Metadata:    map[string]string{metadataPatientID: req.PatientID.String()},
	}
	params.IdempotencyKey = stripe.String(req.IdempotencyKey)

	cus, err := p.client.V1Customers.Create(ctx, params)
	if err != nil {
		return "", classify(err, "create customer")
	}
	return cus.ID, nil
}

// CreateSetupIntent issues the client secret the patient's SDK sheet confirms
// against, plus an ephemeral key so that sheet can also list the cards they
// already saved.
//
// Usage is off_session, which is what tells Stripe the card is being collected
// for a later charge rather than for one happening now. Getting that wrong
// means the saved card fails 3-D Secure the first time it is actually used,
// months later, with no way to ask the patient to re-authenticate.
func (p *Provider) CreateSetupIntent(ctx context.Context, req payment.VaultSetupRequest) (payment.VaultSetupResult, error) {
	if strings.TrimSpace(req.ProviderCustomerID) == "" {
		return payment.VaultSetupResult{}, fmt.Errorf("%w: a setup intent needs a customer to attach the card to", payment.ErrProviderRejected)
	}

	params := &stripe.SetupIntentCreateParams{
		Customer: stripe.String(req.ProviderCustomerID),
		Usage:    stripe.String("off_session"),
		Metadata: map[string]string{metadataPatientID: req.PatientID.String()},
		AutomaticPaymentMethods: &stripe.SetupIntentCreateAutomaticPaymentMethodsParams{
			Enabled: stripe.Bool(true),
		},
	}
	params.IdempotencyKey = stripe.String(req.IdempotencyKey)

	si, err := p.client.V1SetupIntents.Create(ctx, params)
	if err != nil {
		return payment.VaultSetupResult{}, classify(err, "create setup intent")
	}

	out := payment.VaultSetupResult{
		SetupIntentID: si.ID,
		ClientSecret:  si.ClientSecret,
	}

	// The ephemeral key is what turns a card-entry form into a wallet: without
	// it Stripe's sheet can only collect a new card, so a patient with three
	// saved cards is asked to type a fourth. A failure here is not fatal to
	// the setup itself, so it degrades to "new card only" rather than failing
	// the whole call.
	ek, err := p.client.V1EphemeralKeys.Create(ctx, &stripe.EphemeralKeyCreateParams{
		Customer:      stripe.String(req.ProviderCustomerID),
		StripeVersion: stripe.String(stripe.APIVersion),
	})
	if err == nil && ek != nil {
		out.EphemeralKey = ek.Secret
	}
	return out, nil
}

// GetSetupIntent reports whether the patient completed the setup, and is the
// authority on who it belongs to.
func (p *Provider) GetSetupIntent(ctx context.Context, setupIntentID string) (payment.VaultSetupStatus, error) {
	si, err := p.client.V1SetupIntents.Retrieve(ctx, setupIntentID, nil)
	if err != nil {
		return payment.VaultSetupStatus{}, classify(err, "retrieve setup intent")
	}

	out := payment.VaultSetupStatus{
		SetupIntentID: si.ID,
		Succeeded:     si.Status == stripe.SetupIntentStatusSucceeded,
	}
	if si.PaymentMethod != nil {
		out.ProviderToken = si.PaymentMethod.ID
	}
	if si.Customer != nil {
		out.ProviderCustomerID = si.Customer.ID
	}
	if raw, ok := si.Metadata[metadataPatientID]; ok {
		if id, parseErr := uuid.Parse(raw); parseErr == nil {
			out.PatientID = id
		}
	}
	return out, nil
}

// DescribeMethod returns the four display fields, and only those four.
func (p *Provider) DescribeMethod(ctx context.Context, token string) (payment.VaultMethod, error) {
	pm, err := p.client.V1PaymentMethods.Retrieve(ctx, token, nil)
	if err != nil {
		return payment.VaultMethod{}, classify(err, "retrieve payment method")
	}

	out := payment.VaultMethod{Type: mapMethodType(pm.Type)}
	if pm.Card != nil {
		out.Brand = string(pm.Card.Brand)
		out.Last4 = pm.Card.Last4
		//nolint:gosec // Stripe reports month and year as int64; both are
		// bounded by the card's own format and by the CHECK constraints on
		// payment_methods, which refuse anything outside 1-12 and 2000-2100.
		out.ExpMonth = int(pm.Card.ExpMonth)
		//nolint:gosec // see above.
		out.ExpYear = int(pm.Card.ExpYear)
	}
	return out, nil
}

// DetachMethod makes a token unusable at Stripe.
//
// A token Stripe has already forgotten comes back as resource_missing, which
// is translated to payment.ErrNotFound: the caller treats deleting an
// already-deleted card as success, because it is.
func (p *Provider) DetachMethod(ctx context.Context, token string) error {
	if _, err := p.client.V1PaymentMethods.Detach(ctx, token, nil); err != nil {
		var se *stripe.Error
		if errors.As(err, &se) && se.Code == stripe.ErrorCodeResourceMissing {
			return payment.ErrNotFound
		}
		return classify(err, "detach payment method")
	}
	return nil
}

// mapMethodType narrows Stripe's long list of payment method types onto the
// three the schema knows about. Anything unrecognised is reported as a wallet
// rather than guessed at, because "card" carries an expectation (a PAN behind
// it, an expiry to warn about) that a Klarna or an ACH mandate does not meet.
func mapMethodType(t stripe.PaymentMethodType) string {
	switch t {
	case stripe.PaymentMethodTypeCard:
		return "card"
	case stripe.PaymentMethodTypeUSBankAccount, stripe.PaymentMethodTypeSEPADebit,
		stripe.PaymentMethodTypeBACSDebit, stripe.PaymentMethodTypeACSSDebit:
		return "bank"
	default:
		return "wallet"
	}
}
