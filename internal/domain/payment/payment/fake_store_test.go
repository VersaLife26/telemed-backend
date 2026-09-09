package payment

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"telemed/internal/platform/events"
)

// fakeStore is an in-memory Store with real transaction semantics.
//
// It is not a set of no-op stubs. InTx snapshots every collection before
// running the body and restores the snapshot on error, so a rolled-back
// transaction genuinely leaves no trace -- which is the property the webhook
// idempotency and payout re-run tests actually depend on. A fake that
// committed partial work would make those tests pass for the wrong reason.
type fakeStore struct {
	// txMu serialises transactions, standing in for Postgres row locks.
	// mu protects the maps and is held only for the duration of one operation,
	// so a read issued from inside a transaction body cannot deadlock against
	// the transaction that issued it.
	txMu sync.Mutex
	mu   sync.Mutex

	payments map[uuid.UUID]Payment
	webhooks map[string]WebhookEventRecord // keyed provider|event_id, mirroring the unique index
	refunds  map[uuid.UUID]Refund
	payouts  map[uuid.UUID]Payout
	rules    map[uuid.UUID]CommissionRule
	ledger   []LedgerEntry
	outbox   []outboxRecord

	// extra holds the promo, PIN-challenge and saved-card collections. It
	// lives in fake_store_client_test.go so this file stays the one it was.
	extra *promoState

	// txDepth guards against a nested InTx, which the real repository would
	// deadlock on and which is therefore a bug worth catching in tests.
	txDepth int

	// clock stands in for the database's NOW(), which is what stamps
	// created_at on the real tables. A test that advances the service's clock
	// has to move this too, or a window measured against created_at never
	// elapses. Nil means real time.
	clock func() time.Time
}

// now is the fake's NOW().
func (f *fakeStore) now() time.Time {
	if f.clock != nil {
		return f.clock().UTC()
	}
	return time.Now().UTC()
}

type outboxRecord struct {
	Subject     events.Subject
	AggregateID string
	Payload     any
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		payments: map[uuid.UUID]Payment{},
		webhooks: map[string]WebhookEventRecord{},
		refunds:  map[uuid.UUID]Refund{},
		payouts:  map[uuid.UUID]Payout{},
		rules:    map[uuid.UUID]CommissionRule{},
		extra:    newPromoState(),
	}
}

var _ Store = (*fakeStore)(nil)

type storeSnapshot struct {
	payments map[uuid.UUID]Payment
	webhooks map[string]WebhookEventRecord
	refunds  map[uuid.UUID]Refund
	payouts  map[uuid.UUID]Payout
	ledger   []LedgerEntry
	outbox   []outboxRecord
	// extra is snapshotted for the same reason everything else is: a
	// rolled-back transaction that left a promo reservation behind would make
	// the rollback tests pass while the reservation quietly burned a code.
	extra *promoState
}

func (f *fakeStore) snapshot() storeSnapshot {
	s := storeSnapshot{
		payments: make(map[uuid.UUID]Payment, len(f.payments)),
		webhooks: make(map[string]WebhookEventRecord, len(f.webhooks)),
		refunds:  make(map[uuid.UUID]Refund, len(f.refunds)),
		payouts:  make(map[uuid.UUID]Payout, len(f.payouts)),
		ledger:   append([]LedgerEntry(nil), f.ledger...),
		outbox:   append([]outboxRecord(nil), f.outbox...),
	}
	for k, v := range f.payments {
		s.payments[k] = v
	}
	for k, v := range f.webhooks {
		s.webhooks[k] = v
	}
	for k, v := range f.refunds {
		s.refunds[k] = v
	}
	for k, v := range f.payouts {
		s.payouts[k] = v
	}
	s.extra = f.client().clone()
	return s
}

func (f *fakeStore) restore(s storeSnapshot) {
	f.payments, f.webhooks, f.refunds, f.payouts = s.payments, s.webhooks, s.refunds, s.payouts
	f.ledger, f.outbox = s.ledger, s.outbox
	f.extra = s.extra
}

// InTx runs fn with rollback-on-error semantics.
func (f *fakeStore) InTx(ctx context.Context, fn func(context.Context, Tx) error) error {
	f.txMu.Lock()
	defer f.txMu.Unlock()

	f.mu.Lock()
	if f.txDepth > 0 {
		f.mu.Unlock()
		return errors.New("fakeStore: nested transaction; the real repository would deadlock here")
	}
	f.txDepth++
	before := f.snapshot()
	f.mu.Unlock()

	err := fn(ctx, &fakeTx{store: f})

	f.mu.Lock()
	f.txDepth--
	if err != nil {
		f.restore(before)
	}
	f.mu.Unlock()
	return err
}

// --- reads -----------------------------------------------------------------

func (f *fakeStore) GetPayment(_ context.Context, id uuid.UUID) (Payment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getPaymentLocked(id)
}

func (f *fakeStore) getPaymentLocked(id uuid.UUID) (Payment, error) {
	p, ok := f.payments[id]
	if !ok {
		return Payment{}, ErrNotFound
	}
	return p, nil
}

func (f *fakeStore) GetPaymentByAppointment(_ context.Context, appointmentID uuid.UUID) (Payment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byAppointmentLocked(appointmentID)
}

func (f *fakeStore) byAppointmentLocked(appointmentID uuid.UUID) (Payment, error) {
	for _, p := range f.payments {
		if p.AppointmentID == appointmentID {
			return p, nil
		}
	}
	return Payment{}, ErrNotFound
}

func (f *fakeStore) listPayments(match func(Payment) bool, pg Page) ([]Payment, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []Payment
	for _, p := range f.payments {
		if match(p) {
			all = append(all, p)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
	total := int64(len(all))
	if pg.Offset >= len(all) {
		return []Payment{}, total, nil
	}
	end := pg.Offset + pg.PerPage
	if pg.PerPage <= 0 || end > len(all) {
		end = len(all)
	}
	return all[pg.Offset:end], total, nil
}

func (f *fakeStore) ListPaymentsForPatient(_ context.Context, id uuid.UUID, pg Page) ([]Payment, int64, error) {
	return f.listPayments(func(p Payment) bool { return p.PatientID == id }, pg)
}

func (f *fakeStore) ListPaymentsForDoctor(_ context.Context, id uuid.UUID, pg Page) ([]Payment, int64, error) {
	return f.listPayments(func(p Payment) bool { return p.DoctorID == id }, pg)
}

func (f *fakeStore) ListPaymentsForPayout(_ context.Context, payoutID uuid.UUID) ([]Payment, error) {
	items, _, err := f.listPayments(func(p Payment) bool {
		return p.PayoutID != nil && *p.PayoutID == payoutID
	}, Page{PerPage: 1000})
	return items, err
}

func (f *fakeStore) ListRefundsForPayment(_ context.Context, paymentID uuid.UUID) ([]Refund, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []Refund{}
	for _, r := range f.refunds {
		if r.PaymentID == paymentID {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (f *fakeStore) GetPayout(_ context.Context, id uuid.UUID) (Payout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.payouts[id]
	if !ok {
		return Payout{}, ErrNotFound
	}
	return p, nil
}

func (f *fakeStore) listPayouts(match func(Payout) bool, pg Page) ([]Payout, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []Payout
	for _, p := range f.payouts {
		if match(p) {
			all = append(all, p)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
	total := int64(len(all))
	if pg.Offset >= len(all) {
		return []Payout{}, total, nil
	}
	end := pg.Offset + pg.PerPage
	if pg.PerPage <= 0 || end > len(all) {
		end = len(all)
	}
	return all[pg.Offset:end], total, nil
}

func (f *fakeStore) ListPayoutsForDoctor(_ context.Context, id uuid.UUID, pg Page) ([]Payout, int64, error) {
	return f.listPayouts(func(p Payout) bool { return p.DoctorID == id }, pg)
}

func (f *fakeStore) ListAllPayouts(_ context.Context, pg Page) ([]Payout, int64, error) {
	return f.listPayouts(func(Payout) bool { return true }, pg)
}

func (f *fakeStore) UnsettledPayoutIDs(_ context.Context, limit int) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	type row struct {
		id uuid.UUID
		at time.Time
	}
	var rows []row
	for id, p := range f.payouts {
		if p.Status == PayoutPending || p.Status == PayoutProcessing {
			rows = append(rows, row{id, p.CreatedAt})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].at.Before(rows[j].at) })
	out := make([]uuid.UUID, 0, len(rows))
	for i, r := range rows {
		if limit > 0 && i >= limit {
			break
		}
		out = append(out, r.id)
	}
	return out, nil
}

func (f *fakeStore) DoctorsWithDuePayments(_ context.Context, before time.Time, limit int) ([]DoctorDue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	agg := map[uuid.UUID]*DoctorDue{}
	for _, p := range f.payments {
		if !payoutEligible(p, before) {
			continue
		}
		d, ok := agg[p.DoctorID]
		if !ok {
			d = &DoctorDue{DoctorID: p.DoctorID, Currency: p.Currency}
			agg[p.DoctorID] = d
		}
		d.AmountCents += p.NetPayoutCents()
		d.PaymentCount++
	}
	out := make([]DoctorDue, 0, len(agg))
	for _, d := range agg {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DoctorID.String() < out[j].DoctorID.String() })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// payoutEligible mirrors the SQL predicate in ClaimPaymentsForPayout exactly.
// The two must stay in step, which is what the integration test verifies.
func payoutEligible(p Payment, before time.Time) bool {
	return p.PayoutID == nil &&
		p.DeletedAt == nil &&
		(p.Status == StatusSucceeded || p.Status == StatusPartiallyRefunded) &&
		p.SucceededAt != nil &&
		p.SucceededAt.Before(before) &&
		p.NetPayoutCents() > 0
}

func (f *fakeStore) ActiveRules(context.Context) ([]CommissionRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []CommissionRule{}
	for _, r := range f.rules {
		if r.EffectiveTo == nil {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RuleKey < out[j].RuleKey })
	return out, nil
}

func (f *fakeStore) GetRule(_ context.Context, id uuid.UUID) (CommissionRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rules[id]
	if !ok {
		return CommissionRule{}, ErrNotFound
	}
	return r, nil
}

func (f *fakeStore) CountRules(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rules), nil
}

func (f *fakeStore) SeedRules(_ context.Context, rules []CommissionRule) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range rules {
		if _, exists := f.rules[r.ID]; exists {
			continue
		}
		f.rules[r.ID] = r
		n++
	}
	return n, nil
}

func (f *fakeStore) LedgerForPayment(_ context.Context, paymentID uuid.UUID) ([]LedgerEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []LedgerEntry{}
	for _, e := range f.ledger {
		if e.PaymentID != nil && *e.PaymentID == paymentID {
			out = append(out, e)
		}
	}
	return out, nil
}

// --- test helpers ----------------------------------------------------------

func (f *fakeStore) addRule(r CommissionRule) CommissionRule {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	if r.Rounding == "" {
		r.Rounding = DefaultRounding
	}
	f.rules[r.ID] = r
	return r
}

func (f *fakeStore) addPayment(p Payment) Payment {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	p.UpdatedAt = p.CreatedAt
	if p.Version == 0 {
		p.Version = 1
	}
	if p.Currency == "" {
		p.Currency = CurrencyLKR
	}
	f.payments[p.ID] = p
	return p
}

// webhookRecord returns the stored audit row for one delivery, which is where
// a refused webhook leaves its evidence.
func (f *fakeStore) webhookRecord(provider ProviderName, eventID string) (WebhookEventRecord, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.webhooks[string(provider)+"|"+eventID]
	return rec, ok
}

// ledgerTotals sums debits and credits per account across the whole ledger.
func (f *fakeStore) ledgerTotals() map[string]int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int64{}
	for _, e := range f.ledger {
		switch e.Direction {
		case Debit:
			out[e.Account] += e.AmountCents
		case Credit:
			out[e.Account] -= e.AmountCents
		}
	}
	return out
}

// ledgerGroups returns the number of distinct balanced transactions of a type.
func (f *fakeStore) ledgerGroups(entryType string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[uuid.UUID]bool{}
	for _, e := range f.ledger {
		if e.EntryType == entryType {
			seen[e.EntryID] = true
		}
	}
	return len(seen)
}

func (f *fakeStore) outboxCount(subject events.Subject) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, o := range f.outbox {
		if o.Subject == subject {
			n++
		}
	}
	return n
}

// --- Tx --------------------------------------------------------------------

type fakeTx struct{ store *fakeStore }

var _ Tx = (*fakeTx)(nil)

func (t *fakeTx) InsertPayment(_ context.Context, p *Payment) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	s := t.store
	for _, existing := range s.payments {
		if existing.AppointmentID == p.AppointmentID {
			return fmt.Errorf("%w: appointment %s", ErrDuplicate, p.AppointmentID)
		}
		if existing.IdempotencyKey == p.IdempotencyKey {
			return fmt.Errorf("%w: idempotency key", ErrDuplicate)
		}
	}
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	p.CreatedAt = time.Now().UTC()
	p.UpdatedAt = p.CreatedAt
	p.Version = 1
	s.payments[p.ID] = *p
	return nil
}

func (t *fakeTx) LockPayment(_ context.Context, id uuid.UUID) (Payment, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	return t.store.getPaymentLocked(id)
}

func (t *fakeTx) LockPaymentByIntent(_ context.Context, provider ProviderName, intentID string) (Payment, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	for _, p := range t.store.payments {
		if p.Provider == provider && p.ProviderIntentID == intentID && intentID != "" {
			return p, nil
		}
	}
	return Payment{}, ErrNotFound
}

func (t *fakeTx) LockPaymentByAppointment(_ context.Context, appointmentID uuid.UUID) (Payment, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	return t.store.byAppointmentLocked(appointmentID)
}

func (t *fakeTx) UpdatePayment(_ context.Context, p *Payment) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	cur, ok := t.store.payments[p.ID]
	if !ok {
		return ErrNotFound
	}
	if cur.Version != p.Version {
		return fmt.Errorf("%w: payment %s", ErrVersionConflict, p.ID)
	}
	// Mirror the CHECK constraint in 000002 so a Go-side bug that the database
	// would have caught is caught here too.
	if p.Status.Settled() && !p.SplitBalances() {
		return fmt.Errorf("fakeStore: payments_split_balances violated for %s", p.ID)
	}
	if p.RefundedCommissionCents+p.RefundedPayoutCents != p.RefundedCents {
		return fmt.Errorf("fakeStore: payments_refund_split violated for %s", p.ID)
	}
	p.Version = cur.Version + 1
	p.UpdatedAt = time.Now().UTC()
	p.CreatedAt = cur.CreatedAt
	t.store.payments[p.ID] = *p
	return nil
}

func (t *fakeTx) ClaimWebhookEvent(_ context.Context, rec *WebhookEventRecord) (bool, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	key := string(rec.Provider) + "|" + rec.EventID
	if _, exists := t.store.webhooks[key]; exists {
		return false, nil
	}
	if rec.ID == uuid.Nil {
		rec.ID = uuid.New()
	}
	t.store.webhooks[key] = *rec
	return true, nil
}

func (t *fakeTx) MarkWebhookProcessed(_ context.Context, id uuid.UUID, paymentID *uuid.UUID, procErr string) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	for k, rec := range t.store.webhooks {
		if rec.ID == id {
			now := time.Now().UTC()
			rec.ProcessedAt = &now
			rec.ProcessingError = procErr
			if paymentID != nil {
				rec.PaymentID = paymentID
			}
			t.store.webhooks[k] = rec
			return nil
		}
	}
	return ErrNotFound
}

func (t *fakeTx) InsertRefund(_ context.Context, r *Refund) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	for _, existing := range t.store.refunds {
		if existing.IdempotencyKey == r.IdempotencyKey {
			return fmt.Errorf("%w: refund key", ErrDuplicate)
		}
	}
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	r.CreatedAt = time.Now().UTC()
	r.UpdatedAt = r.CreatedAt
	r.Version = 1
	t.store.refunds[r.ID] = *r
	return nil
}

func (t *fakeTx) FindRefundByKey(_ context.Context, key string) (Refund, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	for _, r := range t.store.refunds {
		if r.IdempotencyKey == key {
			return r, nil
		}
	}
	return Refund{}, ErrNotFound
}

func (t *fakeTx) FindRefundByProviderID(_ context.Context, id string) (Refund, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	if id == "" {
		return Refund{}, ErrNotFound
	}
	for _, r := range t.store.refunds {
		if r.ProviderRefundID == id {
			return r, nil
		}
	}
	return Refund{}, ErrNotFound
}

func (t *fakeTx) UpdateRefund(_ context.Context, r *Refund) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	cur, ok := t.store.refunds[r.ID]
	if !ok {
		return ErrNotFound
	}
	if cur.Version != r.Version {
		return fmt.Errorf("%w: refund %s", ErrVersionConflict, r.ID)
	}
	r.Version = cur.Version + 1
	r.UpdatedAt = time.Now().UTC()
	r.CreatedAt = cur.CreatedAt
	t.store.refunds[r.ID] = *r
	return nil
}

func (t *fakeTx) InsertPayout(_ context.Context, p *Payout) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	for _, existing := range t.store.payouts {
		if existing.DoctorID == p.DoctorID &&
			existing.PeriodStart.Equal(p.PeriodStart) && existing.PeriodEnd.Equal(p.PeriodEnd) {
			return fmt.Errorf("%w: payout for doctor %s in this period", ErrDuplicate, p.DoctorID)
		}
		if existing.IdempotencyKey == p.IdempotencyKey {
			return fmt.Errorf("%w: payout key", ErrDuplicate)
		}
	}
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	p.CreatedAt = time.Now().UTC()
	p.UpdatedAt = p.CreatedAt
	p.Version = 1
	t.store.payouts[p.ID] = *p
	return nil
}

func (t *fakeTx) LockPayout(_ context.Context, id uuid.UUID) (Payout, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	p, ok := t.store.payouts[id]
	if !ok {
		return Payout{}, ErrNotFound
	}
	return p, nil
}

func (t *fakeTx) FindPayoutByPeriod(_ context.Context, doctorID uuid.UUID, start, end time.Time) (Payout, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	for _, p := range t.store.payouts {
		if p.DoctorID == doctorID && p.PeriodStart.Equal(start) && p.PeriodEnd.Equal(end) {
			return p, nil
		}
	}
	return Payout{}, ErrNotFound
}

func (t *fakeTx) UpdatePayout(_ context.Context, p *Payout) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	cur, ok := t.store.payouts[p.ID]
	if !ok {
		return ErrNotFound
	}
	if cur.Version != p.Version {
		return fmt.Errorf("%w: payout %s", ErrVersionConflict, p.ID)
	}
	p.Version = cur.Version + 1
	p.UpdatedAt = time.Now().UTC()
	p.CreatedAt = cur.CreatedAt
	t.store.payouts[p.ID] = *p
	return nil
}

func (t *fakeTx) ClaimPaymentsForPayout(_ context.Context, payoutID, doctorID uuid.UUID, before time.Time) (int, int64, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	var count int
	var total int64
	for id, p := range t.store.payments {
		if p.DoctorID != doctorID || !payoutEligible(p, before) {
			continue
		}
		claimed := payoutID
		p.PayoutID = &claimed
		p.Version++
		t.store.payments[id] = p
		count++
		total += p.NetPayoutCents()
	}
	return count, total, nil
}

func (t *fakeTx) ReleasePaymentsFromPayout(_ context.Context, payoutID uuid.UUID) (int, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	n := 0
	for id, p := range t.store.payments {
		if p.PayoutID != nil && *p.PayoutID == payoutID {
			p.PayoutID = nil
			p.Version++
			t.store.payments[id] = p
			n++
		}
	}
	return n, nil
}

func (t *fakeTx) WriteLedger(_ context.Context, lt LedgerTransaction) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	if !lt.Balanced() {
		return fmt.Errorf("fakeStore: refusing unbalanced ledger transaction %s", lt.EntryID)
	}
	for _, leg := range lt.Legs {
		leg.EntryID = lt.EntryID
		leg.EntryType = lt.EntryType
		leg.ID = int64(len(t.store.ledger) + 1)
		t.store.ledger = append(t.store.ledger, leg)
	}
	return nil
}

func (t *fakeTx) Enqueue(_ context.Context, subject events.Subject, aggregateID string, payload any) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	t.store.outbox = append(t.store.outbox, outboxRecord{Subject: subject, AggregateID: aggregateID, Payload: payload})
	return nil
}

// uuidNil is a small helper so tests can name the zero UUID readably.
func uuidNil() uuid.UUID { return uuid.Nil }
