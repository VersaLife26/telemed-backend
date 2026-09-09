// Package payment owns everything the platform knows about money: intents,
// provider webhooks, commission, refunds, doctor payouts, invoices, and the
// double-entry ledger that makes all of it auditable.
//
// It deliberately owns nothing else. It does not know whether an appointment
// happened, whether a doctor is verified, or whether a patient received a
// notification. It learns about those through events and tells the rest of the
// platform about money through events.
package payment

import (
	"time"

	"github.com/google/uuid"
)

// Currency is an ISO-4217 code. LKR is the only currency the platform prices
// in today; the column exists so that a USD corporate contract in 2029 is a
// data change rather than a migration of every amount column.
const CurrencyLKR = "LKR"

// Status is the payment lifecycle state.
//
//	pending ──▶ requires_action ──▶ succeeded ──▶ partially_refunded ──▶ refunded
//	   │                               │
//	   ├──▶ requires_pin ──────────────┤
//	   └──▶ failed ◀───────────────────┘
//
// Payout is not a status: a payment that has been settled to its doctor is a
// succeeded payment with a non-null payout_id. Conflating the two is how you
// end up unable to refund a paid-out consultation.
type Status string

const (
	StatusPending           Status = "pending"
	StatusRequiresAction    Status = "requires_action" // 3DS or redirect pending
	StatusRequiresPIN       Status = "requires_pin"    // Dialog carrier billing, PIN sent
	StatusSucceeded         Status = "succeeded"
	StatusFailed            Status = "failed"
	StatusPartiallyRefunded Status = "partially_refunded"
	StatusRefunded          Status = "refunded"
)

// Terminal reports whether no further provider callback can change the state.
func (s Status) Terminal() bool {
	switch s {
	case StatusFailed, StatusRefunded:
		return true
	default:
		return false
	}
}

// Settled reports whether the money has actually been captured.
func (s Status) Settled() bool {
	switch s {
	case StatusSucceeded, StatusPartiallyRefunded, StatusRefunded:
		return true
	default:
		return false
	}
}

// ProviderName identifies a payment rail.
type ProviderName string

const (
	ProviderStripe  ProviderName = "stripe"
	ProviderPayHere ProviderName = "payhere"
	ProviderDialog  ProviderName = "dialog"
	ProviderMock    ProviderName = "mock"
)

// Valid reports whether p is a rail this service knows about. Used to reject a
// hand-crafted request body before it reaches a CHECK constraint.
func (p ProviderName) Valid() bool {
	switch p {
	case ProviderStripe, ProviderPayHere, ProviderDialog, ProviderMock:
		return true
	default:
		return false
	}
}

// Payment is one patient payment for one appointment.
//
// Every monetary field is int64 cents. There is no float64 in this struct, in
// any struct it contains, or in any function that computes one of its fields.
type Payment struct {
	ID            uuid.UUID `json:"id"`
	AppointmentID uuid.UUID `json:"appointment_id"`
	PatientID     uuid.UUID `json:"patient_id"`
	DoctorID      uuid.UUID `json:"doctor_id"`

	// Pricing inputs frozen at creation time. A doctor who changes specialty
	// next year does not retroactively reprice this consultation.
	Specialty       string `json:"specialty,omitempty"`
	CorporateClient string `json:"corporate_client,omitempty"`

	// AmountCents is what the rail is asked to charge and what the
	// commission split is computed from. GrossAmountCents is the doctor's
	// list price before any promotion, and DiscountCents is the difference.
	// The three are tied together by a CHECK constraint:
	// gross = amount + discount, always.
	//
	// Keeping AmountCents as the charged figure is deliberate. Every existing
	// rule -- ComputeSplit, the ledger, the payout claim, the refund policy --
	// reads it, and all of them want the number the patient actually paid. A
	// design where AmountCents stayed gross and a discount was subtracted
	// later would have required auditing every one of those call sites, and
	// missing one would have paid a doctor out of money nobody collected.
	AmountCents      int64  `json:"amount_cents"`
	GrossAmountCents int64  `json:"gross_amount_cents"`
	DiscountCents    int64  `json:"discount_cents"`
	PromoCode        string `json:"promo_code,omitempty"`
	Currency         string `json:"currency"`

	Provider          ProviderName `json:"provider"`
	ProviderIntentID  string       `json:"provider_intent_id,omitempty"`
	ProviderReference string       `json:"provider_reference,omitempty"`

	Status Status `json:"status"`

	CommissionCents   int64 `json:"commission_cents"`
	ProviderFeeCents  int64 `json:"provider_fee_cents"`
	DoctorPayoutCents int64 `json:"doctor_payout_cents"`
	RefundedCents     int64 `json:"refunded_cents"`

	// Refunds are clawed back from the commission and the doctor payout in the
	// proportion they were originally split. Tracking the clawback separately
	// keeps the capture-time split immutable, which is what lets an invoice
	// reprinted in 2033 still show the numbers the patient actually saw.
	RefundedCommissionCents int64 `json:"refunded_commission_cents"`
	RefundedPayoutCents     int64 `json:"refunded_payout_cents"`

	// The exact commission_rules row that priced this payment. Without this an
	// invoice reprinted after a rate change silently reports the wrong split.
	CommissionRuleID  *uuid.UUID `json:"commission_rule_id,omitempty"`
	CommissionRuleKey string     `json:"commission_rule_key,omitempty"`
	CommissionRuleVer int        `json:"commission_rule_version,omitempty"`

	IdempotencyKey string     `json:"-"`
	PayoutID       *uuid.UUID `json:"payout_id,omitempty"`
	FailureReason  string     `json:"failure_reason,omitempty"`

	SucceededAt *time.Time `json:"succeeded_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	DeletedAt   *time.Time `json:"-"`
	Version     int        `json:"version"`
}

// RefundableCents is how much of this payment may still be returned.
func (p Payment) RefundableCents() int64 {
	if !p.Status.Settled() {
		return 0
	}
	return p.AmountCents - p.RefundedCents
}

// NetCommissionCents is what the platform keeps after refunds.
func (p Payment) NetCommissionCents() int64 { return p.CommissionCents - p.RefundedCommissionCents }

// NetPayoutCents is what the doctor is actually owed after refunds. This, not
// DoctorPayoutCents, is what the payout job settles.
func (p Payment) NetPayoutCents() int64 { return p.DoctorPayoutCents - p.RefundedPayoutCents }

// DiscountBalances asserts that the promotion arithmetic reconstitutes the
// list price. It is checked in the service before every write and again by the
// payments_discount_balances CHECK, for the same reason SplitBalances is.
func (p Payment) DiscountBalances() bool {
	return p.GrossAmountCents == p.AmountCents+p.DiscountCents
}

// SplitBalances asserts the money invariant this whole service exists to keep.
// It is checked in the service layer before every write and again by a CHECK
// constraint in Postgres, because one of those two will eventually be edited by
// someone who did not read this comment.
func (p Payment) SplitBalances() bool {
	return p.CommissionCents+p.ProviderFeeCents+p.DoctorPayoutCents == p.AmountCents
}

// WebhookEventRecord is the persisted record of one inbound provider callback.
// Its (provider, event_id) uniqueness is the platform's replay defence.
type WebhookEventRecord struct {
	ID              uuid.UUID
	Provider        ProviderName
	EventID         string
	EventType       string
	PaymentID       *uuid.UUID
	Payload         []byte
	SignatureHeader string
	ReceivedAt      time.Time
	ProcessedAt     *time.Time
	ProcessingError string
	Attempts        int
}

// PayoutStatus is the settlement lifecycle for a doctor payout batch.
type PayoutStatus string

const (
	PayoutPending    PayoutStatus = "pending"    // claimed payments, no transfer attempted
	PayoutProcessing PayoutStatus = "processing" // transfer in flight at the provider
	PayoutPaid       PayoutStatus = "paid"
	PayoutFailed     PayoutStatus = "failed"
	PayoutCancelled  PayoutStatus = "cancelled" // nothing to settle
)

// Payout is one settlement to one doctor for one period.
type Payout struct {
	ID          uuid.UUID `json:"id"`
	DoctorID    uuid.UUID `json:"doctor_id"`
	PeriodStart time.Time `json:"period_start"`
	PeriodEnd   time.Time `json:"period_end"`

	AmountCents  int64  `json:"amount_cents"`
	Currency     string `json:"currency"`
	PaymentCount int    `json:"payment_count"`

	Status        PayoutStatus `json:"status"`
	Provider      ProviderName `json:"provider"`
	TransferID    string       `json:"transfer_id,omitempty"`
	FailureReason string       `json:"failure_reason,omitempty"`

	IdempotencyKey string `json:"-"`

	InitiatedAt *time.Time `json:"initiated_at,omitempty"`
	PaidAt      *time.Time `json:"paid_at,omitempty"`
	Attempts    int        `json:"attempts"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Version   int       `json:"version"`
}

// RefundStatus is the refund lifecycle.
type RefundStatus string

const (
	RefundPending   RefundStatus = "pending"
	RefundSucceeded RefundStatus = "succeeded"
	RefundFailed    RefundStatus = "failed"
)

// RefundReason records why money went back. It is a closed set so the finance
// report can group by it without string archaeology.
type RefundReason string

const (
	ReasonDoctorCancelled       RefundReason = "doctor_cancelled"
	ReasonPatientCancelledEarly RefundReason = "patient_cancelled_early"
	ReasonPatientCancelledLate  RefundReason = "patient_cancelled_late"
	ReasonNoShow                RefundReason = "no_show"
	ReasonAdminOverride         RefundReason = "admin_override"
	ReasonDuplicate             RefundReason = "duplicate"
)

// Valid reports whether r is one of the recognised reasons.
func (r RefundReason) Valid() bool {
	switch r {
	case ReasonDoctorCancelled, ReasonPatientCancelledEarly, ReasonPatientCancelledLate,
		ReasonNoShow, ReasonAdminOverride, ReasonDuplicate:
		return true
	default:
		return false
	}
}

// Refund is one return of money against a payment.
type Refund struct {
	ID        uuid.UUID `json:"id"`
	PaymentID uuid.UUID `json:"payment_id"`

	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`

	Reason  RefundReason `json:"reason"`
	Policy  string       `json:"policy,omitempty"`
	Percent int          `json:"percent"`

	ProviderRefundID string       `json:"provider_refund_id,omitempty"`
	Status           RefundStatus `json:"status"`
	FailureReason    string       `json:"failure_reason,omitempty"`

	IdempotencyKey string `json:"-"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Version   int       `json:"version"`
}

// RuleScope orders commission rule specificity. Higher wins.
type RuleScope string

const (
	ScopeDefault   RuleScope = "default"   // applies to everything
	ScopeSpecialty RuleScope = "specialty" // e.g. Cardiology
	ScopeCorporate RuleScope = "corporate" // e.g. AIA staff plan
)

// Precedence returns the tie-break rank for rule selection.
func (s RuleScope) Precedence() int {
	switch s {
	case ScopeCorporate:
		return 3
	case ScopeSpecialty:
		return 2
	case ScopeDefault:
		return 1
	default:
		return 0
	}
}

// CommissionRule is one immutable version of one pricing rule.
//
// Rules are never updated in place. Superseding a rule stamps EffectiveTo on
// the current version and inserts version+1. Every payment stores the rule ID
// it was priced with, so an invoice regenerated years later reproduces the
// original numbers byte for byte.
type CommissionRule struct {
	ID      uuid.UUID `json:"id"`
	RuleKey string    `json:"rule_key"`
	Version int       `json:"version"`

	Scope      RuleScope `json:"scope"`
	MatchValue string    `json:"match_value,omitempty"`

	// Basis points: 2000 = 20.00%. Integers all the way down.
	RateBps               int   `json:"rate_bps"`
	ProviderFeeBps        int   `json:"provider_fee_bps"`
	ProviderFeeFixedCents int64 `json:"provider_fee_fixed_cents"`

	Rounding Rounding `json:"rounding"`

	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to,omitempty"`
	Note          string     `json:"note,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Ledger account names. They are strings rather than an enum because a chart of
// accounts grows, and a new account must never require a migration.
const (
	AccountProviderReceivable = "asset:provider_receivable"      // gross owed to us by the rail
	AccountCash               = "asset:cash"                     // our settled bank balance
	AccountCommissionRevenue  = "revenue:commission"             // what the platform earns
	AccountDoctorPayable      = "liability:doctor_payable"       // what we owe doctors
	AccountProviderFeePayable = "liability:provider_fee_payable" // what the rail keeps
)

// LedgerDirection is one side of a double-entry leg.
type LedgerDirection string

const (
	Debit  LedgerDirection = "debit"
	Credit LedgerDirection = "credit"
)

// Ledger entry_type values, used to group and explain a balanced set of legs.
const (
	EntryPaymentCaptured = "payment.captured"
	EntryRefundIssued    = "refund.issued"
	EntryPayoutSent      = "payout.sent"
)

// LedgerEntry is one leg of a balanced double-entry transaction. Rows are
// INSERT-only; the database refuses UPDATE, DELETE and TRUNCATE by trigger.
type LedgerEntry struct {
	ID        int64     `json:"id"`
	EntryID   uuid.UUID `json:"entry_id"`
	EntryType string    `json:"entry_type"`

	Account     string          `json:"account"`
	Direction   LedgerDirection `json:"direction"`
	AmountCents int64           `json:"amount_cents"`
	Currency    string          `json:"currency"`

	PaymentID *uuid.UUID `json:"payment_id,omitempty"`
	PayoutID  *uuid.UUID `json:"payout_id,omitempty"`
	RefundID  *uuid.UUID `json:"refund_id,omitempty"`
	DoctorID  *uuid.UUID `json:"doctor_id,omitempty"`

	Description string    `json:"description,omitempty"`
	OccurredAt  time.Time `json:"occurred_at"`
	CreatedAt   time.Time `json:"created_at"`
}

// LedgerTransaction is a set of legs that must balance before it is written.
type LedgerTransaction struct {
	EntryID   uuid.UUID
	EntryType string
	Legs      []LedgerEntry
}

// Balanced reports whether debits equal credits. A transaction that does not
// balance is never written; the service returns an error and rolls back.
func (t LedgerTransaction) Balanced() bool {
	var debit, credit int64
	for i := range t.Legs {
		l := &t.Legs[i]
		switch l.Direction {
		case Debit:
			debit += l.AmountCents
		case Credit:
			credit += l.AmountCents
		default:
			return false
		}
	}
	return len(t.Legs) > 0 && debit == credit
}

// --- event payloads --------------------------------------------------------
//
// These are the wire contracts other services consume. Fields are added, never
// renamed or removed; the envelope carries a version for the day that is not
// enough.

// PaymentEvent is published on payment.succeeded and payment.failed.
type PaymentEvent struct {
	PaymentID         uuid.UUID `json:"payment_id"`
	AppointmentID     uuid.UUID `json:"appointment_id"`
	PatientID         uuid.UUID `json:"patient_id"`
	DoctorID          uuid.UUID `json:"doctor_id"`
	AmountCents       int64     `json:"amount_cents"`
	Currency          string    `json:"currency"`
	Provider          string    `json:"provider"`
	Status            string    `json:"status"`
	CommissionCents   int64     `json:"commission_cents"`
	ProviderFeeCents  int64     `json:"provider_fee_cents"`
	DoctorPayoutCents int64     `json:"doctor_payout_cents"`
	FailureReason     string    `json:"failure_reason,omitempty"`
	OccurredAt        time.Time `json:"occurred_at"`
}

// RefundEvent is published on payment.refunded.
type RefundEvent struct {
	RefundID       uuid.UUID `json:"refund_id"`
	PaymentID      uuid.UUID `json:"payment_id"`
	AppointmentID  uuid.UUID `json:"appointment_id"`
	PatientID      uuid.UUID `json:"patient_id"`
	DoctorID       uuid.UUID `json:"doctor_id"`
	AmountCents    int64     `json:"amount_cents"`
	Currency       string    `json:"currency"`
	Reason         string    `json:"reason"`
	Percent        int       `json:"percent"`
	FullyRefunded  bool      `json:"fully_refunded"`
	RemainingCents int64     `json:"remaining_cents"`
	OccurredAt     time.Time `json:"occurred_at"`
}

// PayoutEvent is published on payout.sent.
type PayoutEvent struct {
	PayoutID     uuid.UUID `json:"payout_id"`
	DoctorID     uuid.UUID `json:"doctor_id"`
	AmountCents  int64     `json:"amount_cents"`
	Currency     string    `json:"currency"`
	PaymentCount int       `json:"payment_count"`
	PeriodStart  time.Time `json:"period_start"`
	PeriodEnd    time.Time `json:"period_end"`
	Provider     string    `json:"provider"`
	TransferID   string    `json:"transfer_id"`
	OccurredAt   time.Time `json:"occurred_at"`
}

// The appointment.created and appointment.cancelled payloads used to be
// declared here, privately, as "the slice of scheduling's event this service
// needs". They are now events.AppointmentCreated and
// events.AppointmentCancelled -- the same structs scheduling-service publishes.
//
// Taking a private slice of somebody else's payload sounds conservative and is
// not. It reads a contract nobody wrote down, and when the other side does not
// send a field there is no error: encoding/json leaves the zero value. This
// service required amount_cents, scheduling never sent it, and every booking on
// the platform arrived here with AmountCents == 0 and was refused. Both
// services were individually correct. Sharing the struct makes that a compile
// error.
