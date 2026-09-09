package payment

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
)

// The in-memory halves of the promo, PIN-challenge and saved-card stores.
//
// One thing these deliberately do NOT prove: concurrency. fakeStore.InTx holds
// txMu for the whole transaction body, so every transaction is serialised by
// construction and a "100 goroutines fight over the last redemption" test
// against this fake would pass without the row lock existing at all. That test
// lives in integration_test.go against real Postgres, which is the only place
// it means anything.
//
// What these are for is the rules: the arithmetic, the state machine, the
// per-user cap, the default-card invariant. Those are the same whether one
// goroutine or a hundred are running.

// promoState is the extra state the client-surface fakes need. It hangs off
// fakeStore rather than being embedded so fake_store_test.go stays the file it
// already was.
type promoState struct {
	codes       map[uuid.UUID]PromoCode
	redemptions map[uuid.UUID]PromoRedemption
	challenges  map[uuid.UUID]PINChallenge
	customers   map[string]PaymentCustomer // patientID|provider
	methods     map[uuid.UUID]PaymentMethod
}

func newPromoState() *promoState {
	return &promoState{
		codes:       map[uuid.UUID]PromoCode{},
		redemptions: map[uuid.UUID]PromoRedemption{},
		challenges:  map[uuid.UUID]PINChallenge{},
		customers:   map[string]PaymentCustomer{},
		methods:     map[uuid.UUID]PaymentMethod{},
	}
}

func (f *fakeStore) client() *promoState {
	if f.extra == nil {
		f.extra = newPromoState()
	}
	return f.extra
}

// --- Store: promotions ------------------------------------------------------

func (f *fakeStore) ExpiredReservationIDs(_ context.Context, before time.Time, limit int) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []uuid.UUID
	for id, r := range f.client().redemptions {
		if r.Status == PromoReserved && !r.ExpiresAt.After(before) {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStore) GetPromoCodeByCode(_ context.Context, code string) (PromoCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.client().codes {
		if c.Code == code && c.DeletedAt == nil {
			return c, nil
		}
	}
	return PromoCode{}, ErrNotFound
}

func (f *fakeStore) ListPromoCodes(_ context.Context, includeInactive bool, pg Page) ([]PromoCode, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []PromoCode
	for _, c := range f.client().codes {
		if c.DeletedAt != nil || (!includeInactive && !c.Active) {
			continue
		}
		all = append(all, c)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Code < all[j].Code })
	total := int64(len(all))
	if pg.Offset >= len(all) {
		return []PromoCode{}, total, nil
	}
	end := pg.Offset + pg.PerPage
	if pg.PerPage <= 0 || end > len(all) {
		end = len(all)
	}
	return all[pg.Offset:end], total, nil
}

// --- Store: PIN challenges ---------------------------------------------------

func (f *fakeStore) LatestPINChallenge(_ context.Context, paymentID uuid.UUID) (PINChallenge, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var best PINChallenge
	found := false
	for _, c := range f.client().challenges {
		if c.PaymentID != paymentID {
			continue
		}
		if !found || c.CreatedAt.After(best.CreatedAt) {
			best, found = c, true
		}
	}
	return best, found, nil
}

func (f *fakeStore) ExpiredPINChallengeIDs(_ context.Context, before time.Time, limit int) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []uuid.UUID
	for id, c := range f.client().challenges {
		if c.Status == PINPending && !c.ExpiresAt.After(before) {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// --- Store: saved payment methods --------------------------------------------

func customerKey(patientID uuid.UUID, provider ProviderName) string {
	return patientID.String() + "|" + string(provider)
}

func (f *fakeStore) GetPaymentCustomer(_ context.Context, patientID uuid.UUID, provider ProviderName) (PaymentCustomer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.client().customers[customerKey(patientID, provider)]
	if !ok {
		return PaymentCustomer{}, ErrNotFound
	}
	return c, nil
}

func (f *fakeStore) ListPaymentMethods(_ context.Context, patientID uuid.UUID) ([]PaymentMethod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []PaymentMethod{}
	for _, m := range f.client().methods {
		if m.PatientID == patientID && m.DeletedAt == nil {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDefault != out[j].IsDefault {
			return out[i].IsDefault
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

func (f *fakeStore) GetPaymentMethod(_ context.Context, id uuid.UUID) (PaymentMethod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.client().methods[id]
	if !ok || m.DeletedAt != nil {
		return PaymentMethod{}, ErrNotFound
	}
	return m, nil
}

func (f *fakeStore) FindPaymentMethodByToken(_ context.Context, provider ProviderName, token string) (PaymentMethod, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.client().methods {
		if m.Provider == provider && m.ProviderToken == token && m.DeletedAt == nil {
			return m, true, nil
		}
	}
	return PaymentMethod{}, false, nil
}

// --- Tx: promotions -----------------------------------------------------------

func (t *fakeTx) FindPromoCode(ctx context.Context, code string) (PromoCode, error) {
	return t.store.GetPromoCodeByCode(ctx, code)
}

func (t *fakeTx) LockPromoCodeByID(_ context.Context, id uuid.UUID) (PromoCode, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	c, ok := t.store.client().codes[id]
	if !ok || c.DeletedAt != nil {
		return PromoCode{}, ErrNotFound
	}
	return c, nil
}

func (t *fakeTx) AdjustRedemptionCount(_ context.Context, id uuid.UUID, delta int) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	c, ok := t.store.client().codes[id]
	if !ok {
		return ErrNotFound
	}
	c.RedemptionCount += delta
	// Mirror the promo_codes_budget and non-negative CHECKs, so a Go-side bug
	// the database would have refused is refused here too.
	if c.RedemptionCount < 0 {
		return fmt.Errorf("fakeStore: redemption_count went negative for %s", c.Code)
	}
	if c.MaxRedemptions != nil && c.RedemptionCount > *c.MaxRedemptions {
		return fmt.Errorf("fakeStore: promo_codes_budget violated for %s", c.Code)
	}
	c.Version++
	t.store.client().codes[id] = c
	return nil
}

func (t *fakeTx) CountLiveRedemptions(_ context.Context, codeID, userID uuid.UUID) (int, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	n := 0
	for _, r := range t.store.client().redemptions {
		if r.PromoCodeID == codeID && r.UserID == userID && r.Status != PromoReleased {
			n++
		}
	}
	return n, nil
}

func (t *fakeTx) ExpireStaleReservations(ctx context.Context, codeID uuid.UUID, now time.Time) (int, error) {
	t.store.mu.Lock()
	var stale []uuid.UUID
	for id, r := range t.store.client().redemptions {
		if r.PromoCodeID == codeID && r.Status == PromoReserved && !r.ExpiresAt.After(now) {
			stale = append(stale, id)
		}
	}
	for _, id := range stale {
		r := t.store.client().redemptions[id]
		r.Status = PromoReleased
		r.ReleasedAt = &now
		r.ReleaseReason = ReleaseExpired
		r.Version++
		t.store.client().redemptions[id] = r
	}
	t.store.mu.Unlock()

	if len(stale) == 0 {
		return 0, nil
	}
	if err := t.AdjustRedemptionCount(ctx, codeID, -len(stale)); err != nil {
		return 0, err
	}
	return len(stale), nil
}

func (t *fakeTx) InsertRedemption(_ context.Context, r *PromoRedemption) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	// Mirror idx_promo_redemptions_payment_live: at most one live hold per
	// payment. Without this the fake would allow a stack of codes the
	// database refuses.
	for _, existing := range t.store.client().redemptions {
		if existing.PaymentID == r.PaymentID && existing.Status != PromoReleased {
			return fmt.Errorf("%w: payment %s already holds a promo code", ErrDuplicate, r.PaymentID)
		}
	}
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	r.CreatedAt = time.Now().UTC()
	r.UpdatedAt = r.CreatedAt
	r.Version = 1
	t.store.client().redemptions[r.ID] = *r
	return nil
}

func (t *fakeTx) FindLiveRedemptionForPayment(_ context.Context, paymentID uuid.UUID) (PromoRedemption, bool, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	for _, r := range t.store.client().redemptions {
		if r.PaymentID == paymentID && r.Status != PromoReleased {
			return r, true, nil
		}
	}
	return PromoRedemption{}, false, nil
}

func (t *fakeTx) LockRedemption(_ context.Context, id uuid.UUID) (PromoRedemption, bool, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	r, ok := t.store.client().redemptions[id]
	return r, ok, nil
}

func (t *fakeTx) SetRedemptionStatus(_ context.Context, id uuid.UUID, status PromoRedemptionStatus, reason string, at time.Time) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	r, ok := t.store.client().redemptions[id]
	if !ok {
		return ErrNotFound
	}
	r.Status = status
	switch status {
	case PromoConsumed:
		r.ConsumedAt = &at
	case PromoReleased:
		r.ReleasedAt = &at
		r.ReleaseReason = reason
	case PromoReserved:
	}
	r.Version++
	t.store.client().redemptions[id] = r
	return nil
}

func (t *fakeTx) TouchRedemption(_ context.Context, id uuid.UUID, expiresAt time.Time) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	r, ok := t.store.client().redemptions[id]
	if !ok || r.Status != PromoReserved {
		return nil
	}
	r.ExpiresAt = expiresAt
	r.Version++
	t.store.client().redemptions[id] = r
	return nil
}

func (t *fakeTx) InsertPromoCode(_ context.Context, c *PromoCode) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	for _, existing := range t.store.client().codes {
		if existing.Code == c.Code {
			return fmt.Errorf("%w: promo code %s", ErrDuplicate, c.Code)
		}
	}
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	c.CreatedAt = t.store.now()
	c.UpdatedAt = c.CreatedAt
	c.Version = 1
	t.store.client().codes[c.ID] = *c
	return nil
}

func (t *fakeTx) DeactivatePromoCode(_ context.Context, code string) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	for id, c := range t.store.client().codes {
		if c.Code == code && c.DeletedAt == nil {
			c.Active = false
			c.Version++
			t.store.client().codes[id] = c
			return nil
		}
	}
	return ErrNotFound
}

// --- Tx: PIN challenges --------------------------------------------------------

func (t *fakeTx) InsertPINChallenge(_ context.Context, c *PINChallenge) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	// Mirror idx_pin_challenges_payment_pending.
	for _, existing := range t.store.client().challenges {
		if existing.PaymentID == c.PaymentID && existing.Status == PINPending {
			return fmt.Errorf("%w: payment %s already has a pending PIN challenge", ErrDuplicate, c.PaymentID)
		}
	}
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	c.CreatedAt = t.store.now()
	c.UpdatedAt = c.CreatedAt
	c.Version = 1
	t.store.client().challenges[c.ID] = *c
	return nil
}

// CountPINChallengesSince mirrors the SQL: every challenge row ever opened
// against this payment inside the window, regardless of its current status --
// a superseded one still cost an SMS and still reset the attempt counter.
func (t *fakeTx) CountPINChallengesSince(_ context.Context, paymentID uuid.UUID, since time.Time) (int, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	n := 0
	for _, c := range t.store.client().challenges {
		if c.PaymentID == paymentID && !c.CreatedAt.Before(since) {
			n++
		}
	}
	return n, nil
}

func (t *fakeTx) SupersedePendingPINChallenges(_ context.Context, paymentID uuid.UUID, at time.Time) (int, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	n := 0
	for id, c := range t.store.client().challenges {
		if c.PaymentID == paymentID && c.Status == PINPending {
			c.Status = PINSuperseded
			c.SettledAt = &at
			c.FailureReason = "a newer PIN was requested"
			c.Version++
			t.store.client().challenges[id] = c
			n++
		}
	}
	return n, nil
}

func (t *fakeTx) LockPendingPINChallenge(_ context.Context, paymentID uuid.UUID) (PINChallenge, bool, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	for _, c := range t.store.client().challenges {
		if c.PaymentID == paymentID && c.Status == PINPending {
			return c, true, nil
		}
	}
	return PINChallenge{}, false, nil
}

func (t *fakeTx) LockPINChallenge(_ context.Context, id uuid.UUID) (PINChallenge, bool, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	c, ok := t.store.client().challenges[id]
	return c, ok, nil
}

func (t *fakeTx) UpdatePINChallenge(_ context.Context, c *PINChallenge) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	cur, ok := t.store.client().challenges[c.ID]
	if !ok {
		return ErrNotFound
	}
	if cur.Version != c.Version {
		return fmt.Errorf("%w: pin challenge %s", ErrVersionConflict, c.ID)
	}
	if c.Attempts > c.MaxAttempts {
		return fmt.Errorf("fakeStore: pin_challenges_attempts_bounded violated for %s", c.ID)
	}
	c.Version = cur.Version + 1
	c.UpdatedAt = time.Now().UTC()
	c.CreatedAt = cur.CreatedAt
	t.store.client().challenges[c.ID] = *c
	return nil
}

// --- Tx: saved payment methods ---------------------------------------------------

func (t *fakeTx) InsertPaymentCustomer(_ context.Context, c *PaymentCustomer) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	key := customerKey(c.PatientID, c.Provider)
	if _, exists := t.store.client().customers[key]; exists {
		return fmt.Errorf("%w: customer handle", ErrDuplicate)
	}
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	c.CreatedAt = t.store.now()
	c.UpdatedAt = c.CreatedAt
	c.Version = 1
	t.store.client().customers[key] = *c
	return nil
}

func (t *fakeTx) InsertPaymentMethod(_ context.Context, m *PaymentMethod) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	for _, existing := range t.store.client().methods {
		if existing.Provider == m.Provider && existing.ProviderToken == m.ProviderToken && existing.DeletedAt == nil {
			return fmt.Errorf("%w: this card is already saved", ErrDuplicate)
		}
	}
	// Mirror idx_payment_methods_one_default.
	if m.IsDefault {
		for _, existing := range t.store.client().methods {
			if existing.PatientID == m.PatientID && existing.IsDefault && existing.DeletedAt == nil {
				return fmt.Errorf("fakeStore: idx_payment_methods_one_default violated for %s", m.PatientID)
			}
		}
	}
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	m.CreatedAt = time.Now().UTC()
	m.UpdatedAt = m.CreatedAt
	m.Version = 1
	t.store.client().methods[m.ID] = *m
	return nil
}

func (t *fakeTx) LockPaymentMethod(_ context.Context, id uuid.UUID) (PaymentMethod, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	m, ok := t.store.client().methods[id]
	if !ok || m.DeletedAt != nil {
		return PaymentMethod{}, ErrNotFound
	}
	return m, nil
}

func (t *fakeTx) UpdatePaymentMethod(_ context.Context, m *PaymentMethod) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	cur, ok := t.store.client().methods[m.ID]
	if !ok || cur.DeletedAt != nil {
		return ErrNotFound
	}
	if cur.Version != m.Version {
		return fmt.Errorf("%w: payment method %s", ErrVersionConflict, m.ID)
	}
	if m.IsDefault {
		for id, existing := range t.store.client().methods {
			if id != m.ID && existing.PatientID == m.PatientID && existing.IsDefault && existing.DeletedAt == nil {
				return fmt.Errorf("fakeStore: idx_payment_methods_one_default violated for %s", m.PatientID)
			}
		}
	}
	m.Version = cur.Version + 1
	m.UpdatedAt = time.Now().UTC()
	m.CreatedAt = cur.CreatedAt
	m.ProviderToken = cur.ProviderToken // the token is never rewritten
	t.store.client().methods[m.ID] = *m
	return nil
}

func (t *fakeTx) CountPaymentMethods(_ context.Context, patientID uuid.UUID) (int, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	n := 0
	for _, m := range t.store.client().methods {
		if m.PatientID == patientID && m.DeletedAt == nil {
			n++
		}
	}
	return n, nil
}

func (t *fakeTx) ClearDefaultPaymentMethod(_ context.Context, patientID uuid.UUID) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	for id, m := range t.store.client().methods {
		if m.PatientID == patientID && m.IsDefault && m.DeletedAt == nil {
			m.IsDefault = false
			m.Version++
			t.store.client().methods[id] = m
		}
	}
	return nil
}

func (t *fakeTx) SoftDeletePaymentMethod(_ context.Context, id uuid.UUID, at time.Time) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	m, ok := t.store.client().methods[id]
	if !ok || m.DeletedAt != nil {
		return ErrNotFound
	}
	m.DeletedAt = &at
	m.IsDefault = false
	m.Version++
	t.store.client().methods[id] = m
	return nil
}

func (t *fakeTx) PromoteOldestPaymentMethod(_ context.Context, patientID uuid.UUID) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	var oldest *PaymentMethod
	for id := range t.store.client().methods {
		m := t.store.client().methods[id]
		if m.PatientID != patientID || m.DeletedAt != nil {
			continue
		}
		if oldest == nil || m.CreatedAt.Before(oldest.CreatedAt) ||
			(m.CreatedAt.Equal(oldest.CreatedAt) && m.ID.String() < oldest.ID.String()) {
			cp := m
			oldest = &cp
		}
	}
	if oldest == nil {
		return nil
	}
	oldest.IsDefault = true
	oldest.Version++
	t.store.client().methods[oldest.ID] = *oldest
	return nil
}

// clone deep-copies the client-surface collections so fakeStore.InTx can roll
// them back with everything else.
func (p *promoState) clone() *promoState {
	out := newPromoState()
	for k, v := range p.codes {
		out.codes[k] = v
	}
	for k, v := range p.redemptions {
		out.redemptions[k] = v
	}
	for k, v := range p.challenges {
		out.challenges[k] = v
	}
	for k, v := range p.customers {
		out.customers[k] = v
	}
	for k, v := range p.methods {
		out.methods[k] = v
	}
	return out
}

// FindPaymentMethodByToken on the transaction is the same read as the Store
// one; the real repository's differs only in taking FOR UPDATE, which the
// fake models by serialising transactions wholesale.
func (t *fakeTx) FindPaymentMethodByToken(ctx context.Context, provider ProviderName, token string) (PaymentMethod, bool, error) {
	return t.store.FindPaymentMethodByToken(ctx, provider, token)
}
