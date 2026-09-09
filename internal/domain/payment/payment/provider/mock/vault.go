package mockprovider

import (
	"context"
	"strings"
	"sync"

	"github.com/google/uuid"

	"telemed/internal/domain/payment/payment"
)

// The mock rail's card vault.
//
// It exists so the saved-card flow can be driven end to end with no Stripe
// account: `make run` with PAYMENT_ENABLE_MOCK gives a developer a working
// setup-intent, a token, a card in the list, and a working delete.
//
// It is still a real implementation of the interface, in the same spirit as
// the rest of this package: the setup intent has to be *completed* before
// ConfirmSetup will hand back a token, so the "the patient closed the sheet
// without finishing" branch is reachable locally instead of only in
// production. And there is no card number anywhere in it, for the same reason
// there is none in the schema: this package would be the easiest place on the
// platform to leave one lying around.

type mockSetup struct {
	patientID  uuid.UUID
	customerID string
	token      string
	succeeded  bool
}

type vaultState struct {
	mu      sync.Mutex
	setups  map[string]*mockSetup
	methods map[string]payment.VaultMethod
	// byPatient makes EnsureCustomer idempotent, exactly as a real rail's
	// idempotency key does.
	byPatient map[uuid.UUID]string
}

func newVaultState() *vaultState {
	return &vaultState{
		setups:    map[string]*mockSetup{},
		methods:   map[string]payment.VaultMethod{},
		byPatient: map[uuid.UUID]string{},
	}
}

var _ payment.VaultProvider = (*Provider)(nil)

// EnsureCustomer returns a stable handle per patient.
func (p *Provider) EnsureCustomer(_ context.Context, req payment.VaultCustomerRequest) (string, error) {
	v := p.vault()
	v.mu.Lock()
	defer v.mu.Unlock()

	if id, ok := v.byPatient[req.PatientID]; ok {
		return id, nil
	}
	id := "cus_mock_" + strings.ReplaceAll(req.PatientID.String(), "-", "")[:20]
	v.byPatient[req.PatientID] = id
	return id, nil
}

// CreateSetupIntent opens a setup that CompleteSetup can later finish.
func (p *Provider) CreateSetupIntent(_ context.Context, req payment.VaultSetupRequest) (payment.VaultSetupResult, error) {
	v := p.vault()
	v.mu.Lock()
	defer v.mu.Unlock()

	// Idempotent on the key, like the real rail: a retried call returns the
	// same setup rather than leaving a trail of abandoned ones.
	for id, s := range v.setups {
		if s.patientID == req.PatientID && !s.succeeded {
			return payment.VaultSetupResult{
				SetupIntentID: id,
				ClientSecret:  id + "_secret",
				EphemeralKey:  "ek_mock_" + id,
			}, nil
		}
	}

	id := "seti_mock_" + uuid.NewString()
	v.setups[id] = &mockSetup{patientID: req.PatientID, customerID: req.ProviderCustomerID}
	return payment.VaultSetupResult{
		SetupIntentID: id,
		ClientSecret:  id + "_secret",
		EphemeralKey:  "ek_mock_" + id,
	}, nil
}

// CompleteSetup stands in for the patient confirming the sheet on their
// handset. Local tooling and tests call it; nothing in the service does.
func (p *Provider) CompleteSetup(setupIntentID string, m payment.VaultMethod) (string, bool) {
	v := p.vault()
	v.mu.Lock()
	defer v.mu.Unlock()

	s, ok := v.setups[setupIntentID]
	if !ok {
		return "", false
	}
	token := "pm_mock_" + uuid.NewString()
	s.token = token
	s.succeeded = true
	v.methods[token] = m
	return token, true
}

// GetSetupIntent reports whether the patient finished, and with what.
func (p *Provider) GetSetupIntent(_ context.Context, setupIntentID string) (payment.VaultSetupStatus, error) {
	v := p.vault()
	v.mu.Lock()
	defer v.mu.Unlock()

	s, ok := v.setups[setupIntentID]
	if !ok {
		return payment.VaultSetupStatus{}, payment.ErrNotFound
	}
	return payment.VaultSetupStatus{
		SetupIntentID:      setupIntentID,
		Succeeded:          s.succeeded,
		ProviderToken:      s.token,
		ProviderCustomerID: s.customerID,
		PatientID:          s.patientID,
	}, nil
}

// DescribeMethod returns the display metadata recorded at completion.
func (p *Provider) DescribeMethod(_ context.Context, token string) (payment.VaultMethod, error) {
	v := p.vault()
	v.mu.Lock()
	defer v.mu.Unlock()

	m, ok := v.methods[token]
	if !ok {
		return payment.VaultMethod{}, payment.ErrNotFound
	}
	return m, nil
}

// DetachMethod makes a token unusable.
func (p *Provider) DetachMethod(_ context.Context, token string) error {
	v := p.vault()
	v.mu.Lock()
	defer v.mu.Unlock()

	if _, ok := v.methods[token]; !ok {
		return payment.ErrNotFound
	}
	delete(v.methods, token)
	return nil
}
