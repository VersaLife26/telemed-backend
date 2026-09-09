package payment

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/platform/events"
	"telemed/internal/platform/logger"
)

// Service holds every business rule about money. It never sees an
// *http.Request and never writes SQL; handlers translate HTTP into calls on
// this type and repositories translate calls on Tx into statements.

// Config tunes the service.
type Config struct {
	// HoldPeriod is how long captured money waits before it is eligible for
	// payout. It exists so a chargeback raised the same day is caught before
	// the money has left the building.
	HoldPeriod time.Duration
	// DefaultProvider is used when a request does not name a rail.
	DefaultProvider ProviderName
	// Currency is the pricing currency for new payments.
	Currency string
	// Promo tunes how long an applied-but-unpaid promo code is held.
	Promo PromoConfig
	// PIN tunes the carrier-billing challenge window and attempt allowance.
	PIN PINConfig
	// Vault names the rail that stores tokenised payment methods.
	Vault VaultConfig
}

// Service is the payment domain service.
type Service struct {
	store     Store
	pricer    *Pricer
	providers *Registry
	log       zerolog.Logger
	cfg       Config
	promo     PromoConfig
	pin       PINConfig
	vaultCfg  VaultConfig

	// now is injected so tests can pin the clock. Production passes time.Now.
	now func() time.Time
}

// NewService builds the service.
func NewService(store Store, pricer *Pricer, providers *Registry, log zerolog.Logger, cfg Config) *Service {
	if cfg.HoldPeriod <= 0 {
		cfg.HoldPeriod = 24 * time.Hour
	}
	if cfg.Currency == "" {
		cfg.Currency = CurrencyLKR
	}
	if cfg.DefaultProvider == "" {
		cfg.DefaultProvider = providers.Default()
	}
	cfg.Promo = cfg.Promo.withDefaults()
	cfg.PIN = cfg.PIN.withDefaults()
	cfg.Vault = cfg.Vault.withDefaults()
	return &Service{
		store: store, pricer: pricer, providers: providers, log: log, cfg: cfg,
		promo: cfg.Promo, pin: cfg.PIN, vaultCfg: cfg.Vault,
		now: time.Now,
	}
}

// WithClock replaces the clock. Tests only.
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// --- errors the handler maps to HTTP ---------------------------------------

var (
	// ErrPaymentNotReady means the appointment.created event has not landed
	// yet, so there is nothing to pay for. It is a timing condition, not a
	// client mistake, and the client should retry.
	ErrPaymentNotReady = errors.New("payment: no payment exists for that appointment yet")

	// ErrAlreadySettled means the payment is past the point where this
	// operation makes sense.
	ErrAlreadySettled = errors.New("payment: already settled")

	// ErrNothingRefundable means the policy allows a refund but there is no
	// money left to return, or the policy allows none at all.
	ErrNothingRefundable = errors.New("payment: nothing left to refund")

	// ErrForbidden means the caller does not own the resource.
	ErrForbidden = errors.New("payment: caller does not own this payment")

	// ErrRefundNotSelfService means a patient asked for their own refund
	// directly. Refunds follow from cancelling the appointment; see the long
	// comment in Refund.
	ErrRefundNotSelfService = errors.New("payment: a refund follows from cancelling the appointment, it is not requested directly")
)

// --- intent creation -------------------------------------------------------

// CreateIntentInput is the request to begin collecting money.
type CreateIntentInput struct {
	AppointmentID uuid.UUID
	// CallerID is the authenticated patient. It is checked against the payment
	// so one patient cannot start a payment for another's appointment.
	CallerID    uuid.UUID
	CallerIsOps bool

	Provider ProviderName
	// PatientPhone is required for carrier billing and ignored otherwise.
	PatientPhone string
	ReturnURL    string
}

// IntentView is what the client needs to complete the payment.
type IntentView struct {
	Payment      Payment `json:"payment"`
	ClientSecret string  `json:"client_secret,omitempty"`
	RedirectURL  string  `json:"redirect_url,omitempty"`
	Reference    string  `json:"reference,omitempty"`
	// NextAction tells the client what to do: confirm_card, redirect,
	// enter_pin, or none.
	NextAction string `json:"next_action"`
	// PINChallenge is populated only when NextAction is enter_pin. It carries
	// the deadline and the remaining attempts, which is everything the
	// patient's app needs and nothing it does not -- no carrier reference,
	// and obviously no PIN.
	PINChallenge *PINChallengeView `json:"pin_challenge,omitempty"`
	// Order is the price breakdown the client shows on the confirmation
	// screen, so the discount stays visible after the promo screen is gone.
	Order OrderSummary `json:"order"`
}

// CreateIntent attaches a provider intent to an existing pending payment.
//
// The payment row itself is created by the appointment.created consumer, not
// here: the amount is scheduling's fact, not the client's. A client that could
// name its own amount could name zero.
func (s *Service) CreateIntent(ctx context.Context, in CreateIntentInput) (IntentView, error) {
	existing, err := s.store.GetPaymentByAppointment(ctx, in.AppointmentID)
	if errors.Is(err, ErrNotFound) {
		return IntentView{}, ErrPaymentNotReady
	}
	if err != nil {
		return IntentView{}, err
	}
	if !in.CallerIsOps && existing.PatientID != in.CallerID {
		return IntentView{}, ErrForbidden
	}
	if existing.Status.Settled() {
		// Already paid. Returning the payment rather than an error means a
		// client that lost the response to its first call recovers cleanly.
		return IntentView{Payment: existing, NextAction: "none", Order: summaryFor(existing, time.Time{})}, nil
	}

	// Revalidate any promotion BEFORE quoting the rail, and extend its hold
	// so it cannot lapse while the provider call is in flight. Without this a
	// reservation that expired between the patient applying the code and
	// tapping Pay would still be charged at the discounted price -- money the
	// platform collected from nobody.
	existing, err = s.refreshQuote(ctx, existing)
	if err != nil {
		return IntentView{}, err
	}
	quotedCents := existing.AmountCents

	provider := in.Provider
	if provider == "" {
		provider = existing.Provider
	}
	if provider == "" {
		provider = s.cfg.DefaultProvider
	}
	if !provider.Valid() {
		return IntentView{}, fmt.Errorf("payment: unknown provider %q", provider)
	}
	rail, err := s.providers.Get(provider)
	if err != nil {
		return IntentView{}, err
	}

	// Freeze the commission rule now, at quote time, so a rate change between
	// the patient tapping Pay and the webhook landing cannot reprice the
	// consultation underneath them.
	split, rule, err := s.pricer.Price(ctx, existing.AmountCents, PricingContext{
		Specialty:       existing.Specialty,
		CorporateClient: existing.CorporateClient,
	})
	if err != nil {
		return IntentView{}, err
	}

	// The idempotency key is derived from the payment id, so every retry of
	// this call presents the same key and the rail returns the same intent
	// instead of creating a second one.
	req := IntentRequest{
		PaymentID:      existing.ID,
		AppointmentID:  existing.AppointmentID,
		PatientID:      existing.PatientID,
		DoctorID:       existing.DoctorID,
		AmountCents:    existing.AmountCents,
		Currency:       existing.Currency,
		IdempotencyKey: existing.IdempotencyKey,
		Description:    "Telemedicine consultation " + existing.AppointmentID.String(),
		PatientPhone:   in.PatientPhone,
		ReturnURL:      in.ReturnURL,
	}

	res, err := rail.CreateIntent(ctx, req)
	if err != nil {
		return IntentView{}, err
	}

	updated := existing
	updated.Provider = provider
	updated.ProviderIntentID = res.ProviderIntentID
	updated.ProviderReference = res.Reference
	updated.Status = res.Status.Status()
	updated.CommissionCents = split.CommissionCents
	updated.ProviderFeeCents = split.ProviderFeeCents
	updated.DoctorPayoutCents = split.PayoutCents
	updated.CommissionRuleID = &rule.ID
	updated.CommissionRuleKey = rule.RuleKey
	updated.CommissionRuleVer = rule.Version

	var challenge *PINChallengeView
	err = s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		locked, err := tx.LockPayment(ctx, existing.ID)
		if err != nil {
			return err
		}
		if locked.Status.Settled() {
			updated = locked // someone else's webhook won the race; keep their truth
			return nil
		}
		if locked.AmountCents != quotedCents {
			// The price moved between the quote and this transaction -- a
			// promotion applied or released concurrently. Charging the rail's
			// figure now would mean the patient is billed one amount and shown
			// another, so abandon and let the client retry against the new
			// price.
			return fmt.Errorf("%w: the amount changed from %d to %d while the provider was quoting",
				ErrVersionConflict, quotedCents, locked.AmountCents)
		}
		updated.Version = locked.Version
		if err := tx.UpdatePayment(ctx, &updated); err != nil {
			return err
		}
		// A rail that settles synchronously (mock, and Dialog when the PIN was
		// already confirmed) needs its ledger and event written here, because
		// no webhook is coming.
		if updated.Status == StatusSucceeded {
			return s.applyCapture(ctx, tx, &updated, res.ProviderFeeCents)
		}
		// A carrier-billing rail returns requires_pin. Recording the challenge
		// here, on the same transaction, is what makes "the status says a PIN
		// is outstanding" and "we know which PIN, until when, and how many
		// tries are left" one atomic fact rather than two writes with a
		// window between them.
		if updated.Status == StatusRequiresPIN {
			c, err := s.openPINChallenge(ctx, tx, updated, res, maskPhone(in.PatientPhone), s.now().UTC())
			if err != nil {
				return err
			}
			view := toPINChallengeView(c, updated)
			challenge = &view
		}
		return nil
	})
	if err != nil {
		return IntentView{}, err
	}

	return IntentView{
		Payment:      updated,
		ClientSecret: res.ClientSecret,
		RedirectURL:  res.RedirectURL,
		Reference:    res.Reference,
		NextAction:   nextAction(updated.Status, res),
		PINChallenge: challenge,
		Order:        summaryFor(updated, time.Time{}),
	}, nil
}

// refreshQuote revalidates the promotion on a payment and returns the
// authoritative amount to quote.
//
// It is the guard that makes the lazy expiry in promo.go safe. Applying a code
// releases *other* patients' stale holds on that code without touching their
// payment rows, so a payment can be carrying a discount whose reservation is
// already gone. Every path that is about to move money runs through here
// first: a hold that is still live is extended past the provider round trip,
// and one that is not is stripped and the list price restored.
func (s *Service) refreshQuote(ctx context.Context, p Payment) (Payment, error) {
	if p.PromoCode == "" && p.DiscountCents == 0 {
		return p, nil
	}

	now := s.now().UTC()
	out := p
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		locked, err := tx.LockPayment(ctx, p.ID)
		if err != nil {
			return err
		}
		out = locked
		if locked.Status.Settled() || locked.PromoCode == "" {
			return nil
		}

		red, ok, err := tx.FindLiveRedemptionForPayment(ctx, locked.ID)
		if err != nil {
			return err
		}
		switch {
		case ok && red.Status == PromoConsumed:
			return nil // the money already moved with this code on it
		case ok && now.Before(red.ExpiresAt):
			// Extend past the provider round trip, so a charge can never
			// outlive the hold that justified its price.
			out = locked
			return tx.TouchRedemption(ctx, red.ID, now.Add(s.promo.ReservationTTL))
		case ok:
			if err := releasePromoFor(ctx, tx, locked.ID, ReleaseExpired, now); err != nil {
				return err
			}
		}

		locked.DiscountCents = 0
		locked.PromoCode = ""
		locked.AmountCents = locked.GrossAmountCents
		if err := tx.UpdatePayment(ctx, &locked); err != nil {
			return err
		}
		out = locked
		return nil
	})
	return out, err
}

func nextAction(st Status, res IntentResult) string {
	switch {
	case st == StatusSucceeded:
		return "none"
	case st == StatusRequiresPIN:
		return "enter_pin"
	case res.RedirectURL != "":
		return "redirect"
	case res.ClientSecret != "":
		return "confirm_card"
	default:
		return "none"
	}
}

// --- carrier billing PIN confirmation --------------------------------------

// ConfirmPINInput carries the code the patient received by SMS.
type ConfirmPINInput struct {
	PaymentID   uuid.UUID
	CallerID    uuid.UUID
	CallerIsOps bool
	PIN         string
}

// ConfirmPIN and the rest of the carrier-billing flow live in pin.go, next to
// the PINChallenge state they operate on. They used to live here, and the
// version that did had no expiry, no attempt counter, and mapped every
// provider rejection straight to a failed payment -- so one mistyped digit
// ended the payment. See the commentary at the top of pin.go.

// --- capture ---------------------------------------------------------------

// applyCapture is the single place a payment becomes money. It recomputes the
// split from the frozen rule, writes the payment row, appends the balanced
// ledger transaction, and enqueues payment.succeeded -- all on the caller's
// transaction, so either every one of those happened or none did.
//
// actualFeeCents is the rail's reported fee when it knows one. Zero means the
// modelled fee from the commission rule stands.
func (s *Service) applyCapture(ctx context.Context, tx Tx, p *Payment, actualFeeCents int64) error {
	if p.Status.Settled() && p.SucceededAt != nil {
		return nil // idempotent: a redelivered event changes nothing
	}

	// Reprice from the rule frozen at intent creation, so this is reproducible
	// years later from the payment row alone.
	if p.CommissionRuleID != nil {
		rule, err := s.pricer.RuleByID(ctx, *p.CommissionRuleID)
		if err != nil {
			return fmt.Errorf("payment: reload commission rule %s: %w", *p.CommissionRuleID, err)
		}
		split, err := ComputeSplit(p.AmountCents, rule)
		if err != nil {
			return err
		}
		p.CommissionCents = split.CommissionCents
		p.ProviderFeeCents = split.ProviderFeeCents
		p.DoctorPayoutCents = split.PayoutCents
	}

	// The rail's real fee supersedes the modelled one. The commission is
	// unchanged -- it is our contract with the doctor, not a function of what
	// Stripe charged us -- so the difference lands in the payout, and the
	// three parts still sum to the amount exactly.
	if actualFeeCents > 0 && actualFeeCents != p.ProviderFeeCents {
		if p.CommissionCents+actualFeeCents > p.AmountCents {
			return fmt.Errorf("payment: rail reported fee %d which with commission %d exceeds amount %d",
				actualFeeCents, p.CommissionCents, p.AmountCents)
		}
		p.ProviderFeeCents = actualFeeCents
		p.DoctorPayoutCents = p.AmountCents - p.CommissionCents - actualFeeCents
	}

	if !p.SplitBalances() {
		return fmt.Errorf("payment: split does not balance for %s: %d + %d + %d != %d",
			p.ID, p.CommissionCents, p.ProviderFeeCents, p.DoctorPayoutCents, p.AmountCents)
	}

	now := s.now().UTC()
	p.Status = StatusSucceeded
	p.SucceededAt = &now
	p.FailureReason = ""

	if err := tx.UpdatePayment(ctx, p); err != nil {
		return err
	}
	if err := tx.WriteLedger(ctx, s.captureLedger(*p, now)); err != nil {
		return err
	}
	// The promotion is spent here and nowhere else, on the same transaction as
	// the ledger: a code is consumed if and only if the money moved.
	if err := consumePromo(ctx, tx, p.ID, now); err != nil {
		return err
	}
	return tx.Enqueue(ctx, events.SubjectPaymentSucceeded, p.ID.String(), s.paymentEvent(*p))
}

// captureLedger builds the balanced double-entry for a capture.
//
//	DEBIT  asset:provider_receivable        amount
//	CREDIT revenue:commission               commission
//	CREDIT liability:doctor_payable         payout
//	CREDIT liability:provider_fee_payable   fee
//
// The gross amount is what the rail owes us; the three credits are where every
// cent of it is destined. Because commission + payout + fee == amount by
// construction, the transaction balances for every possible input.
func (s *Service) captureLedger(p Payment, at time.Time) LedgerTransaction {
	pid := p.ID
	did := p.DoctorID
	legs := []LedgerEntry{{
		Account: AccountProviderReceivable, Direction: Debit, AmountCents: p.AmountCents,
		Currency: p.Currency, PaymentID: &pid, DoctorID: &did, OccurredAt: at,
		Description: "gross consultation fee receivable from " + string(p.Provider),
	}}
	if p.CommissionCents > 0 {
		legs = append(legs, LedgerEntry{
			Account: AccountCommissionRevenue, Direction: Credit, AmountCents: p.CommissionCents,
			Currency: p.Currency, PaymentID: &pid, DoctorID: &did, OccurredAt: at,
			Description: "platform commission " + BpsString(rateOf(p)),
		})
	}
	if p.DoctorPayoutCents > 0 {
		legs = append(legs, LedgerEntry{
			Account: AccountDoctorPayable, Direction: Credit, AmountCents: p.DoctorPayoutCents,
			Currency: p.Currency, PaymentID: &pid, DoctorID: &did, OccurredAt: at,
			Description: "amount payable to doctor",
		})
	}
	if p.ProviderFeeCents > 0 {
		legs = append(legs, LedgerEntry{
			Account: AccountProviderFeePayable, Direction: Credit, AmountCents: p.ProviderFeeCents,
			Currency: p.Currency, PaymentID: &pid, DoctorID: &did, OccurredAt: at,
			Description: "processing fee retained by " + string(p.Provider),
		})
	}
	return LedgerTransaction{EntryID: uuid.New(), EntryType: EntryPaymentCaptured, Legs: legs}
}

// rateOf recovers the effective commission rate in basis points for display.
// It is presentation only; nothing computes money from it.
func rateOf(p Payment) int {
	if p.AmountCents == 0 {
		return 0
	}
	return int(p.CommissionCents * BasisPointDenominator / p.AmountCents)
}

func (s *Service) paymentEvent(p Payment) PaymentEvent {
	return PaymentEvent{
		PaymentID: p.ID, AppointmentID: p.AppointmentID, PatientID: p.PatientID, DoctorID: p.DoctorID,
		AmountCents: p.AmountCents, Currency: p.Currency, Provider: string(p.Provider),
		Status: string(p.Status), CommissionCents: p.CommissionCents,
		ProviderFeeCents: p.ProviderFeeCents, DoctorPayoutCents: p.DoctorPayoutCents,
		FailureReason: p.FailureReason, OccurredAt: s.now().UTC(),
	}
}

// --- reads -----------------------------------------------------------------

// GetPayment loads a payment, enforcing that the caller owns it.
func (s *Service) GetPayment(ctx context.Context, id, callerID, callerDoctorID uuid.UUID, ops bool) (Payment, error) {
	p, err := s.store.GetPayment(ctx, id)
	if err != nil {
		return Payment{}, err
	}
	if ops || p.PatientID == callerID || (callerDoctorID != uuid.Nil && p.DoctorID == callerDoctorID) {
		return p, nil
	}
	// Deliberately 403 rather than 404: the caller proved they know a valid
	// payment id, so pretending it does not exist buys nothing and confuses
	// support.
	return Payment{}, ErrForbidden
}

// ListMyPayments returns the caller's payments -- their own as a patient, or
// the ones they earned as a doctor.
func (s *Service) ListMyPayments(ctx context.Context, patientID, doctorID uuid.UUID, page Page) ([]Payment, int64, error) {
	if doctorID != uuid.Nil {
		return s.store.ListPaymentsForDoctor(ctx, doctorID, page)
	}
	return s.store.ListPaymentsForPatient(ctx, patientID, page)
}

// ListRefunds returns the refunds against a payment.
func (s *Service) ListRefunds(ctx context.Context, paymentID uuid.UUID) ([]Refund, error) {
	return s.store.ListRefundsForPayment(ctx, paymentID)
}

// Ledger returns the double-entry legs for a payment, for the finance report
// and for answering "where did this money go".
func (s *Service) Ledger(ctx context.Context, paymentID uuid.UUID) ([]LedgerEntry, error) {
	return s.store.LedgerForPayment(ctx, paymentID)
}

// --- refunds ---------------------------------------------------------------

// RefundInput requests money back.
type RefundInput struct {
	PaymentID   uuid.UUID
	CallerID    uuid.UUID
	CallerIsOps bool

	// Actor and StartAt drive the policy. A caller that is not ops may only
	// cancel as the patient, and may not claim a start time -- both are taken
	// from the request only for ops-initiated refunds.
	Actor   CancelActor
	NoShow  bool
	StartAt time.Time
	// CancelledAt is when the cancellation actually happened. It matters:
	// pricing a refund off "now" instead of off the cancellation instant
	// silently converts a full refund into a 50% one whenever the event sat in
	// a queue for two hours.
	CancelledAt time.Time
	// AmountCents, when non-zero, overrides the policy. Ops only.
	AmountCents int64
	// PercentOverride, when in 1..100, is the refund percentage decided by the
	// scheduling service and carried on appointment.cancelled.
	//
	// It exists so that this service applies the platform's cancellation policy
	// rather than its own copy of it. Zero means "not supplied" -- a genuine
	// zero-percent decision never reaches Refund, because the caller short-
	// circuits on Refundable() first.
	PercentOverride int
	Reason          RefundReason
	// IdempotencyKey lets a caller retry safely. Empty means one is derived.
	IdempotencyKey string
}

// Refund returns money to a patient according to the cancellation policy.
//
// The provider call happens between two transactions, never inside one: an
// HTTP call to Stripe holding a Postgres row lock is how a slow rail turns
// into a database incident.
func (s *Service) Refund(ctx context.Context, in RefundInput) (Refund, error) {
	p, err := s.store.GetPayment(ctx, in.PaymentID)
	if err != nil {
		return Refund{}, err
	}
	if !in.CallerIsOps && p.PatientID != in.CallerID {
		return Refund{}, ErrForbidden
	}

	// Every field below is an input to the refund PERCENTAGE, and every one of
	// them is ops-only per RefundInput's contract. Enforcing that here, rather
	// than trusting the handler to have done it, is deliberate: the handler
	// did not, and the way it did not was a misplaced closing brace that put
	// StartAt outside the ops block. A caller-supplied StartAt is worth 100%
	// of a captured payment, so the check that stops it should not live in the
	// layer where a brace can move it.
	//
	// Zeroing rather than erroring keeps a client that sends a harmless
	// unrecognised field working, while making the field inert.
	if !in.CallerIsOps {
		in.Actor = ActorPatient
		in.NoShow = false
		in.AmountCents = 0
		in.StartAt = time.Time{}
		in.CancelledAt = time.Time{}
		in.PercentOverride = 0

		// Reason belongs in this list, and its absence was worth 50% more.
		//
		// It does not price the refund directly, but it IS the derived
		// idempotency key ("refund:<payment>:<reason>", below). refundRequest
		// validates it as oneof six constants, so a caller who received 50%
		// under one reason could POST again under another, get a second key,
		// and be handed the remaining 50% -- RefundableCents() permits it and
		// Status.Settled() includes partially_refunded. Two requests, a full
		// refund of a consultation they attended, and a `refunds` row stamped
		// "doctor_cancelled" poisoning finance reporting.
		in.Reason = ""

		// And the wider point the percentage guard did not reach: nothing on
		// this path establishes that the appointment was ever cancelled.
		//
		// With StartAt zeroed, DecideRefund sees a notice period of roughly
		// minus two thousand years, lands on the patient_cancelled_late
		// branch, and returns 50% -- to any patient, for any settled payment,
		// including one for a consultation that happened. The percentage guard
		// stopped the caller choosing the NUMBER; it never established that a
		// refund was due at all.
		//
		// This service cannot know whether an appointment was cancelled --
		// scheduling owns that, decides the policy, and carries its decision
		// on appointment.cancelled, which OnAppointmentCancelled applies with
		// CallerIsOps=true. That is the whole legitimate patient refund path,
		// and it starts with cancelling the appointment. A direct POST here by
		// the patient bypasses it, and no client makes one.
		return Refund{}, ErrRefundNotSelfService
	}

	cancelledAt := in.CancelledAt
	if cancelledAt.IsZero() {
		cancelledAt = s.now().UTC()
	}
	decision := DecideRefund(in.Actor, in.NoShow, cancelledAt, in.StartAt)
	if in.PercentOverride > 0 && in.PercentOverride <= 100 {
		// The caller was handed a decision by whoever owns the policy. Keep
		// this service's reason vocabulary, take their number.
		decision.Percent = in.PercentOverride
		decision.Policy = fmt.Sprintf("%s (percent supplied by the scheduling service: %d%%)",
			decision.Policy, in.PercentOverride)
	}

	reason := in.Reason
	if reason == "" || !reason.Valid() {
		reason = decision.Reason
	}
	key := in.IdempotencyKey
	if key == "" {
		key = fmt.Sprintf("refund:%s:%s", p.ID, reason)
	}

	// The idempotency check comes before every other check, deliberately.
	//
	// After a full refund the payment has nothing left to refund, so a repeat
	// of the same request would otherwise be rejected as "nothing refundable"
	// -- which is technically true and operationally useless. A retry must see
	// the refund it already made.
	var existing Refund
	found := false
	err = s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		r, err := tx.FindRefundByKey(ctx, key)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		existing, found = r, true
		return nil
	})
	if err != nil {
		return Refund{}, err
	}
	if found && existing.Status == RefundSucceeded {
		return existing, nil
	}

	if !p.Status.Settled() {
		return Refund{}, fmt.Errorf("%w: payment %s is %s", ErrNothingRefundable, p.ID, p.Status)
	}

	mode := s.roundingFor(ctx, p)
	refund := existing
	if !found {
		amount := in.AmountCents
		if amount == 0 {
			if amount, err = RefundAmount(p, decision, mode); err != nil {
				return Refund{}, err
			}
		} else if !in.CallerIsOps {
			// Only ops may name an amount; a patient naming their own would be
			// naming their own refund.
			return Refund{}, ErrForbidden
		}
		if amount > p.RefundableCents() {
			amount = p.RefundableCents()
		}
		if amount <= 0 {
			return Refund{}, fmt.Errorf("%w: %s", ErrNothingRefundable, decision.Policy)
		}

		// Phase 1: reserve the refund row so a crash after the provider call
		// still leaves a record to reconcile against.
		refund = Refund{
			ID: uuid.New(), PaymentID: p.ID, AmountCents: amount, Currency: p.Currency,
			Reason: reason, Policy: decision.Policy, Percent: decision.Percent,
			Status: RefundPending, IdempotencyKey: key,
		}
		err = s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
			if r, err := tx.FindRefundByKey(ctx, key); err == nil {
				refund = r
				return nil
			} else if !errors.Is(err, ErrNotFound) {
				return err
			}
			return tx.InsertRefund(ctx, &refund)
		})
		if err != nil {
			return Refund{}, err
		}
		if refund.Status == RefundSucceeded {
			return refund, nil
		}
	}

	// Phase 2: ask the rail. The same idempotency key every time, so a retry
	// after a timeout returns the original refund rather than issuing a second.
	rail, err := s.providers.Get(p.Provider)
	if err != nil {
		return Refund{}, err
	}
	res, providerErr := rail.Refund(ctx, RefundRequest{
		PaymentID: p.ID, RefundID: refund.ID, ProviderIntentID: p.ProviderIntentID,
		AmountCents: refund.AmountCents, Currency: refund.Currency,
		Reason: refund.Reason, IdempotencyKey: refund.IdempotencyKey,
	})
	if providerErr != nil && !errors.Is(providerErr, ErrProviderRejected) {
		// Unavailable, not refused. The refund row stays pending and the next
		// call -- a retry, or the reconciliation webhook -- finishes it.
		return refund, providerErr
	}

	// Phase 3: record the outcome, move the money in the ledger, announce it.
	err = s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		locked, err := tx.FindRefundByKey(ctx, refund.IdempotencyKey)
		if err != nil {
			return err
		}
		if locked.Status == RefundSucceeded {
			refund = locked
			return nil
		}
		if providerErr != nil {
			locked.Status = RefundFailed
			locked.FailureReason = "provider rejected the refund"
			if err := tx.UpdateRefund(ctx, &locked); err != nil {
				return err
			}
			refund = locked
			return nil
		}
		locked.Status = RefundSucceeded
		locked.ProviderRefundID = res.ProviderRefundID
		if err := tx.UpdateRefund(ctx, &locked); err != nil {
			return err
		}
		refund = locked
		return s.applyRefund(ctx, tx, locked, mode)
	})
	if err != nil {
		return Refund{}, err
	}
	if providerErr != nil {
		return refund, providerErr
	}
	return refund, nil
}

// applyRefund moves the money: it claws the refund back from the commission
// and the doctor payable in the proportion they were originally split, updates
// the payment, writes the balanced ledger legs and enqueues payment.refunded.
func (s *Service) applyRefund(ctx context.Context, tx Tx, r Refund, mode Rounding) error {
	p, err := tx.LockPayment(ctx, r.PaymentID)
	if err != nil {
		return err
	}
	if r.AmountCents > p.RefundableCents() {
		return fmt.Errorf("%w: refund %d exceeds remaining %d", ErrNothingRefundable, r.AmountCents, p.RefundableCents())
	}

	commissionBack, payoutBack, err := ProrateRefund(p, r.AmountCents, mode)
	if err != nil {
		return err
	}
	// Never claw back more of either bucket than the capture put there.
	if commissionBack > p.CommissionCents-p.RefundedCommissionCents {
		over := commissionBack - (p.CommissionCents - p.RefundedCommissionCents)
		commissionBack -= over
		payoutBack += over
	}
	if payoutBack > p.DoctorPayoutCents-p.RefundedPayoutCents {
		over := payoutBack - (p.DoctorPayoutCents - p.RefundedPayoutCents)
		payoutBack -= over
		commissionBack += over
	}
	if commissionBack+payoutBack != r.AmountCents {
		return fmt.Errorf("payment: refund split %d + %d does not equal %d", commissionBack, payoutBack, r.AmountCents)
	}

	p.RefundedCents += r.AmountCents
	p.RefundedCommissionCents += commissionBack
	p.RefundedPayoutCents += payoutBack
	if p.RefundedCents >= p.AmountCents {
		p.Status = StatusRefunded
	} else {
		p.Status = StatusPartiallyRefunded
	}
	if err := tx.UpdatePayment(ctx, &p); err != nil {
		return err
	}

	at := s.now().UTC()
	pid, rid, did := p.ID, r.ID, p.DoctorID
	legs := []LedgerEntry{}
	if commissionBack > 0 {
		legs = append(legs, LedgerEntry{
			Account: AccountCommissionRevenue, Direction: Debit, AmountCents: commissionBack,
			Currency: p.Currency, PaymentID: &pid, RefundID: &rid, DoctorID: &did, OccurredAt: at,
			Description: "commission reversed on refund",
		})
	}
	if payoutBack > 0 {
		legs = append(legs, LedgerEntry{
			Account: AccountDoctorPayable, Direction: Debit, AmountCents: payoutBack,
			Currency: p.Currency, PaymentID: &pid, RefundID: &rid, DoctorID: &did, OccurredAt: at,
			Description: "doctor payable reversed on refund",
		})
	}
	legs = append(legs, LedgerEntry{
		Account: AccountProviderReceivable, Direction: Credit, AmountCents: r.AmountCents,
		Currency: p.Currency, PaymentID: &pid, RefundID: &rid, DoctorID: &did, OccurredAt: at,
		Description: "refund returned to patient via " + string(p.Provider),
	})
	if err := tx.WriteLedger(ctx, LedgerTransaction{
		EntryID: uuid.New(), EntryType: EntryRefundIssued, Legs: legs,
	}); err != nil {
		return err
	}

	return tx.Enqueue(ctx, events.SubjectPaymentRefunded, p.ID.String(), RefundEvent{
		RefundID: r.ID, PaymentID: p.ID, AppointmentID: p.AppointmentID,
		PatientID: p.PatientID, DoctorID: p.DoctorID,
		AmountCents: r.AmountCents, Currency: r.Currency, Reason: string(r.Reason),
		Percent: r.Percent, FullyRefunded: p.Status == StatusRefunded,
		RemainingCents: p.RefundableCents(), OccurredAt: at,
	})
}

// roundingFor resolves the rounding mode the payment was priced with, falling
// back to the platform default if the rule has since been deleted.
func (s *Service) roundingFor(ctx context.Context, p Payment) Rounding {
	if p.CommissionRuleID == nil {
		return DefaultRounding
	}
	rule, err := s.pricer.RuleByID(ctx, *p.CommissionRuleID)
	if err != nil || !rule.Rounding.Valid() {
		return DefaultRounding
	}
	return rule.Rounding
}

// --- webhooks --------------------------------------------------------------

// WebhookResult tells the handler what to answer.
type WebhookResult struct {
	EventID   string
	Replayed  bool
	Outcome   WebhookOutcome
	PaymentID *uuid.UUID
}

// ErrWebhookAmountMismatch means a webhook authenticated cleanly and then
// reported a different amount or currency than the payment row it names.
//
// It is never a transient condition and it is never safe to settle through.
// Either the rail is telling us something we did not ask for -- a partial
// capture reported as a capture, a currency conversion we did not request --
// or the callback was forged by someone who could not also change the payment
// row. Both deserve a stop, and neither is fixed by retrying.
var ErrWebhookAmountMismatch = errors.New("payment: webhook amount does not match the payment")

// AmountMismatch carries both sides of the disagreement so the operator does
// not have to reconstruct it from two systems at 3am. It is recorded verbatim
// in webhook_events.processing_error.
type AmountMismatch struct {
	PaymentID        uuid.UUID
	EventID          string
	EventType        string
	ReportedCents    int64
	ExpectedCents    int64
	ReportedCurrency string
	ExpectedCurrency string
}

func (m *AmountMismatch) Error() string {
	return fmt.Sprintf("%s: event %s reported %d %s against payment %s which is %d %s",
		ErrWebhookAmountMismatch, m.EventID, m.ReportedCents, m.ReportedCurrency,
		m.PaymentID, m.ExpectedCents, m.ExpectedCurrency)
}

func (m *AmountMismatch) Unwrap() error { return ErrWebhookAmountMismatch }

// checkReportedAmount refuses to settle a payment at a figure the rail did not
// report.
//
// Every provider populates WebhookEvent.AmountCents and .Currency and, until
// this existed, nothing read either of them: applyCapture marked the payment
// succeeded at the DATABASE's amount whatever the rail actually collected. A
// webhook claiming any amount at all was accepted, and a partially-captured
// Stripe intent settled as a full capture.
//
// A zero amount means "this rail did not report one on this event" and is not
// a mismatch -- PayHere's chargeback notify and Stripe's payment_intent
// failure both legitimately carry none. A zero currency is the same. What is
// refused is a rail that names a figure and names a different one from ours.
func checkReportedAmount(p Payment, evt WebhookEvent) error {
	amountDisagrees := evt.AmountCents != 0 && evt.AmountCents != p.AmountCents
	currencyDisagrees := evt.Currency != "" && !strings.EqualFold(evt.Currency, p.Currency)
	if !amountDisagrees && !currencyDisagrees {
		return nil
	}
	return &AmountMismatch{
		PaymentID: p.ID, EventID: evt.EventID, EventType: evt.Type,
		ReportedCents: evt.AmountCents, ExpectedCents: p.AmountCents,
		ReportedCurrency: evt.Currency, ExpectedCurrency: p.Currency,
	}
}

// HandleWebhook authenticates, deduplicates and applies one provider callback.
//
// Everything after verification happens in ONE transaction: the webhook_events
// insert, the payment state change, the ledger legs and the outbox event. That
// is what makes the idempotency claim true rather than aspirational -- a
// replayed event cannot produce a second ledger effect, because the insert that
// would have to precede it is rejected by the unique index in the same
// transaction.
//
// A processing failure rolls the whole thing back, including the dedup row, so
// the provider's retry is genuinely retried rather than being swallowed as a
// duplicate.
func (s *Service) HandleWebhook(ctx context.Context, providerName ProviderName, headers http.Header, body []byte) (WebhookResult, error) {
	rail, err := s.providers.Get(providerName)
	if err != nil {
		return WebhookResult{}, err
	}

	evt, err := rail.VerifyWebhook(ctx, headers, body)
	if err != nil {
		// Do not touch the database and do not log the body. An unauthenticated
		// caller must not be able to write a row or fill a disk.
		return WebhookResult{}, err
	}
	if evt.EventID == "" {
		return WebhookResult{}, fmt.Errorf("%w: event carries no id to deduplicate on", ErrSignatureInvalid)
	}

	// A completed card setup is recorded BEFORE the dedup transaction, not
	// inside it, because saving a card needs two provider API calls -- fetch
	// the setup intent, then describe the token -- and network I/O inside a
	// transaction holds row locks across seconds of somebody else's latency.
	//
	// Doing it before the dedup claim is safe precisely because the write is
	// idempotent on UNIQUE (provider, provider_token): the client's own
	// confirm call and this webhook race constantly, and the loser is a no-op
	// either way. Failing here returns an error, so the rail retries and the
	// card is not lost.
	if evt.Outcome == OutcomeSetupSucceeded {
		if err := s.OnSetupIntentWebhook(ctx, evt.ProviderIntentID); err != nil {
			return WebhookResult{}, err
		}
	}

	result := WebhookResult{EventID: evt.EventID, Outcome: evt.Outcome}
	var mismatch *AmountMismatch
	err = s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		rec := &WebhookEventRecord{
			ID: uuid.New(), Provider: providerName, EventID: evt.EventID,
			EventType: evt.Type, Payload: evt.Raw, ReceivedAt: s.now().UTC(),
		}
		claimed, err := tx.ClaimWebhookEvent(ctx, rec)
		if err != nil {
			return err
		}
		if !claimed {
			result.Replayed = true
			return nil
		}

		paymentID, note, err := s.applyWebhook(ctx, tx, providerName, evt)
		if err != nil {
			// A money disagreement is the one processing failure that must be
			// KEPT rather than rolled back. Rolling it back would discard the
			// dedup row along with the evidence, and the rail's next retry
			// would arrive as a first delivery and be refused all over again,
			// forever, with nothing on record. So the event row commits with
			// the discrepancy in processing_error, no payment state moves, and
			// the error is returned to the handler afterwards.
			var mm *AmountMismatch
			if errors.As(err, &mm) {
				mismatch = mm
				return tx.MarkWebhookProcessed(ctx, rec.ID, &mm.PaymentID, mm.Error())
			}
			return err
		}
		result.PaymentID = paymentID
		return tx.MarkWebhookProcessed(ctx, rec.ID, paymentID, note)
	})
	if err != nil {
		return WebhookResult{}, err
	}
	if mismatch != nil {
		// Loud, and at error level: this is either a provider defect or an
		// attempt to settle a payment at a price nobody agreed to, and both
		// are things a human has to look at. The amounts are money, not PHI.
		s.log.Error().
			Str("provider", string(providerName)).
			Str("event_id", logger.MaskID(mismatch.EventID)).
			Str("event_type", mismatch.EventType).
			Str("payment_id", logger.MaskID(mismatch.PaymentID.String())).
			Int64("reported_cents", mismatch.ReportedCents).
			Int64("expected_cents", mismatch.ExpectedCents).
			Str("reported_currency", mismatch.ReportedCurrency).
			Str("expected_currency", mismatch.ExpectedCurrency).
			Msg("refusing to settle: the rail and the payment row disagree about the money")
		return WebhookResult{}, mismatch
	}
	return result, nil
}

// applyWebhook maps a verified event onto a state transition.
func (s *Service) applyWebhook(ctx context.Context, tx Tx, providerName ProviderName, evt WebhookEvent) (*uuid.UUID, string, error) {
	if evt.Outcome == OutcomeIgnored {
		return nil, "no handler for event type " + evt.Type, nil
	}

	switch evt.Outcome {
	case OutcomeSetupSucceeded:
		// Already applied by HandleWebhook, outside this transaction. The
		// event row is still written, so the audit trail records that the
		// rail told us.
		return nil, "card setup applied before the dedup transaction", nil
	case OutcomeRefundSucceeded, OutcomeRefundFailed:
		return s.applyRefundWebhook(ctx, tx, evt)
	case OutcomePayoutPaid, OutcomePayoutFailed:
		// Payout state is owned by the payout job, which polls the transfer it
		// created. A rail-initiated payout event is recorded for the audit
		// trail and changes nothing, because two writers for one row is how
		// payouts get paid twice.
		return nil, "payout events are recorded but not applied; the payout job owns that state", nil
	}

	p, err := s.resolvePayment(ctx, tx, providerName, evt)
	if err != nil {
		return nil, "", err
	}

	switch evt.Outcome {
	case OutcomePaymentSucceeded:
		if p.Status.Settled() {
			return &p.ID, "payment already settled", nil
		}
		// Before applyCapture, never after: applyCapture is where the payment
		// becomes money and where the ledger legs are appended, and the ledger
		// is append-only, so a wrong amount written there cannot be corrected.
		if err := checkReportedAmount(p, evt); err != nil {
			return nil, "", err
		}
		if err := s.applyCapture(ctx, tx, &p, evt.ProviderFeeCents); err != nil {
			return nil, "", err
		}
		return &p.ID, "", nil

	case OutcomePaymentFailed:
		if p.Status.Settled() {
			// A late failure event for money we already have is a rail bug or
			// an out-of-order delivery. Recording it and refusing to act is
			// the only safe response.
			return &p.ID, "failure event arrived after settlement; ignored", nil
		}
		if p.Status == StatusFailed {
			return &p.ID, "already failed", nil
		}
		p.Status = StatusFailed
		p.FailureReason = evt.FailureReason
		if err := tx.UpdatePayment(ctx, &p); err != nil {
			return nil, "", err
		}
		// The patient did not get the consultation, so they keep the code.
		if err := releasePromoFor(ctx, tx, p.ID, ReleasePaymentFail, s.now().UTC()); err != nil {
			return nil, "", err
		}
		if err := tx.Enqueue(ctx, events.SubjectPaymentFailed, p.ID.String(), s.paymentEvent(p)); err != nil {
			return nil, "", err
		}
		return &p.ID, "", nil

	case OutcomePaymentPending:
		if p.Status.Settled() || p.Status == StatusFailed {
			return &p.ID, "terminal state; pending event ignored", nil
		}
		p.Status = StatusRequiresAction
		if err := tx.UpdatePayment(ctx, &p); err != nil {
			return nil, "", err
		}
		return &p.ID, "", nil

	default:
		return &p.ID, "unhandled outcome " + string(evt.Outcome), nil
	}
}

func (s *Service) applyRefundWebhook(ctx context.Context, tx Tx, evt WebhookEvent) (*uuid.UUID, string, error) {
	if evt.ProviderRefundID == "" {
		return nil, "refund event carries no provider refund id", nil
	}
	r, err := tx.FindRefundByProviderID(ctx, evt.ProviderRefundID)
	if errors.Is(err, ErrNotFound) {
		// A refund we did not initiate: someone refunded from the Stripe
		// dashboard. Recording it and acknowledging is correct -- inventing a
		// refunds row for it would produce a ledger entry with no policy
		// behind it. The reconciliation procedure in the runbook covers this.
		return nil, "refund " + evt.ProviderRefundID + " was not initiated by this service", nil
	}
	if err != nil {
		return nil, "", err
	}

	switch {
	case r.Status == RefundSucceeded:
		return &r.PaymentID, "refund already applied", nil

	case evt.Outcome == OutcomeRefundFailed:
		r.Status = RefundFailed
		r.FailureReason = evt.FailureReason
		if err := tx.UpdateRefund(ctx, &r); err != nil {
			return nil, "", err
		}
		return &r.PaymentID, "", nil

	default:
		// The normal path here is a refund left pending by a crash between the
		// provider call and the commit. The webhook finishes it.
		r.Status = RefundSucceeded
		if err := tx.UpdateRefund(ctx, &r); err != nil {
			return nil, "", err
		}
		p, err := tx.LockPayment(ctx, r.PaymentID)
		if err != nil {
			return nil, "", err
		}
		if err := s.applyRefund(ctx, tx, r, s.roundingFor(ctx, p)); err != nil {
			return nil, "", err
		}
		return &r.PaymentID, "", nil
	}
}

// resolvePayment finds the payment an event refers to, trying the correlation
// identifiers in order of reliability.
func (s *Service) resolvePayment(ctx context.Context, tx Tx, providerName ProviderName, evt WebhookEvent) (Payment, error) {
	if evt.ProviderIntentID != "" {
		p, err := tx.LockPaymentByIntent(ctx, providerName, evt.ProviderIntentID)
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return Payment{}, err
		}
	}
	if evt.PaymentID != "" {
		if id, perr := uuid.Parse(evt.PaymentID); perr == nil {
			p, err := tx.LockPayment(ctx, id)
			if err == nil {
				return sameRail(providerName, p)
			}
			if !errors.Is(err, ErrNotFound) {
				return Payment{}, err
			}
		}
	}
	if evt.AppointmentID != "" {
		if id, perr := uuid.Parse(evt.AppointmentID); perr == nil {
			p, err := tx.LockPaymentByAppointment(ctx, id)
			if err == nil {
				return sameRail(providerName, p)
			}
			if !errors.Is(err, ErrNotFound) {
				return Payment{}, err
			}
		}
	}
	// Returning ErrNotFound rolls the transaction back and answers non-2xx, so
	// the rail retries. That matters: the usual cause is that this event beat
	// appointment.created through the system by a few hundred milliseconds.
	return Payment{}, fmt.Errorf("%w: no payment matches event %s", ErrNotFound, evt.EventID)
}

// sameRail refuses to let one provider's callback settle another provider's
// payment.
//
// LockPaymentByIntent is already scoped to the rail. The two fallback lookups
// -- by payment id and by appointment id -- were not, so a signature verified
// with PayHere's secret could settle a payment whose row says "stripe"
// (SECURITY-REVIEW F32). Each rail's signature says "this callback came from
// us"; it says nothing about a payment that was never ours, and a callback for
// a payment on another rail is either a bug or an attempt.
func sameRail(providerName ProviderName, p Payment) (Payment, error) {
	if p.Provider != "" && p.Provider != providerName {
		return Payment{}, fmt.Errorf("%w: a %s callback resolved payment %s, which is on %s",
			ErrNotFound, providerName, p.ID, p.Provider)
	}
	return p, nil
}

// --- event consumers -------------------------------------------------------

// OnAppointmentCreated creates the pending payment for a new appointment.
// It is idempotent on appointment_id: a redelivered event finds the row
// already there and returns without touching it.
func (s *Service) OnAppointmentCreated(ctx context.Context, payload events.AppointmentCreated) error {
	if payload.AppointmentID == uuid.Nil || payload.AmountCents <= 0 {
		return fmt.Errorf("payment: appointment.created for %s carries no payable amount", payload.AppointmentID)
	}
	currency := payload.Currency
	if currency == "" {
		currency = s.cfg.Currency
	}

	p := Payment{
		ID:              uuid.New(),
		AppointmentID:   payload.AppointmentID,
		PatientID:       payload.PatientID,
		DoctorID:        payload.DoctorID,
		Specialty:       payload.Specialty,
		CorporateClient: payload.CorporateClient,
		AmountCents:     payload.AmountCents,
		// No promotion has been applied yet, so gross == charged. ApplyPromo
		// is the only thing that ever moves them apart.
		GrossAmountCents: payload.AmountCents,
		Currency:         currency,
		Provider:         s.cfg.DefaultProvider,
		Status:           StatusPending,
		// Deterministic from the appointment, so a redelivery of the event
		// collides on the unique index instead of creating a second payment.
		IdempotencyKey: "appointment:" + payload.AppointmentID.String(),
	}

	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		if _, err := tx.LockPaymentByAppointment(ctx, payload.AppointmentID); err == nil {
			return nil // already created
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		return tx.InsertPayment(ctx, &p)
	})
	if errors.Is(err, ErrDuplicate) {
		return nil // lost a race with a concurrent delivery; the row exists
	}
	return err
}

// OnAppointmentCancelled applies the refund policy to a cancellation.
//
// A cancellation with no payment, or with a payment that never captured, is a
// no-op rather than an error: there is no money to return and the event must
// not be redelivered forever.
func (s *Service) OnAppointmentCancelled(ctx context.Context, payload events.AppointmentCancelled) error {
	p, err := s.store.GetPaymentByAppointment(ctx, payload.AppointmentID)
	if errors.Is(err, ErrNotFound) {
		s.log.Debug().Str("appointment_id", logger.MaskID(payload.AppointmentID.String())).
			Msg("cancellation for an appointment with no payment; nothing to refund")
		return nil
	}
	if err != nil {
		return err
	}
	if !p.Status.Settled() || p.RefundableCents() == 0 {
		// Nothing to refund, but an unpaid payment may still be holding a
		// promo code the patient should get back.
		if !p.Status.Settled() {
			return s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
				return releasePromoFor(ctx, tx, p.ID, ReleaseCancelled, s.now().UTC())
			})
		}
		return nil
	}

	// CancelledAt is the moment the cancellation was REQUESTED, not the moment
	// this event was consumed. Pricing off "now" silently converts a full
	// refund into a 50% one whenever the event sat in a queue past the
	// two-hour boundary -- a bug that only ever appears under load, in
	// production, in the patient's favour never.
	cancelledAt := payload.CancelledAt
	if cancelledAt.IsZero() {
		cancelledAt = s.now().UTC()
	}

	actor := ParseCancelActor(payload.CancelledBy)

	// Scheduling owns the cancellation policy and already decided. Adopt its
	// number rather than recomputing one that can disagree.
	decision, fromEvent := RefundDecisionFromEvent(
		payload.RefundPolicy, payload.RefundPercent, actor, payload.NoShow, cancelledAt, payload.StartAt)
	if !fromEvent {
		s.log.Warn().
			Str("appointment_id", logger.MaskID(payload.AppointmentID.String())).
			Msg("appointment.cancelled carried no refund decision; falling back to " +
				"this service's copy of the policy")
		decision = DecideRefund(actor, payload.NoShow, cancelledAt, payload.StartAt)
	}

	if !decision.Refundable() {
		s.log.Info().
			Str("payment_id", logger.MaskID(p.ID.String())).
			Str("policy", decision.Policy).
			Msg("cancellation carries no refund")
		return nil
	}

	_, err = s.Refund(ctx, RefundInput{
		PaymentID:   p.ID,
		CallerIsOps: true, // the platform itself is acting, not a user
		Actor:       actor,
		NoShow:      payload.NoShow,
		StartAt:     payload.StartAt,
		CancelledAt: cancelledAt,
		Reason:      decision.Reason,
		// PercentOverride carries scheduling's decision through Refund, which
		// would otherwise re-derive it from the policy in this package.
		PercentOverride: decision.Percent,
		// Keyed on the appointment so a redelivered cancellation cannot refund
		// twice.
		IdempotencyKey: "cancel:" + payload.AppointmentID.String(),
	})
	if errors.Is(err, ErrNothingRefundable) {
		return nil
	}
	return err
}

// --- payouts and invoices (read paths used by the handler) -----------------

// ListPayouts returns settlement history. A doctor sees only their own; finance
// and super_admin see every doctor's.
func (s *Service) ListPayouts(ctx context.Context, doctorID uuid.UUID, all bool, page Page) ([]Payout, int64, error) {
	if all {
		return s.store.ListAllPayouts(ctx, page)
	}
	if doctorID == uuid.Nil {
		// A caller who is neither a doctor nor finance has no payouts to see.
		// An empty list is the honest answer; a 403 would leak that payouts
		// exist at all.
		return []Payout{}, 0, nil
	}
	return s.store.ListPayoutsForDoctor(ctx, doctorID, page)
}

// PayoutDetail returns a payout with the payments it settled, which is the
// remittance advice a doctor needs to reconcile their bank statement.
func (s *Service) PayoutDetail(ctx context.Context, id, doctorID uuid.UUID, all bool) (Payout, []Payment, error) {
	p, err := s.store.GetPayout(ctx, id)
	if err != nil {
		return Payout{}, nil, err
	}
	if !all && p.DoctorID != doctorID {
		return Payout{}, nil, ErrForbidden
	}
	items, err := s.store.ListPaymentsForPayout(ctx, id)
	if err != nil {
		return Payout{}, nil, err
	}
	return p, items, nil
}

// RuleFor returns the exact commission rule version a payment was priced with.
//
// This is the whole reason rules are rows rather than an environment variable:
// an invoice regenerated at any point in the future reproduces the rate that
// was in force on the day, not the rate that is in force now.
func (s *Service) RuleFor(ctx context.Context, p Payment) (CommissionRule, error) {
	if p.CommissionRuleID == nil {
		return CommissionRule{}, nil
	}
	rule, err := s.pricer.RuleByID(ctx, *p.CommissionRuleID)
	if errors.Is(err, ErrNotFound) {
		// A rule row is never deleted, so this only happens if someone did
		// something to the database by hand. The invoice still renders; it
		// simply cannot show the rate breakdown.
		return CommissionRule{}, nil
	}
	return rule, err
}

// Now exposes the service clock to the handler, so a DTO that has to decide
// whether a saved card has expired uses the same clock the tests pin rather
// than reaching for time.Now itself.
func (s *Service) Now() time.Time { return s.now().UTC() }

// PaymentForAppointment resolves an appointment to its payment with the same
// ownership check every other read applies. It exists so the appointment-keyed
// carrier-billing endpoints and the payment-keyed one share one authorisation
// rule instead of two that can drift.
func (s *Service) PaymentForAppointment(ctx context.Context, appointmentID, callerID uuid.UUID, ops bool) (Payment, error) {
	p, err := s.store.GetPaymentByAppointment(ctx, appointmentID)
	if errors.Is(err, ErrNotFound) {
		return Payment{}, ErrPaymentNotReady
	}
	if err != nil {
		return Payment{}, err
	}
	if !ops && p.PatientID != callerID {
		return Payment{}, ErrForbidden
	}
	return p, nil
}
