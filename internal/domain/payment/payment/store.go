package payment

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
)

// Store and Tx are the persistence ports the service layer depends on.
//
// They are declared here, next to the code that consumes them, rather than in
// repository.go next to the code that implements them. That is what lets the
// unit tests drive the payout job and the webhook idempotency logic against an
// in-memory fake with no Postgres in sight, while the real implementation still
// gets to use pgx transactions properly.

// ErrNotFound is returned by every lookup that finds nothing. The service maps
// it to a 404; repositories never construct HTTP errors.
var ErrNotFound = errors.New("payment: not found")

// ErrVersionConflict is returned when an optimistic-lock guarded UPDATE matched
// no rows, meaning someone else changed the row first. The caller retries the
// whole transaction or gives up; it never retries just the UPDATE.
//
// It is the platform's shared sentinel rather than a private one, so a caller
// spanning several services writes one errors.Is check instead of learning each
// repository's private error.
var ErrVersionConflict = database.ErrOptimisticLock

// ErrDuplicate is returned when a unique constraint rejected an insert. For
// webhook events this is the normal, expected path, not an error condition.
var ErrDuplicate = errors.New("payment: duplicate row")

// Page describes a slice of a list query.
type Page struct {
	Page    int
	PerPage int
	Offset  int
}

// DoctorDue is one doctor with settleable payments.
type DoctorDue struct {
	DoctorID     uuid.UUID
	AmountCents  int64
	PaymentCount int
	Currency     string
}

// Store is the non-transactional surface: reads, and the entry point into a
// transaction.
type Store interface {
	// InTx runs fn inside one database transaction. Everything the business
	// rule touches -- payment rows, ledger legs, and the outbox event that
	// announces the change -- commits together or not at all.
	InTx(ctx context.Context, fn func(context.Context, Tx) error) error

	GetPayment(ctx context.Context, id uuid.UUID) (Payment, error)
	GetPaymentByAppointment(ctx context.Context, appointmentID uuid.UUID) (Payment, error)
	ListPaymentsForPatient(ctx context.Context, patientID uuid.UUID, p Page) ([]Payment, int64, error)
	ListPaymentsForDoctor(ctx context.Context, doctorID uuid.UUID, p Page) ([]Payment, int64, error)
	ListPaymentsForPayout(ctx context.Context, payoutID uuid.UUID) ([]Payment, error)

	ListRefundsForPayment(ctx context.Context, paymentID uuid.UUID) ([]Refund, error)

	GetPayout(ctx context.Context, id uuid.UUID) (Payout, error)
	ListPayoutsForDoctor(ctx context.Context, doctorID uuid.UUID, p Page) ([]Payout, int64, error)
	ListAllPayouts(ctx context.Context, p Page) ([]Payout, int64, error)
	// UnsettledPayoutIDs returns payouts still owed a provider transfer,
	// including ones left behind by a crashed earlier run. This is what makes
	// the daily job a resume rather than a restart.
	UnsettledPayoutIDs(ctx context.Context, limit int) ([]uuid.UUID, error)
	// DoctorsWithDuePayments finds doctors holding captured, unsettled money
	// older than the hold period.
	DoctorsWithDuePayments(ctx context.Context, before time.Time, limit int) ([]DoctorDue, error)

	ActiveRules(ctx context.Context) ([]CommissionRule, error)
	GetRule(ctx context.Context, id uuid.UUID) (CommissionRule, error)
	CountRules(ctx context.Context) (int, error)
	SeedRules(ctx context.Context, rules []CommissionRule) (int, error)

	LedgerForPayment(ctx context.Context, paymentID uuid.UUID) ([]LedgerEntry, error)

	// --- promotions ------------------------------------------------------
	// ExpiredReservationIDs feeds the sweeper. It is a read of ids only, so
	// the sweeper takes one short transaction per row rather than holding a
	// lock over a whole batch.
	ExpiredReservationIDs(ctx context.Context, before time.Time, limit int) ([]uuid.UUID, error)
	ListPromoCodes(ctx context.Context, includeInactive bool, p Page) ([]PromoCode, int64, error)
	GetPromoCodeByCode(ctx context.Context, code string) (PromoCode, error)

	// --- carrier-billing PIN challenges -----------------------------------
	LatestPINChallenge(ctx context.Context, paymentID uuid.UUID) (PINChallenge, bool, error)
	ExpiredPINChallengeIDs(ctx context.Context, before time.Time, limit int) ([]uuid.UUID, error)

	// --- saved payment methods -------------------------------------------
	GetPaymentCustomer(ctx context.Context, patientID uuid.UUID, provider ProviderName) (PaymentCustomer, error)
	ListPaymentMethods(ctx context.Context, patientID uuid.UUID) ([]PaymentMethod, error)
	GetPaymentMethod(ctx context.Context, id uuid.UUID) (PaymentMethod, error)
	FindPaymentMethodByToken(ctx context.Context, provider ProviderName, token string) (PaymentMethod, bool, error)
}

// Tx is the transactional surface. Every mutation lives here, which means every
// mutation is inside a transaction by construction -- there is no way to write
// a payment row without also being able to write the outbox event beside it.
type Tx interface {
	InsertPayment(ctx context.Context, p *Payment) error
	// LockPayment selects FOR UPDATE. Every state transition takes this lock
	// first, so two concurrent webhooks for the same payment serialise instead
	// of racing.
	LockPayment(ctx context.Context, id uuid.UUID) (Payment, error)
	LockPaymentByIntent(ctx context.Context, provider ProviderName, intentID string) (Payment, error)
	LockPaymentByAppointment(ctx context.Context, appointmentID uuid.UUID) (Payment, error)
	// UpdatePayment is version-guarded; it returns ErrVersionConflict if the
	// row moved underneath us, and bumps version on success.
	UpdatePayment(ctx context.Context, p *Payment) error

	// ClaimWebhookEvent inserts the event row. It returns false when the
	// (provider, event_id) unique constraint rejected the insert, which is
	// precisely the replay case: the caller acknowledges and does nothing else.
	ClaimWebhookEvent(ctx context.Context, rec *WebhookEventRecord) (bool, error)
	MarkWebhookProcessed(ctx context.Context, id uuid.UUID, paymentID *uuid.UUID, processingErr string) error

	InsertRefund(ctx context.Context, r *Refund) error
	FindRefundByKey(ctx context.Context, idempotencyKey string) (Refund, error)
	// FindRefundByProviderID matches a rail's refund confirmation back to our
	// row, which is how a refund left pending by a crash gets finished.
	FindRefundByProviderID(ctx context.Context, providerRefundID string) (Refund, error)
	UpdateRefund(ctx context.Context, r *Refund) error

	InsertPayout(ctx context.Context, p *Payout) error
	LockPayout(ctx context.Context, id uuid.UUID) (Payout, error)
	FindPayoutByPeriod(ctx context.Context, doctorID uuid.UUID, start, end time.Time) (Payout, error)
	UpdatePayout(ctx context.Context, p *Payout) error
	// ClaimPaymentsForPayout stamps payout_id onto every eligible payment that
	// does not already carry one. The `payout_id IS NULL` predicate is the
	// exactly-once guarantee: a re-run after a crash claims nothing, because
	// the first run already claimed it.
	ClaimPaymentsForPayout(ctx context.Context, payoutID, doctorID uuid.UUID, before time.Time) (count int, totalCents int64, err error)
	// ReleasePaymentsFromPayout undoes the claim when a payout is abandoned.
	ReleasePaymentsFromPayout(ctx context.Context, payoutID uuid.UUID) (int, error)

	// WriteLedger appends a balanced set of legs. It refuses to write an
	// unbalanced transaction.
	WriteLedger(ctx context.Context, lt LedgerTransaction) error

	// --- promotions ------------------------------------------------------
	//
	// Every method here is called while the caller already holds FOR UPDATE on
	// the promo_codes row it concerns. That lock, not any individual
	// statement, is what makes "read the budget, decide, then write" correct
	// under concurrency; see the commentary at the top of promo.go.

	// FindPromoCode looks a code up by its normalised (upper-case) form.
	FindPromoCode(ctx context.Context, code string) (PromoCode, error)
	// LockPromoCodeByID takes FOR UPDATE on one code. Callers lock several in
	// id order so a pair of concurrent swaps cannot deadlock.
	LockPromoCodeByID(ctx context.Context, id uuid.UUID) (PromoCode, error)
	// AdjustRedemptionCount moves the live-redemption counter by delta. The
	// promo_codes_budget CHECK refuses an increment past the code's cap, which
	// is the backstop underneath the row lock.
	AdjustRedemptionCount(ctx context.Context, id uuid.UUID, delta int) error
	// CountLiveRedemptions is the per-user limit check.
	CountLiveRedemptions(ctx context.Context, codeID, userID uuid.UUID) (int, error)
	// ExpireStaleReservations releases this code's abandoned holds and
	// decrements the counter to match, so a limited code cannot be
	// permanently burned by patients who never paid.
	ExpireStaleReservations(ctx context.Context, codeID uuid.UUID, now time.Time) (int, error)

	InsertRedemption(ctx context.Context, r *PromoRedemption) error
	FindLiveRedemptionForPayment(ctx context.Context, paymentID uuid.UUID) (PromoRedemption, bool, error)
	LockRedemption(ctx context.Context, id uuid.UUID) (PromoRedemption, bool, error)
	SetRedemptionStatus(ctx context.Context, id uuid.UUID, status PromoRedemptionStatus, reason string, at time.Time) error
	// TouchRedemption extends a hold, so a patient still typing does not lose
	// the code they already applied.
	TouchRedemption(ctx context.Context, id uuid.UUID, expiresAt time.Time) error

	InsertPromoCode(ctx context.Context, c *PromoCode) error
	DeactivatePromoCode(ctx context.Context, code string) error

	// --- carrier-billing PIN challenges -----------------------------------
	InsertPINChallenge(ctx context.Context, c *PINChallenge) error
	// SupersedePendingPINChallenges retires whatever was outstanding, so the
	// partial unique index can hold "at most one pending challenge per
	// payment" while still allowing a resend.
	SupersedePendingPINChallenges(ctx context.Context, paymentID uuid.UUID, at time.Time) (int, error)
	CountPINChallengesSince(ctx context.Context, paymentID uuid.UUID, since time.Time) (int, error)
	LockPendingPINChallenge(ctx context.Context, paymentID uuid.UUID) (PINChallenge, bool, error)
	LockPINChallenge(ctx context.Context, id uuid.UUID) (PINChallenge, bool, error)
	UpdatePINChallenge(ctx context.Context, c *PINChallenge) error

	// --- saved payment methods -------------------------------------------
	InsertPaymentCustomer(ctx context.Context, c *PaymentCustomer) error
	InsertPaymentMethod(ctx context.Context, m *PaymentMethod) error
	LockPaymentMethod(ctx context.Context, id uuid.UUID) (PaymentMethod, error)
	UpdatePaymentMethod(ctx context.Context, m *PaymentMethod) error
	FindPaymentMethodByToken(ctx context.Context, provider ProviderName, token string) (PaymentMethod, bool, error)
	CountPaymentMethods(ctx context.Context, patientID uuid.UUID) (int, error)
	ClearDefaultPaymentMethod(ctx context.Context, patientID uuid.UUID) error
	SoftDeletePaymentMethod(ctx context.Context, id uuid.UUID, at time.Time) error
	// PromoteOldestPaymentMethod gives a patient a default again after they
	// deleted the one they had. A patient with three saved cards and no
	// default is shown "enter a card" at checkout.
	PromoteOldestPaymentMethod(ctx context.Context, patientID uuid.UUID) error

	// Enqueue writes a domain event to the outbox on this same transaction.
	// The relay publishes it after commit. Business code never calls the
	// broker directly.
	Enqueue(ctx context.Context, subject events.Subject, aggregateID string, payload any) error
}
