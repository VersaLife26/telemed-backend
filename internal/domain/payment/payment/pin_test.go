package payment

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Carrier-billing PIN challenges.
//
// The behaviour under test is everything the old single-call ConfirmPIN did
// not have: a durable expiry, a bounded attempt count, and the distinction
// between "wrong PIN, try again" and "that is enough, the payment has failed".

// testPINProvider is an in-package carrier rail.
//
// It cannot reuse internal/payment/provider/mock, because that package imports
// this one. It is a real enough rail for the state machine: it honours a
// correct PIN, rejects a wrong one with ErrProviderRejected, and can be told
// to be unreachable so the "never charge a patient for our timeout" branch is
// testable.
type testPINProvider struct {
	*testProvider

	mu           sync.Mutex
	correctPIN   string
	unavailable  bool
	confirmCalls int
	sendCalls    int
	expiresIn    time.Duration
	// now is the harness clock, shared with the service. A rail that stamped
	// its PIN expiry from the wall clock while the service ran on a pinned
	// one would produce challenges that are already dead, which says nothing
	// about the code under test.
	now func() time.Time
}

func newTestPINProvider(now func() time.Time) *testPINProvider {
	base := newTestProvider(ProviderDialog)
	base.intent = IntentResult{Status: IntentRequiresPIN}
	return &testPINProvider{
		testProvider: base, correctPIN: "4321", expiresIn: 5 * time.Minute, now: now,
	}
}

// CreateIntent mirrors the real Dialog rail: it sends a PIN and reports
// requires_pin, never succeeded. No money has moved and none will until the
// patient proves possession of the handset.
func (p *testPINProvider) CreateIntent(ctx context.Context, req IntentRequest) (IntentResult, error) {
	challenge, err := p.SendPIN(ctx, req)
	if err != nil {
		return IntentResult{}, err
	}
	expires := challenge.ExpiresAt
	return IntentResult{
		ProviderIntentID: challenge.Reference,
		Reference:        challenge.Reference,
		Status:           IntentRequiresPIN,
		ExpiresAt:        &expires,
	}, nil
}

func (p *testPINProvider) SendPIN(_ context.Context, req IntentRequest) (ProviderPINChallenge, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if req.PatientPhone == "" {
		return ProviderPINChallenge{}, fmt.Errorf("%w: no msisdn", ErrProviderRejected)
	}
	p.sendCalls++
	return ProviderPINChallenge{
		Reference:    "ref_" + uuid.NewString(),
		ExpiresAt:    p.now().Add(p.expiresIn).UTC(),
		MaskedMSISDN: "+9477***4567",
	}, nil
}

func (p *testPINProvider) ConfirmPIN(_ context.Context, req PINConfirmation) (IntentResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.confirmCalls++
	if p.unavailable {
		return IntentResult{}, fmt.Errorf("%w: carrier timed out", ErrProviderUnavailable)
	}
	if req.PIN != p.correctPIN {
		return IntentResult{}, fmt.Errorf("%w: E1313", ErrProviderRejected)
	}
	return IntentResult{ProviderIntentID: "trx_" + uuid.NewString(), Status: IntentSucceeded}, nil
}

func (p *testPINProvider) setUnavailable(v bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unavailable = v
}

func (p *testPINProvider) confirms() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.confirmCalls
}

// pinHarness wires the service to the carrier rail.
type pinHarness struct {
	store *fakeStore
	rail  *testPINProvider
	svc   *Service
	now   time.Time
	rule  CommissionRule

	// clock is the single source of "now" for the service and the rail alike,
	// so advancing it moves both.
	clock time.Time
}

// advance moves the harness clock. Both the service and the carrier rail read
// through it.
func (h *pinHarness) advance(d time.Duration) { h.clock = h.clock.Add(d) }

func newPINHarness(t *testing.T) *pinHarness {
	t.Helper()

	store := newFakeStore()
	rule := store.addRule(CommissionRule{
		RuleKey: "default", Version: 1, Scope: ScopeDefault,
		RateBps: 2000, ProviderFeeBps: 300, Rounding: RoundHalfUp,
		EffectiveFrom: time.Now().Add(-time.Hour),
	})

	now := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	h := &pinHarness{store: store, now: now, clock: now, rule: rule}

	// The fake store's created_at stamps follow the harness clock too, exactly
	// as the real table's DEFAULT NOW() follows real time.
	store.clock = func() time.Time { return h.clock }

	rail := newTestPINProvider(func() time.Time { return h.clock })
	reg := NewRegistry(ProviderDialog)
	reg.Register(rail)

	pricer := NewPricer(store, time.Minute, CommissionRule{})
	require.NoError(t, pricer.Refresh(context.Background()))

	svc := NewService(store, pricer, reg, zerolog.Nop(), Config{
		DefaultProvider: ProviderDialog,
		Currency:        CurrencyLKR,
		PIN:             PINConfig{TTL: 5 * time.Minute, MaxAttempts: 3},
	}).WithClock(func() time.Time { return h.clock })

	h.rail, h.svc = rail, svc
	return h
}

func (h *pinHarness) payment(t *testing.T, amountCents int64) Payment {
	t.Helper()
	return h.store.addPayment(Payment{
		ID:               uuid.New(),
		AppointmentID:    uuid.New(),
		PatientID:        uuid.New(),
		DoctorID:         uuid.New(),
		AmountCents:      amountCents,
		GrossAmountCents: amountCents,
		Currency:         CurrencyLKR,
		Provider:         ProviderDialog,
		Status:           StatusPending,
		IdempotencyKey:   "appointment:" + uuid.NewString(),
	})
}

func (h *pinHarness) request(t *testing.T, p Payment) PINChallengeView {
	t.Helper()
	view, err := h.svc.RequestPIN(context.Background(), RequestPINInput{
		AppointmentID: p.AppointmentID, CallerID: p.PatientID, Phone: "+94771234567",
	})
	require.NoError(t, err)
	return view
}

// TestRequestPINOpensADurableChallenge. The intermediate state has to exist in
// the database with an expiry, not only as a status word on the payment.
func TestRequestPINOpensADurableChallenge(t *testing.T) {
	t.Parallel()

	h := newPINHarness(t)
	ctx := context.Background()
	p := h.payment(t, 250_000)

	view := h.request(t, p)
	assert.Equal(t, p.ID, view.PaymentID)
	assert.Equal(t, 3, view.AttemptsRemaining)
	assert.Equal(t, int64(250_000), view.AmountCents)
	assert.NotEmpty(t, view.MaskedMSISDN)
	assert.True(t, view.ExpiresAt.After(h.now), "the client must know when the PIN dies")

	stored, err := h.store.GetPayment(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusRequiresPIN, stored.Status)

	challenge, found, err := h.store.LatestPINChallenge(ctx, p.ID)
	require.NoError(t, err)
	require.True(t, found, "the challenge must be persisted, not held in a request's memory")
	assert.Equal(t, PINPending, challenge.Status)
	assert.Equal(t, 0, challenge.Attempts)
	assert.NotEmpty(t, challenge.Reference, "the debit is made against the carrier's reference")
	assert.NotContains(t, challenge.MaskedMSISDN, "1234567",
		"the subscriber's number must be stored masked and never in full")
}

// TestRequestPINSupersedesTheOutstandingChallenge. Tapping "resend" must leave
// exactly one live reference: two would mean the SMS the patient is reading
// might correspond to either.
func TestRequestPINSupersedesTheOutstandingChallenge(t *testing.T) {
	t.Parallel()

	h := newPINHarness(t)
	ctx := context.Background()
	p := h.payment(t, 250_000)

	first := h.request(t, p)
	h.advance(time.Minute)
	second := h.request(t, p)
	assert.True(t, second.ExpiresAt.After(first.ExpiresAt),
		"a resent PIN carries its own, later deadline")

	var pending int
	for _, c := range h.store.client().challenges {
		if c.PaymentID == p.ID && c.Status == PINPending {
			pending++
		}
	}
	assert.Equal(t, 1, pending, "exactly one PIN may be outstanding at a time")

	// And the newest one is the one that works.
	_, err := h.svc.ConfirmPIN(ctx, ConfirmPINInput{PaymentID: p.ID, CallerID: p.PatientID, PIN: "4321"})
	require.NoError(t, err)
}

// TestConfirmPINCapturesOnTheCorrectCode.
func TestConfirmPINCapturesOnTheCorrectCode(t *testing.T) {
	t.Parallel()

	h := newPINHarness(t)
	ctx := context.Background()
	p := h.payment(t, 250_000)
	h.request(t, p)

	out, err := h.svc.ConfirmPIN(ctx, ConfirmPINInput{PaymentID: p.ID, CallerID: p.PatientID, PIN: "4321"})
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, out.Status)
	assert.True(t, out.SplitBalances())

	challenge, _, err := h.store.LatestPINChallenge(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, PINConfirmed, challenge.Status)
	require.NotNil(t, challenge.ConfirmedAt)

	// Idempotent: a second tap on an already-settled payment is harmless.
	again, err := h.svc.ConfirmPIN(ctx, ConfirmPINInput{PaymentID: p.ID, CallerID: p.PatientID, PIN: "4321"})
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, again.Status)
}

// TestConfirmPINWrongCodeDoesNotKillThePayment is the regression test for the
// behaviour before the challenge existed: one mistyped digit mapped straight
// to StatusFailed and the patient had to rebook.
func TestConfirmPINWrongCodeDoesNotKillThePayment(t *testing.T) {
	t.Parallel()

	h := newPINHarness(t)
	ctx := context.Background()
	p := h.payment(t, 250_000)
	h.request(t, p)

	_, err := h.svc.ConfirmPIN(ctx, ConfirmPINInput{PaymentID: p.ID, CallerID: p.PatientID, PIN: "0000"})
	require.ErrorIs(t, err, ErrPINInvalid)

	stored, err := h.store.GetPayment(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusRequiresPIN, stored.Status, "a wrong digit must leave the patient on the same screen")

	challenge, _, err := h.store.LatestPINChallenge(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, PINPending, challenge.Status)
	assert.Equal(t, 1, challenge.Attempts, "the attempt must be durably counted")
	assert.Equal(t, 2, challenge.AttemptsRemaining())

	// The right code still works afterwards.
	out, err := h.svc.ConfirmPIN(ctx, ConfirmPINInput{PaymentID: p.ID, CallerID: p.PatientID, PIN: "4321"})
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, out.Status)
}

// TestConfirmPINExhaustsAttempts. Four digits is 10,000 guesses; the allowance
// has to be bounded and the payment has to end when it runs out.
func TestConfirmPINExhaustsAttempts(t *testing.T) {
	t.Parallel()

	h := newPINHarness(t)
	ctx := context.Background()
	p := h.payment(t, 250_000)
	h.request(t, p)

	for i := range 2 {
		_, err := h.svc.ConfirmPIN(ctx, ConfirmPINInput{PaymentID: p.ID, CallerID: p.PatientID, PIN: "0000"})
		require.ErrorIsf(t, err, ErrPINInvalid, "attempt %d", i+1)
	}

	_, err := h.svc.ConfirmPIN(ctx, ConfirmPINInput{PaymentID: p.ID, CallerID: p.PatientID, PIN: "0000"})
	require.ErrorIs(t, err, ErrPINAttemptsExceeded)

	stored, err := h.store.GetPayment(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusFailed, stored.Status)

	challenge, _, err := h.store.LatestPINChallenge(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, PINFailed, challenge.Status)

	// A fourth attempt must not reach the carrier at all.
	before := h.rail.confirms()
	_, err = h.svc.ConfirmPIN(ctx, ConfirmPINInput{PaymentID: p.ID, CallerID: p.PatientID, PIN: "4321"})
	require.ErrorIs(t, err, ErrPINNotOutstanding)
	assert.Equal(t, before, h.rail.confirms(),
		"once the allowance is gone the carrier must not be asked again")
}

// TestConfirmPINExpiry. A PIN the carrier has already forgotten must be
// refused here, and the payment must return to a state a new PIN can be
// requested from -- otherwise the patient is parked in requires_pin with
// nothing that can move them.
func TestConfirmPINExpiry(t *testing.T) {
	t.Parallel()

	h := newPINHarness(t)
	ctx := context.Background()
	p := h.payment(t, 250_000)
	h.request(t, p)

	h.advance(10 * time.Minute)

	before := h.rail.confirms()
	_, err := h.svc.ConfirmPIN(ctx, ConfirmPINInput{PaymentID: p.ID, CallerID: p.PatientID, PIN: "4321"})
	require.ErrorIs(t, err, ErrPINExpired)
	assert.Equal(t, before, h.rail.confirms(),
		"an expired PIN must be refused here, not sent to the carrier to be refused there")

	stored, err := h.store.GetPayment(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusPending, stored.Status)
	assert.Empty(t, stored.ProviderIntentID, "the dead challenge's handles must be cleared")
	assert.Empty(t, stored.ProviderReference)

	challenge, _, err := h.store.LatestPINChallenge(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, PINExpired, challenge.Status)

	// And a new PIN can be requested.
	view := h.request(t, p)
	assert.Equal(t, 3, view.AttemptsRemaining, "a fresh challenge starts with a fresh allowance")
}

// TestConfirmPINGivesBackTheAttemptWhenTheCarrierNeverAnswered.
//
// Counting the attempt before the network call is the safe default: a crash
// costs one guess rather than granting unlimited ones. But a carrier that
// never answered received no guess, and charging the patient for our timeout
// is not defensible -- three network blips would fail a payment on a PIN
// nobody ever got wrong.
func TestConfirmPINGivesBackTheAttemptWhenTheCarrierNeverAnswered(t *testing.T) {
	t.Parallel()

	h := newPINHarness(t)
	ctx := context.Background()
	p := h.payment(t, 250_000)
	h.request(t, p)

	h.rail.setUnavailable(true)
	for range 5 {
		_, err := h.svc.ConfirmPIN(ctx, ConfirmPINInput{PaymentID: p.ID, CallerID: p.PatientID, PIN: "4321"})
		require.ErrorIs(t, err, ErrProviderUnavailable)
	}

	challenge, _, err := h.store.LatestPINChallenge(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, challenge.Attempts, "an unreachable carrier costs the patient nothing")
	assert.Equal(t, PINPending, challenge.Status)

	h.rail.setUnavailable(false)
	out, err := h.svc.ConfirmPIN(ctx, ConfirmPINInput{PaymentID: p.ID, CallerID: p.PatientID, PIN: "4321"})
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, out.Status)
}

// TestConfirmPINWithNoChallengeIsAConflict, not a 500 and not an
// "unsupported provider": the client can act on "there is no PIN waiting".
func TestConfirmPINWithNoChallengeIsAConflict(t *testing.T) {
	t.Parallel()

	h := newPINHarness(t)
	p := h.payment(t, 250_000)

	_, err := h.svc.ConfirmPIN(context.Background(), ConfirmPINInput{
		PaymentID: p.ID, CallerID: p.PatientID, PIN: "4321",
	})
	require.ErrorIs(t, err, ErrPINNotOutstanding)
}

// TestPINOwnership. One patient must not be able to burn another's attempts.
func TestPINOwnership(t *testing.T) {
	t.Parallel()

	h := newPINHarness(t)
	ctx := context.Background()
	p := h.payment(t, 250_000)
	h.request(t, p)

	_, err := h.svc.ConfirmPIN(ctx, ConfirmPINInput{PaymentID: p.ID, CallerID: uuid.New(), PIN: "4321"})
	require.ErrorIs(t, err, ErrForbidden)

	challenge, _, err := h.store.LatestPINChallenge(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, challenge.Attempts, "a stranger's guess must not count against the owner")
}

// TestPINSweeperRetiresUnansweredChallenges. Not a correctness dependency --
// ConfirmPIN refuses an expired challenge on its own -- but it is what stops
// abandoned payments accumulating in requires_pin where an operator reads them
// as stuck.
func TestPINSweeperRetiresUnansweredChallenges(t *testing.T) {
	t.Parallel()

	h := newPINHarness(t)
	ctx := context.Background()
	p := h.payment(t, 250_000)
	h.request(t, p)

	n, err := h.svc.SweepExpiredPINChallenges(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "a live challenge must survive the sweep")

	h.advance(time.Hour)
	n, err = h.svc.SweepExpiredPINChallenges(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	stored, err := h.store.GetPayment(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusPending, stored.Status)

	challenge, _, err := h.store.LatestPINChallenge(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, PINExpired, challenge.Status)
}

// TestRequestPINNeedsAPhoneNumber. Carrier billing without an MSISDN is not a
// degraded flow, it is no flow at all, and failing here is clearer than
// failing at Ideamart.
func TestRequestPINNeedsAPhoneNumber(t *testing.T) {
	t.Parallel()

	h := newPINHarness(t)
	p := h.payment(t, 250_000)

	_, err := h.svc.RequestPIN(context.Background(), RequestPINInput{
		AppointmentID: p.AppointmentID, CallerID: p.PatientID,
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrProviderRejected))
}

// PIN_MAX_ATTEMPTS is per CHALLENGE, and openPINChallenge supersedes whatever
// is pending and inserts a fresh row with Attempts back at zero -- so a
// challenge was free to reopen. Two things followed. The three guesses against
// a 4-digit PIN multiplied by however many times the caller re-requested, and
// every request drove a real carrier SMS to a phone number taken from the
// request body: this service does not hold the patient's number, by design, so
// it cannot check the number belongs to them. At the 120/min per-principal
// rate limit that is roughly 40 SMS a minute to an arbitrary handset, at
// platform cost.
func TestRequestPIN_ResendsAreBounded(t *testing.T) {
	t.Parallel()

	h := newPINHarness(t)
	p := h.payment(t, 250_000)

	// PINConfig defaults to MaxResends=5 over an hour.
	for i := 1; i <= 5; i++ {
		_, err := h.svc.RequestPIN(context.Background(), RequestPINInput{
			AppointmentID: p.AppointmentID, CallerID: p.PatientID, Phone: "+94771234567",
		})
		require.NoErrorf(t, err, "resend %d must be allowed: a patient who mistyped their number needs one", i)
	}

	_, err := h.svc.RequestPIN(context.Background(), RequestPINInput{
		AppointmentID: p.AppointmentID, CallerID: p.PatientID, Phone: "+94771234567",
	})
	require.Error(t, err, "a sixth PIN request was accepted; each one is a carrier SMS to a caller-supplied "+
		"number and a fresh set of guesses against a 4-digit PIN")
	require.ErrorIs(t, err, ErrPINResendLimit)

	// The window is real: past it, the patient is not locked out for ever.
	h.advance(2 * time.Hour)
	_, err = h.svc.RequestPIN(context.Background(), RequestPINInput{
		AppointmentID: p.AppointmentID, CallerID: p.PatientID, Phone: "+94771234567",
	})
	require.NoError(t, err, "the resend bound is a window, not a permanent lockout")
}
