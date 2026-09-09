package payment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Saved payment methods.
//
// # What this service stores, and what it refuses to
//
// A provider token. That is all. There is no card number in this file, in the
// schema, in a log line or in an event payload, and there is no code path that
// could put one there: the card is typed into the provider's own SDK sheet on
// the handset, the provider returns an opaque handle, and the handle is the
// only thing that ever reaches this process.
//
// `brand`, `last4` and the expiry are display metadata *the provider tells
// us* about a card we have never seen. They are never accepted from a client:
// a client that could assert its own brand and last4 could attach somebody
// else's token to its own account and label it convincingly. Every field on a
// saved method comes from VaultProvider.DescribeMethod.
//
// # Why there are two ways to record a saved card
//
// The client confirms a SetupIntent inside the provider's sheet and then has
// to tell somebody. Two things can do that, and both are wired:
//
//   - the client itself, via POST /payments/methods with the setup intent id.
//     Immediate, so the card is in the list by the time the screen refreshes.
//   - the provider's setup_intent.succeeded webhook. Slower, but it still
//     arrives if the app was killed the instant after the sheet closed.
//
// They race constantly and that is fine: both funnel into recordSetupIntent,
// and the UNIQUE (provider, provider_token) index makes the loser a no-op
// rather than a duplicate card.

// PaymentMethod is one tokenised instrument belonging to one patient.
type PaymentMethod struct {
	ID        uuid.UUID `json:"id"`
	PatientID uuid.UUID `json:"-"`

	Provider ProviderName `json:"provider"`
	// ProviderCustomerID and ProviderToken are the rail's handles. Neither is
	// serialised to a client: they are credentials against the rail, and a
	// client that held one could act on the card outside our authorisation.
	ProviderCustomerID string `json:"-"`
	ProviderToken      string `json:"-"`

	MethodType string `json:"type"`
	Brand      string `json:"brand,omitempty"`
	// Last4 is the last four digits of the PAN. Storing exactly four is
	// explicitly permitted, and it is the only way a patient can recognise
	// their own card in a list.
	Last4    string `json:"last4,omitempty"`
	ExpMonth int    `json:"exp_month,omitempty"`
	ExpYear  int    `json:"exp_year,omitempty"`

	IsDefault bool `json:"is_default"`

	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"-"`
	Version   int        `json:"-"`
}

// Expired reports whether the card's own expiry has passed. A saved card that
// expired last month is still listed -- the patient should see why it stopped
// working rather than watch it vanish -- but it is flagged.
func (m PaymentMethod) Expired(now time.Time) bool {
	if m.ExpYear == 0 || m.ExpMonth == 0 {
		return false
	}
	end := time.Date(m.ExpYear, time.Month(m.ExpMonth), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	return !now.Before(end)
}

// PaymentCustomer maps a patient onto the rail's customer handle.
type PaymentCustomer struct {
	ID                 uuid.UUID
	PatientID          uuid.UUID
	Provider           ProviderName
	ProviderCustomerID string
	CreatedAt          time.Time
	UpdatedAt          time.Time
	Version            int
}

// VaultConfig names the rail that holds saved methods.
type VaultConfig struct {
	// Provider is the rail a SetupIntent is created on. Only one rail can own
	// the vault at a time: a saved card is meaningless to a rail other than
	// the one that tokenised it.
	Provider ProviderName
	// Enabled turns the whole surface off. When false the endpoints are still
	// routed and answer 501, which tells a client "this deployment does not
	// do saved cards" instead of "something broke".
	Enabled bool
}

func (c VaultConfig) withDefaults() VaultConfig {
	if c.Provider == "" {
		c.Provider = ProviderStripe
	}
	return c
}

// SetupIntentView is what the client hands to the provider's SDK sheet.
type SetupIntentView struct {
	SetupIntentID string `json:"setup_intent_id"`
	// ClientSecret is a short-lived secret scoped to this one setup. It is
	// returned to the owning patient and never logged.
	ClientSecret string `json:"client_secret"`
	// CustomerID and EphemeralKey let the provider's sheet display the
	// patient's already-saved cards alongside the new-card form. Without them
	// the sheet can only ever collect a new card, which is why a vault with
	// no ephemeral key is a list screen and not a checkout feature.
	CustomerID   string `json:"customer_id,omitempty"`
	EphemeralKey string `json:"ephemeral_key,omitempty"`
	Provider     string `json:"provider"`
}

// --- errors ----------------------------------------------------------------

var (
	// ErrVaultDisabled means no rail in this deployment stores payment
	// methods.
	ErrVaultDisabled = errors.New("payment: saved payment methods are not enabled on this deployment")
	// ErrSetupIncomplete means the SetupIntent exists but the patient never
	// finished it, so there is no token to save.
	ErrSetupIncomplete = errors.New("payment: that card setup was not completed")
	// ErrMethodNotOwned means the setup intent belongs to a different patient.
	ErrMethodNotOwned = errors.New("payment: that card setup belongs to another patient")
)

// --- service ---------------------------------------------------------------

// vaultRail resolves the configured vault provider, or explains why there
// isn't one.
func (s *Service) vaultRail() (VaultProvider, error) {
	if !s.vaultCfg.Enabled {
		return nil, ErrVaultDisabled
	}
	rail, err := s.providers.Get(s.vaultCfg.Provider)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrVaultDisabled, err)
	}
	vault, ok := rail.(VaultProvider)
	if !ok {
		return nil, fmt.Errorf("%w: %s does not tokenise payment methods", ErrUnsupported, s.vaultCfg.Provider)
	}
	return vault, nil
}

// StartCardSetup creates a provider SetupIntent the client's SDK sheet can
// confirm.
//
// The customer handle is created on first use and reused forever after, so a
// patient's saved cards all live under one customer at the rail. Two handles
// for one patient would mean half their cards silently invisible.
func (s *Service) StartCardSetup(ctx context.Context, patientID uuid.UUID) (SetupIntentView, error) {
	vault, err := s.vaultRail()
	if err != nil {
		return SetupIntentView{}, err
	}

	customerID, err := s.ensureVaultCustomer(ctx, vault, patientID)
	if err != nil {
		return SetupIntentView{}, err
	}

	res, err := vault.CreateSetupIntent(ctx, VaultSetupRequest{
		PatientID:          patientID,
		ProviderCustomerID: customerID,
		// Stable per patient, so a client that retries after a lost response
		// gets the same SetupIntent back instead of leaving a trail of
		// abandoned ones at the rail.
		IdempotencyKey: "setup:" + patientID.String(),
	})
	if err != nil {
		return SetupIntentView{}, err
	}

	return SetupIntentView{
		SetupIntentID: res.SetupIntentID,
		ClientSecret:  res.ClientSecret,
		CustomerID:    customerID,
		EphemeralKey:  res.EphemeralKey,
		Provider:      string(s.vaultCfg.Provider),
	}, nil
}

// ensureVaultCustomer reads, or creates and stores, the rail's customer handle
// for a patient.
//
// The insert is guarded by UNIQUE (patient_id, provider), so two concurrent
// first-time setups cannot produce two handles: the loser re-reads the
// winner's row. It may leave one orphaned customer at the rail, which is
// harmless -- an empty Stripe Customer with no cards attached -- and far
// better than the alternative of a patient whose saved cards are split.
func (s *Service) ensureVaultCustomer(ctx context.Context, vault VaultProvider, patientID uuid.UUID) (string, error) {
	existing, err := s.store.GetPaymentCustomer(ctx, patientID, s.vaultCfg.Provider)
	if err == nil {
		return existing.ProviderCustomerID, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return "", err
	}

	customerID, err := vault.EnsureCustomer(ctx, VaultCustomerRequest{
		PatientID: patientID,
		// Nothing but the id crosses to the rail. Not a name, not a phone,
		// not an email: a breach of the payment account must not disclose who
		// this platform's patients are.
		IdempotencyKey: "customer:" + patientID.String(),
	})
	if err != nil {
		return "", err
	}

	row := PaymentCustomer{
		ID:                 uuid.New(),
		PatientID:          patientID,
		Provider:           s.vaultCfg.Provider,
		ProviderCustomerID: customerID,
	}
	err = s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		return tx.InsertPaymentCustomer(ctx, &row)
	})
	if errors.Is(err, ErrDuplicate) {
		again, readErr := s.store.GetPaymentCustomer(ctx, patientID, s.vaultCfg.Provider)
		if readErr != nil {
			return "", readErr
		}
		return again.ProviderCustomerID, nil
	}
	if err != nil {
		return "", err
	}
	return customerID, nil
}

// ConfirmCardSetup records the card a completed SetupIntent produced.
//
// patientID is the authenticated caller when the client calls this, and
// uuid.Nil when the provider webhook does -- the webhook has no caller, so
// ownership comes from the setup intent's own metadata. Either way the
// metadata is checked against the caller, so one patient cannot claim
// another's setup by guessing its id.
func (s *Service) ConfirmCardSetup(ctx context.Context, patientID uuid.UUID, setupIntentID string) (PaymentMethod, error) {
	vault, err := s.vaultRail()
	if err != nil {
		return PaymentMethod{}, err
	}
	if strings.TrimSpace(setupIntentID) == "" {
		return PaymentMethod{}, fmt.Errorf("%w: no setup intent was named", ErrSetupIncomplete)
	}

	status, err := vault.GetSetupIntent(ctx, setupIntentID)
	if err != nil {
		return PaymentMethod{}, err
	}
	if !status.Succeeded || status.ProviderToken == "" {
		return PaymentMethod{}, ErrSetupIncomplete
	}
	owner := status.PatientID
	if owner == uuid.Nil {
		return PaymentMethod{}, fmt.Errorf("%w: the setup intent names no patient", ErrSetupIncomplete)
	}
	if patientID != uuid.Nil && owner != patientID {
		return PaymentMethod{}, ErrMethodNotOwned
	}

	described, err := vault.DescribeMethod(ctx, status.ProviderToken)
	if err != nil {
		return PaymentMethod{}, err
	}

	return s.recordMethod(ctx, owner, status, described)
}

// recordMethod writes the saved card, making it the default when it is the
// patient's first.
func (s *Service) recordMethod(ctx context.Context, patientID uuid.UUID, status VaultSetupStatus, m VaultMethod) (PaymentMethod, error) {
	if err := m.validate(); err != nil {
		return PaymentMethod{}, err
	}

	out := PaymentMethod{
		ID:                 uuid.New(),
		PatientID:          patientID,
		Provider:           s.vaultCfg.Provider,
		ProviderCustomerID: status.ProviderCustomerID,
		ProviderToken:      status.ProviderToken,
		MethodType:         m.Type,
		Brand:              m.Brand,
		Last4:              m.Last4,
		ExpMonth:           m.ExpMonth,
		ExpYear:            m.ExpYear,
	}

	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		if existing, ok, err := tx.FindPaymentMethodByToken(ctx, out.Provider, out.ProviderToken); err != nil {
			return err
		} else if ok {
			// The other recording path got here first. Refresh the display
			// metadata and keep the row that exists.
			existing.Brand, existing.Last4 = m.Brand, m.Last4
			existing.ExpMonth, existing.ExpYear = m.ExpMonth, m.ExpYear
			if err := tx.UpdatePaymentMethod(ctx, &existing); err != nil {
				return err
			}
			out = existing
			return nil
		}

		n, err := tx.CountPaymentMethods(ctx, patientID)
		if err != nil {
			return err
		}
		out.IsDefault = n == 0
		return tx.InsertPaymentMethod(ctx, &out)
	})
	if errors.Is(err, ErrDuplicate) {
		// Lost the race between the FindByToken read and the insert. The
		// winner's row is the answer.
		existing, ok, readErr := s.store.FindPaymentMethodByToken(ctx, out.Provider, out.ProviderToken)
		if readErr != nil {
			return PaymentMethod{}, readErr
		}
		if ok {
			return existing, nil
		}
	}
	if err != nil {
		return PaymentMethod{}, err
	}
	return out, nil
}

// ListCards returns a patient's saved methods, default first.
func (s *Service) ListCards(ctx context.Context, patientID uuid.UUID) ([]PaymentMethod, error) {
	if !s.vaultCfg.Enabled {
		return nil, ErrVaultDisabled
	}
	return s.store.ListPaymentMethods(ctx, patientID)
}

// SetDefaultCard makes one method the default, atomically.
//
// Clearing the old default and setting the new one happen in one transaction
// under the partial unique index idx_payment_methods_one_default, so the
// database refuses any interleaving that would leave a patient with two
// defaults -- which is not cosmetic: checkout would pick whichever row came
// back first and charge a card the patient did not choose.
func (s *Service) SetDefaultCard(ctx context.Context, patientID, methodID uuid.UUID) (PaymentMethod, error) {
	if !s.vaultCfg.Enabled {
		return PaymentMethod{}, ErrVaultDisabled
	}

	var out PaymentMethod
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		m, err := tx.LockPaymentMethod(ctx, methodID)
		if err != nil {
			return err
		}
		if m.PatientID != patientID {
			return ErrForbidden
		}
		if err := tx.ClearDefaultPaymentMethod(ctx, patientID); err != nil {
			return err
		}
		m.IsDefault = true
		if err := tx.UpdatePaymentMethod(ctx, &m); err != nil {
			return err
		}
		out = m
		return nil
	})
	return out, err
}

// ForgetCard detaches a saved method at the rail and soft-deletes the row.
//
// The rail is told first. If we deleted our row and the detach then failed,
// the token would stay chargeable at the provider with nothing on our side
// recording that it exists -- a card the patient believes they deleted and we
// can no longer even see. Doing it in this order means the worst case is a
// detached token we still list, which the next delete retries cleanly.
func (s *Service) ForgetCard(ctx context.Context, patientID, methodID uuid.UUID) error {
	vault, err := s.vaultRail()
	if err != nil {
		return err
	}

	m, err := s.store.GetPaymentMethod(ctx, methodID)
	if err != nil {
		return err
	}
	if m.PatientID != patientID {
		return ErrForbidden
	}

	if err := vault.DetachMethod(ctx, m.ProviderToken); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}

	now := s.now().UTC()
	return s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		locked, err := tx.LockPaymentMethod(ctx, methodID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil // already gone; deleting twice is not an error
			}
			return err
		}
		if locked.PatientID != patientID {
			return ErrForbidden
		}
		wasDefault := locked.IsDefault
		if err := tx.SoftDeletePaymentMethod(ctx, methodID, now); err != nil {
			return err
		}
		if !wasDefault {
			return nil
		}
		// A patient who deletes their default is left with no default at all
		// unless something promotes one, and a checkout with no default falls
		// back to "enter a card" for someone who has three saved.
		return tx.PromoteOldestPaymentMethod(ctx, patientID)
	})
}

// OnSetupIntentWebhook is the provider-driven half of card recording. It is
// called from the webhook path with no authenticated caller, so ownership
// comes entirely from the setup intent's metadata.
func (s *Service) OnSetupIntentWebhook(ctx context.Context, setupIntentID string) error {
	_, err := s.ConfirmCardSetup(ctx, uuid.Nil, setupIntentID)
	switch {
	case errors.Is(err, ErrVaultDisabled), errors.Is(err, ErrSetupIncomplete):
		// Nothing to record, and no amount of retrying changes that.
		return nil
	default:
		return err
	}
}
