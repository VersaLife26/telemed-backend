package payment

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Saved payment methods.
//
// The two properties that matter most here are not features:
//
//   - nothing a client sends becomes a stored card attribute. Brand, last4 and
//     expiry all come from the rail, so a client cannot label somebody else's
//     token as its own card.
//   - exactly one default per patient, always. Two defaults means checkout
//     charges whichever row the query returned first.

// testVaultProvider is an in-package rail with a card vault.
type testVaultProvider struct {
	*testProvider

	mu       sync.Mutex
	setups   map[string]VaultSetupStatus
	methods  map[string]VaultMethod
	byOwner  map[uuid.UUID]string
	detaches int
}

func newTestVaultProvider() *testVaultProvider {
	return &testVaultProvider{
		testProvider: newTestProvider(ProviderStripe),
		setups:       map[string]VaultSetupStatus{},
		methods:      map[string]VaultMethod{},
		byOwner:      map[uuid.UUID]string{},
	}
}

func (p *testVaultProvider) EnsureCustomer(_ context.Context, req VaultCustomerRequest) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if id, ok := p.byOwner[req.PatientID]; ok {
		return id, nil
	}
	id := "cus_" + uuid.NewString()
	p.byOwner[req.PatientID] = id
	return id, nil
}

func (p *testVaultProvider) CreateSetupIntent(_ context.Context, req VaultSetupRequest) (VaultSetupResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := "seti_" + uuid.NewString()
	p.setups[id] = VaultSetupStatus{
		SetupIntentID:      id,
		ProviderCustomerID: req.ProviderCustomerID,
		PatientID:          req.PatientID,
	}
	return VaultSetupResult{SetupIntentID: id, ClientSecret: id + "_secret", EphemeralKey: "ek_" + id}, nil
}

// complete stands in for the patient confirming the sheet on their handset.
func (p *testVaultProvider) complete(setupIntentID string, m VaultMethod) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.setups[setupIntentID]
	token := "pm_" + uuid.NewString()
	s.Succeeded = true
	s.ProviderToken = token
	p.setups[setupIntentID] = s
	p.methods[token] = m
	return token
}

func (p *testVaultProvider) GetSetupIntent(_ context.Context, id string) (VaultSetupStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.setups[id]
	if !ok {
		return VaultSetupStatus{}, ErrNotFound
	}
	return s, nil
}

func (p *testVaultProvider) DescribeMethod(_ context.Context, token string) (VaultMethod, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.methods[token]
	if !ok {
		return VaultMethod{}, ErrNotFound
	}
	return m, nil
}

func (p *testVaultProvider) DetachMethod(_ context.Context, token string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.detaches++
	if _, ok := p.methods[token]; !ok {
		return ErrNotFound
	}
	delete(p.methods, token)
	return nil
}

// vaultHarness wires the service to a rail with a vault.
type vaultHarness struct {
	store *fakeStore
	rail  *testVaultProvider
	svc   *Service
	now   time.Time
}

func newVaultHarness(t *testing.T, enabled bool) *vaultHarness {
	t.Helper()

	store := newFakeStore()
	store.addRule(CommissionRule{
		RuleKey: "default", Version: 1, Scope: ScopeDefault,
		RateBps: 2000, ProviderFeeBps: 300, Rounding: RoundHalfUp,
		EffectiveFrom: time.Now().Add(-time.Hour),
	})

	rail := newTestVaultProvider()
	reg := NewRegistry(ProviderStripe)
	reg.Register(rail)

	pricer := NewPricer(store, time.Minute, CommissionRule{})
	require.NoError(t, pricer.Refresh(context.Background()))

	now := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	svc := NewService(store, pricer, reg, zerolog.Nop(), Config{
		DefaultProvider: ProviderStripe,
		Currency:        CurrencyLKR,
		Vault:           VaultConfig{Enabled: enabled, Provider: ProviderStripe},
	}).WithClock(func() time.Time { return now })

	return &vaultHarness{store: store, rail: rail, svc: svc, now: now}
}

// saveCard runs the whole flow: setup intent, patient confirms, client tells us.
func (h *vaultHarness) saveCard(t *testing.T, patientID uuid.UUID, m VaultMethod) PaymentMethod {
	t.Helper()
	ctx := context.Background()

	view, err := h.svc.StartCardSetup(ctx, patientID)
	require.NoError(t, err)
	require.NotEmpty(t, view.ClientSecret)

	h.rail.complete(view.SetupIntentID, m)

	saved, err := h.svc.ConfirmCardSetup(ctx, patientID, view.SetupIntentID)
	require.NoError(t, err)
	return saved
}

func visaCard() VaultMethod {
	return VaultMethod{Type: "card", Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: 2030}
}

// TestSaveCardStoresATokenAndNothingElse.
func TestSaveCardStoresATokenAndNothingElse(t *testing.T) {
	t.Parallel()

	h := newVaultHarness(t, true)
	patient := uuid.New()
	saved := h.saveCard(t, patient, visaCard())

	assert.Equal(t, "card", saved.MethodType)
	assert.Equal(t, "visa", saved.Brand)
	assert.Equal(t, "4242", saved.Last4)
	assert.True(t, saved.IsDefault, "a patient's first card is their default")
	assert.NotEmpty(t, saved.ProviderToken)

	// The wire shape must not carry the token or the customer handle: both are
	// credentials against the rail.
	dto := toMethodDTO(saved, h.now)
	assert.Equal(t, "4242", dto.Last4)
	assert.False(t, dto.Expired)
	body, err := marshalDTO(dto)
	require.NoError(t, err)
	assert.NotContains(t, body, saved.ProviderToken, "the provider token must never reach a client")
	assert.NotContains(t, body, saved.ProviderCustomerID, "the customer handle must never reach a client")
}

// TestSaveCardRefusesAnythingThatIsNotDisplayMetadata is the PAN guard.
//
// last4 is the only numeric card field on a saved method, so it is the only
// place a full card number could ever land. Four digits, or nothing.
func TestSaveCardRefusesAnythingThatIsNotDisplayMetadata(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		method VaultMethod
	}{
		{"a full PAN in last4", VaultMethod{Type: "card", Brand: "visa", Last4: "4242424242424242"}},
		{"non-digits in last4", VaultMethod{Type: "card", Brand: "visa", Last4: "42x2"}},
		{"a nonsense expiry month", VaultMethod{Type: "card", Last4: "4242", ExpMonth: 13}},
		{"a nonsense expiry year", VaultMethod{Type: "card", Last4: "4242", ExpYear: 1999}},
		{"no method type at all", VaultMethod{Last4: "4242"}},
		{"an invented method type", VaultMethod{Type: "cheque", Last4: "4242"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newVaultHarness(t, true)
			patient := uuid.New()
			ctx := context.Background()

			view, err := h.svc.StartCardSetup(ctx, patient)
			require.NoError(t, err)
			h.rail.complete(view.SetupIntentID, tc.method)

			_, err = h.svc.ConfirmCardSetup(ctx, patient, view.SetupIntentID)
			require.Error(t, err, "the rail reported %+v and it was stored anyway", tc.method)

			methods, err := h.svc.ListCards(ctx, patient)
			require.NoError(t, err)
			assert.Empty(t, methods, "nothing may be stored when the metadata is refused")
		})
	}
}

// TestSaveCardIsIdempotentAcrossBothRecordingPaths.
//
// The client's own confirm call and the provider's setup_intent.succeeded
// webhook race every single time. The loser must be a no-op, not a second
// card in the patient's list.
func TestSaveCardIsIdempotentAcrossBothRecordingPaths(t *testing.T) {
	t.Parallel()

	h := newVaultHarness(t, true)
	ctx := context.Background()
	patient := uuid.New()

	view, err := h.svc.StartCardSetup(ctx, patient)
	require.NoError(t, err)
	h.rail.complete(view.SetupIntentID, visaCard())

	// The client gets there first...
	first, err := h.svc.ConfirmCardSetup(ctx, patient, view.SetupIntentID)
	require.NoError(t, err)
	// ...and then the webhook lands.
	require.NoError(t, h.svc.OnSetupIntentWebhook(ctx, view.SetupIntentID))

	methods, err := h.svc.ListCards(ctx, patient)
	require.NoError(t, err)
	require.Len(t, methods, 1, "one card, however many times it is reported")
	assert.Equal(t, first.ID, methods[0].ID)
}

// TestConfirmCardSetupRefusesAnIncompleteSetup. A patient who closed the sheet
// has no token, and a row with an empty token would be a card that can never
// be charged and can never be deleted.
func TestConfirmCardSetupRefusesAnIncompleteSetup(t *testing.T) {
	t.Parallel()

	h := newVaultHarness(t, true)
	ctx := context.Background()
	patient := uuid.New()

	view, err := h.svc.StartCardSetup(ctx, patient)
	require.NoError(t, err)

	_, err = h.svc.ConfirmCardSetup(ctx, patient, view.SetupIntentID)
	require.ErrorIs(t, err, ErrSetupIncomplete)

	// The webhook path swallows the same condition rather than making the rail
	// retry forever: there is nothing to record and no retry will change that.
	require.NoError(t, h.svc.OnSetupIntentWebhook(ctx, view.SetupIntentID))
}

// TestConfirmCardSetupRefusesSomebodyElsesSetup. The setup intent's metadata,
// not the caller, is the authority on ownership -- and a caller who is not
// that owner is refused.
func TestConfirmCardSetupRefusesSomebodyElsesSetup(t *testing.T) {
	t.Parallel()

	h := newVaultHarness(t, true)
	ctx := context.Background()
	owner, thief := uuid.New(), uuid.New()

	view, err := h.svc.StartCardSetup(ctx, owner)
	require.NoError(t, err)
	h.rail.complete(view.SetupIntentID, visaCard())

	_, err = h.svc.ConfirmCardSetup(ctx, thief, view.SetupIntentID)
	require.ErrorIs(t, err, ErrMethodNotOwned)

	stolen, err := h.svc.ListCards(ctx, thief)
	require.NoError(t, err)
	assert.Empty(t, stolen)
}

// TestExactlyOneDefaultCard.
func TestExactlyOneDefaultCard(t *testing.T) {
	t.Parallel()

	h := newVaultHarness(t, true)
	ctx := context.Background()
	patient := uuid.New()

	first := h.saveCard(t, patient, visaCard())
	second := h.saveCard(t, patient, VaultMethod{Type: "card", Brand: "mastercard", Last4: "5555", ExpMonth: 6, ExpYear: 2029})

	assert.True(t, first.IsDefault)
	assert.False(t, second.IsDefault, "a second card must not silently take over as default")

	switched, err := h.svc.SetDefaultCard(ctx, patient, second.ID)
	require.NoError(t, err)
	assert.True(t, switched.IsDefault)

	methods, err := h.svc.ListCards(ctx, patient)
	require.NoError(t, err)
	defaults := 0
	for _, m := range methods {
		if m.IsDefault {
			defaults++
		}
	}
	assert.Equal(t, 1, defaults, "exactly one default, always")
	assert.Equal(t, second.ID, methods[0].ID, "the default sorts first")
}

// TestDeletingTheDefaultPromotesAnother. A patient with three saved cards and
// no default is shown "enter a card" at checkout, which is the opposite of
// what saving cards is for.
func TestDeletingTheDefaultPromotesAnother(t *testing.T) {
	t.Parallel()

	h := newVaultHarness(t, true)
	ctx := context.Background()
	patient := uuid.New()

	first := h.saveCard(t, patient, visaCard())
	h.saveCard(t, patient, VaultMethod{Type: "card", Brand: "mastercard", Last4: "5555", ExpMonth: 6, ExpYear: 2029})

	require.NoError(t, h.svc.ForgetCard(ctx, patient, first.ID))

	methods, err := h.svc.ListCards(ctx, patient)
	require.NoError(t, err)
	require.Len(t, methods, 1)
	assert.True(t, methods[0].IsDefault, "the surviving card must become the default")
	assert.Equal(t, "5555", methods[0].Last4)
}

// TestForgetCardDetachesAtTheRailFirst.
//
// Order matters. Deleting our row first and then failing the detach would
// leave a chargeable token at the provider that we can no longer even see.
// This way the worst case is a detached token we still list, which the next
// delete clears up.
func TestForgetCardDetachesAtTheRailFirst(t *testing.T) {
	t.Parallel()

	h := newVaultHarness(t, true)
	ctx := context.Background()
	patient := uuid.New()
	saved := h.saveCard(t, patient, visaCard())

	require.NoError(t, h.svc.ForgetCard(ctx, patient, saved.ID))
	_, found, err := h.store.FindPaymentMethodByToken(ctx, ProviderStripe, saved.ProviderToken)
	require.NoError(t, err)
	assert.False(t, found, "the row is soft-deleted")

	_, err = h.rail.DescribeMethod(ctx, saved.ProviderToken)
	assert.ErrorIs(t, err, ErrNotFound, "the token must be detached at the rail")

	// Deleting twice is not an error; the second call finds nothing.
	assert.Error(t, h.svc.ForgetCard(ctx, patient, saved.ID),
		"a deleted card is gone: a second delete is a 404, not a silent success")
}

// TestForgetCardRefusesSomebodyElsesCard.
func TestForgetCardRefusesSomebodyElsesCard(t *testing.T) {
	t.Parallel()

	h := newVaultHarness(t, true)
	ctx := context.Background()
	owner := uuid.New()
	saved := h.saveCard(t, owner, visaCard())

	require.ErrorIs(t, h.svc.ForgetCard(ctx, uuid.New(), saved.ID), ErrForbidden)
	require.ErrorIs(t, func() error {
		_, err := h.svc.SetDefaultCard(ctx, uuid.New(), saved.ID)
		return err
	}(), ErrForbidden)

	methods, err := h.svc.ListCards(ctx, owner)
	require.NoError(t, err)
	assert.Len(t, methods, 1, "the owner's card is untouched")
}

// TestExpiredCardIsListedAndFlagged. A card that expired last month should not
// vanish: the patient needs to see why it stopped working.
func TestExpiredCardIsListedAndFlagged(t *testing.T) {
	t.Parallel()

	h := newVaultHarness(t, true)
	patient := uuid.New()
	saved := h.saveCard(t, patient, VaultMethod{
		Type: "card", Brand: "visa", Last4: "4242", ExpMonth: 7, ExpYear: 2026,
	})

	assert.True(t, saved.Expired(h.now), "August 2026 is past a July 2026 expiry")
	assert.False(t, saved.Expired(time.Date(2026, 7, 31, 23, 59, 0, 0, time.UTC)),
		"a card is good through the last day of its expiry month")
	assert.True(t, toMethodDTO(saved, h.now).Expired)
}

// TestVaultDisabledAnswersHonestly. A deployment with no card rail must say
// "not available here", not return an empty list that looks like "you have no
// saved cards".
func TestVaultDisabledAnswersHonestly(t *testing.T) {
	t.Parallel()

	h := newVaultHarness(t, false)
	ctx := context.Background()
	patient := uuid.New()

	_, err := h.svc.StartCardSetup(ctx, patient)
	assert.ErrorIs(t, err, ErrVaultDisabled)

	_, err = h.svc.ListCards(ctx, patient)
	assert.ErrorIs(t, err, ErrVaultDisabled, "an empty list would be a lie")

	_, err = h.svc.SetDefaultCard(ctx, patient, uuid.New())
	assert.ErrorIs(t, err, ErrVaultDisabled)

	assert.ErrorIs(t, h.svc.ForgetCard(ctx, patient, uuid.New()), ErrVaultDisabled)
}

// TestCustomerHandleIsCreatedOnceAndReused. Two handles for one patient means
// half their saved cards silently invisible.
func TestCustomerHandleIsCreatedOnceAndReused(t *testing.T) {
	t.Parallel()

	h := newVaultHarness(t, true)
	ctx := context.Background()
	patient := uuid.New()

	first, err := h.svc.StartCardSetup(ctx, patient)
	require.NoError(t, err)
	second, err := h.svc.StartCardSetup(ctx, patient)
	require.NoError(t, err)

	assert.Equal(t, first.CustomerID, second.CustomerID)
	assert.NotEmpty(t, first.EphemeralKey,
		"without an ephemeral key the provider's sheet can only ever collect a new card")

	stored, err := h.store.GetPaymentCustomer(ctx, patient, ProviderStripe)
	require.NoError(t, err)
	assert.Equal(t, first.CustomerID, stored.ProviderCustomerID)
}

// marshalDTO renders a wire shape so a test can assert that a secret is not in
// it. Asserting on the JSON rather than on struct fields is deliberate: it is
// the bytes that reach the client, and a future `json:"-"` removed by accident
// would pass a field-by-field check.
func marshalDTO(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}
