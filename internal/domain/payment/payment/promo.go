package payment

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// Promotional codes.
//
// The hard part of a promo code is not the arithmetic, it is that two people
// can type the last redemption of a code at the same millisecond and exactly
// one of them must get it. Everything in this file is arranged around that.
//
// # How a redemption is serialised
//
// Apply runs in one transaction that takes locks in a fixed order:
//
//	1. the payment row            (FOR UPDATE)  -- one apply per payment at a time
//	2. every promo_codes row involved, ordered by id (FOR UPDATE)
//
// Ordering step 2 by id is what makes replacing code X with code Y safe: two
// transactions swapping the same pair in opposite directions on different
// payments would otherwise deadlock. Sorting means they always take X before
// Y, so one waits instead.
//
// Holding the code's row lock for the whole check-then-write is the point.
// Reading the count, deciding there is budget left, and then inserting is only
// correct if nobody can insert between the read and the write, and a row lock
// is the only thing that guarantees that. The `promo_codes_budget` CHECK
// underneath is a backstop, not the mechanism.
//
// # Why a reservation and not an immediate spend
//
// Applying a code happens on the payment screen, before the patient has paid
// anything -- the discount has to be visible in the order summary for them to
// decide. If applying spent the code, a patient who typed it and then changed
// their mind would have burned a limited promotion for nothing. So Apply
// *reserves*, capture *consumes*, and everything else *releases*:
//
//	reserved --capture--> consumed      the code is spent
//	reserved --failure/cancel/replace/expiry--> released   the code is free again
//
// There is deliberately no consumed -> released edge. Refunding a paid
// consultation does not hand the promotion back; the patient used it.

// PromoDiscountType is how a code computes its discount.
type PromoDiscountType string

const (
	// PromoPercent takes a proportion of the consultation fee, in integer
	// basis points, optionally capped.
	PromoPercent PromoDiscountType = "percent"
	// PromoFixed takes a flat number of cents.
	PromoFixed PromoDiscountType = "fixed"
)

// PromoRedemptionStatus is the lifecycle of one redemption.
type PromoRedemptionStatus string

const (
	PromoReserved PromoRedemptionStatus = "reserved"
	PromoConsumed PromoRedemptionStatus = "consumed"
	PromoReleased PromoRedemptionStatus = "released"
)

// Release reasons. A closed set so a report can group by them.
const (
	ReleaseExpired     = "expired"
	ReleaseReplaced    = "replaced"
	ReleaseRemoved     = "removed_by_patient"
	ReleasePaymentFail = "payment_failed"
	ReleaseCancelled   = "appointment_cancelled"
)

// PromoCode is one promotion.
type PromoCode struct {
	ID          uuid.UUID `json:"id"`
	Code        string    `json:"code"`
	Description string    `json:"description,omitempty"`

	DiscountType     PromoDiscountType `json:"discount_type"`
	PercentBps       int               `json:"percent_bps,omitempty"`
	AmountOffCents   int64             `json:"amount_off_cents,omitempty"`
	MaxDiscountCents int64             `json:"max_discount_cents,omitempty"`
	MinAmountCents   int64             `json:"min_amount_cents,omitempty"`
	Currency         string            `json:"currency"`

	ValidFrom  time.Time  `json:"valid_from"`
	ValidUntil *time.Time `json:"valid_until,omitempty"`

	// MaxRedemptions nil means unlimited.
	MaxRedemptions  *int `json:"max_redemptions,omitempty"`
	MaxPerUser      int  `json:"max_per_user"`
	RedemptionCount int  `json:"redemption_count"`

	Active bool `json:"active"`

	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"-"`
	Version   int        `json:"-"`
}

// PromoRedemption is one patient's use of one code against one payment.
type PromoRedemption struct {
	ID          uuid.UUID `json:"id"`
	PromoCodeID uuid.UUID `json:"promo_code_id"`
	Code        string    `json:"code"`

	UserID        uuid.UUID `json:"user_id"`
	PaymentID     uuid.UUID `json:"payment_id"`
	AppointmentID uuid.UUID `json:"appointment_id"`

	DiscountCents int64  `json:"discount_cents"`
	Currency      string `json:"currency"`

	Status        PromoRedemptionStatus `json:"status"`
	ReservedAt    time.Time             `json:"reserved_at"`
	ExpiresAt     time.Time             `json:"expires_at"`
	ConsumedAt    *time.Time            `json:"consumed_at,omitempty"`
	ReleasedAt    *time.Time            `json:"released_at,omitempty"`
	ReleaseReason string                `json:"release_reason,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Version   int       `json:"-"`
}

// OrderSummary is what the payment screen renders. It is deliberately
// server-computed in full: the client never subtracts a discount itself,
// because a client that computes its own total is a client that can be made
// to compute the wrong one.
type OrderSummary struct {
	PaymentID     uuid.UUID `json:"payment_id"`
	AppointmentID uuid.UUID `json:"appointment_id"`

	ConsultationFeeCents int64  `json:"consultation_fee_cents"`
	DiscountCents        int64  `json:"discount_cents"`
	TotalCents           int64  `json:"total_cents"`
	Currency             string `json:"currency"`

	AppliedPromoCode string     `json:"applied_promo_code,omitempty"`
	PromoExpiresAt   *time.Time `json:"promo_expires_at,omitempty"`
}

// --- errors ----------------------------------------------------------------

var (
	// ErrPromoUnknown means no such code, or it has been deactivated.
	ErrPromoUnknown = errors.New("payment: promo code not found")
	// ErrPromoExpired means the code is outside its validity window.
	ErrPromoExpired = errors.New("payment: promo code is not currently valid")
	// ErrPromoExhausted means the code's own budget, or this patient's share
	// of it, is used up.
	ErrPromoExhausted = errors.New("payment: promo code has no redemptions left")
	// ErrPromoNotApplicable means the code exists and has budget but does not
	// apply to this payment: wrong currency, below the minimum spend, or the
	// payment is already past the point where its amount may change.
	ErrPromoNotApplicable = errors.New("payment: promo code does not apply to this payment")
	// ErrNoPromoApplied is returned by Remove when there is nothing to remove.
	ErrNoPromoApplied = errors.New("payment: no promo code is applied to this payment")
)

// minChargeableCents is the floor a discount may not take a payment below.
//
// A promotion that reduces the total to zero is not a cheap consultation, it
// is a *free* one, and a free consultation is not a payment at all: no rail
// will accept a zero-value charge, the ledger's amount_cents > 0 CHECK refuses
// a zero leg, and the whole capture path is built around money moving. Making
// that work means letting the booking flow skip payment entirely, which is a
// scheduling-service change and a product decision, not a promo-code feature.
// Until that exists, a code that would zero the bill is refused with a clear
// error instead of silently capping itself and charging the patient one cent.
const minChargeableCents int64 = 100 // LKR 1.00

// NormalizePromoCode upper-cases and strips whitespace, so "welcome 20" and
// "WELCOME20" are one code. It also rejects anything that is not
// alphanumeric, hyphen or underscore, which keeps a lookup from being handed
// a megabyte of Unicode.
func NormalizePromoCode(raw string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		switch {
		case unicode.IsSpace(r):
			continue
		case r == '-' || r == '_':
			b.WriteRune(r)
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(unicode.ToUpper(r))
		default:
			return "" // anything else is not a code we could have issued
		}
	}
	return b.String()
}

// DiscountFor computes what this code takes off amountCents, in cents, with
// no floating point anywhere.
//
// It returns 0 when the code does not apply to this amount at all. Callers
// treat 0 as "not applicable" rather than "a zero discount", because a code
// that computes to nothing is not a code the patient should see applied.
func (p PromoCode) DiscountFor(amountCents int64) int64 {
	if amountCents <= 0 || amountCents < p.MinAmountCents {
		return 0
	}

	var off int64
	switch p.DiscountType {
	case PromoPercent:
		// Integer basis points, rounded half up, exactly like ComputeSplit.
		// (amount * bps + 5000) / 10000 rounds .5 away from zero for the
		// non-negative amounts this function is only ever given.
		off = (amountCents*int64(p.PercentBps) + 5000) / 10000
		if p.MaxDiscountCents > 0 && off > p.MaxDiscountCents {
			off = p.MaxDiscountCents
		}
	case PromoFixed:
		off = p.AmountOffCents
	default:
		return 0
	}

	if off > amountCents {
		off = amountCents
	}
	return off
}

// Redeemable reports whether the code is live at time now, ignoring budget
// (which needs a database read) and the specific payment.
func (p PromoCode) Redeemable(now time.Time) error {
	if !p.Active || p.DeletedAt != nil {
		return ErrPromoUnknown
	}
	if now.Before(p.ValidFrom) {
		return ErrPromoExpired
	}
	if p.ValidUntil != nil && !now.Before(*p.ValidUntil) {
		return ErrPromoExpired
	}
	return nil
}

// --- service ---------------------------------------------------------------

// PromoConfig tunes reservation lifetime.
type PromoConfig struct {
	// ReservationTTL is how long an applied-but-unpaid code is held for the
	// patient before anyone else may have it. Long enough to finish a
	// checkout on a slow connection, short enough that a limited code is not
	// hostage to abandoned carts.
	ReservationTTL time.Duration
}

func (c PromoConfig) withDefaults() PromoConfig {
	if c.ReservationTTL <= 0 {
		// Long enough to finish a checkout on a bad connection, short enough
		// that a limited promotion is not hostage to abandoned carts. It is
		// also a floor on how stale a quote the rail can be handed: refreshed
		// on every intent creation, so the charge can never outlive the hold.
		c.ReservationTTL = 30 * time.Minute
	}
	return c
}

// ApplyPromoInput is the request to put a code on a payment.
type ApplyPromoInput struct {
	AppointmentID uuid.UUID
	CallerID      uuid.UUID
	CallerIsOps   bool
	Code          string
}

// ApplyPromo reserves a promo code against the caller's pending payment and
// returns the recomputed order summary.
//
// It is safe to call repeatedly: applying the same code twice is a no-op that
// returns the same summary, and applying a different code releases the first
// reservation in the same transaction that takes the second. There is never a
// moment where the patient holds two codes or none.
func (s *Service) ApplyPromo(ctx context.Context, in ApplyPromoInput) (OrderSummary, error) {
	code := NormalizePromoCode(in.Code)
	if code == "" {
		return OrderSummary{}, ErrPromoUnknown
	}

	pay, err := s.store.GetPaymentByAppointment(ctx, in.AppointmentID)
	if errors.Is(err, ErrNotFound) {
		return OrderSummary{}, ErrPaymentNotReady
	}
	if err != nil {
		return OrderSummary{}, err
	}
	if !in.CallerIsOps && pay.PatientID != in.CallerID {
		return OrderSummary{}, ErrForbidden
	}

	now := s.now().UTC()
	var out OrderSummary

	err = s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		locked, err := tx.LockPayment(ctx, pay.ID)
		if err != nil {
			return err
		}
		if err := promoMutable(locked); err != nil {
			return err
		}

		existing, hasExisting, err := tx.FindLiveRedemptionForPayment(ctx, locked.ID)
		if err != nil {
			return err
		}
		if hasExisting && existing.Status == PromoConsumed {
			// The money already moved with this code on it. Nothing to change.
			return ErrPromoNotApplicable
		}

		target, err := tx.FindPromoCode(ctx, code)
		if errors.Is(err, ErrNotFound) {
			return ErrPromoUnknown
		}
		if err != nil {
			return err
		}

		// Lock every code row involved, in id order, so a pair of concurrent
		// swaps between the same two codes queues instead of deadlocking.
		ids := []uuid.UUID{target.ID}
		if hasExisting && existing.PromoCodeID != target.ID {
			ids = append(ids, existing.PromoCodeID)
		}
		locks, err := lockCodesInOrder(ctx, tx, ids)
		if err != nil {
			return err
		}
		target = locks[target.ID]

		// Self-heal before deciding: a code held by abandoned checkouts is
		// released here, by the next person to try it, so a limited promotion
		// cannot be permanently burned by patients who never paid.
		for id := range locks {
			if _, err := tx.ExpireStaleReservations(ctx, id, now); err != nil {
				return err
			}
		}
		target, err = tx.LockPromoCodeByID(ctx, target.ID)
		if err != nil {
			return err
		}

		if hasExisting {
			if existing.PromoCodeID == target.ID {
				// Re-applying the code already on the payment. Refresh the
				// hold so a patient who is still typing does not lose it, and
				// return the summary unchanged.
				if err := tx.TouchRedemption(ctx, existing.ID, now.Add(s.promo.ReservationTTL)); err != nil {
					return err
				}
				out = summaryFor(locked, existing.ExpiresAt)
				return nil
			}
			if err := releaseRedemption(ctx, tx, existing, ReleaseReplaced, now); err != nil {
				return err
			}
		}

		if err := target.Redeemable(now); err != nil {
			return err
		}
		if !strings.EqualFold(target.Currency, locked.Currency) {
			return ErrPromoNotApplicable
		}

		discount := target.DiscountFor(locked.GrossAmountCents)
		if discount <= 0 {
			return ErrPromoNotApplicable
		}
		if locked.GrossAmountCents-discount < minChargeableCents {
			return fmt.Errorf("%w: this code would reduce the total below the minimum chargeable amount", ErrPromoNotApplicable)
		}

		// Budget. Both checks happen while holding the code's row lock, which
		// is what makes "read the count, then insert" correct.
		if target.MaxRedemptions != nil && target.RedemptionCount >= *target.MaxRedemptions {
			return ErrPromoExhausted
		}
		mine, err := tx.CountLiveRedemptions(ctx, target.ID, locked.PatientID)
		if err != nil {
			return err
		}
		if mine >= target.MaxPerUser {
			return ErrPromoExhausted
		}

		red := PromoRedemption{
			ID:            uuid.New(),
			PromoCodeID:   target.ID,
			Code:          target.Code,
			UserID:        locked.PatientID,
			PaymentID:     locked.ID,
			AppointmentID: locked.AppointmentID,
			DiscountCents: discount,
			Currency:      locked.Currency,
			Status:        PromoReserved,
			ReservedAt:    now,
			ExpiresAt:     now.Add(s.promo.ReservationTTL),
		}
		if err := tx.InsertRedemption(ctx, &red); err != nil {
			return err
		}
		// The CHECK on promo_codes.redemption_count fires here if the budget
		// was somehow exceeded, which is the backstop under the row lock.
		if err := tx.AdjustRedemptionCount(ctx, target.ID, +1); err != nil {
			return err
		}

		locked.DiscountCents = discount
		locked.AmountCents = locked.GrossAmountCents - discount
		locked.PromoCode = target.Code
		if !locked.DiscountBalances() {
			return fmt.Errorf("payment: discount does not balance for %s: %d != %d + %d",
				locked.ID, locked.GrossAmountCents, locked.AmountCents, locked.DiscountCents)
		}
		if err := tx.UpdatePayment(ctx, &locked); err != nil {
			return err
		}

		out = summaryFor(locked, red.ExpiresAt)
		return nil
	})
	if err != nil {
		return OrderSummary{}, err
	}
	return out, nil
}

// RemovePromoInput is the request to clear the code on a payment.
type RemovePromoInput struct {
	AppointmentID uuid.UUID
	CallerID      uuid.UUID
	CallerIsOps   bool
}

// RemovePromo takes the code back off a payment and returns the code to its
// pool. It exists because a patient who typed the wrong code and cannot undo
// it will abandon the booking rather than pay a price they did not expect.
func (s *Service) RemovePromo(ctx context.Context, in RemovePromoInput) (OrderSummary, error) {
	pay, err := s.store.GetPaymentByAppointment(ctx, in.AppointmentID)
	if errors.Is(err, ErrNotFound) {
		return OrderSummary{}, ErrPaymentNotReady
	}
	if err != nil {
		return OrderSummary{}, err
	}
	if !in.CallerIsOps && pay.PatientID != in.CallerID {
		return OrderSummary{}, ErrForbidden
	}

	now := s.now().UTC()
	var out OrderSummary

	err = s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		locked, err := tx.LockPayment(ctx, pay.ID)
		if err != nil {
			return err
		}
		if err := promoMutable(locked); err != nil {
			return err
		}

		existing, ok, err := tx.FindLiveRedemptionForPayment(ctx, locked.ID)
		if err != nil {
			return err
		}
		if !ok || existing.Status != PromoReserved {
			return ErrNoPromoApplied
		}
		if _, err := lockCodesInOrder(ctx, tx, []uuid.UUID{existing.PromoCodeID}); err != nil {
			return err
		}
		if err := releaseRedemption(ctx, tx, existing, ReleaseRemoved, now); err != nil {
			return err
		}

		locked.DiscountCents = 0
		locked.PromoCode = ""
		locked.AmountCents = locked.GrossAmountCents
		if err := tx.UpdatePayment(ctx, &locked); err != nil {
			return err
		}
		out = summaryFor(locked, time.Time{})
		return nil
	})
	if err != nil {
		return OrderSummary{}, err
	}
	return out, nil
}

// OrderSummaryFor returns the current summary without changing anything, so
// the payment screen can render the total it is about to charge.
func (s *Service) OrderSummaryFor(ctx context.Context, appointmentID, callerID uuid.UUID, ops bool) (OrderSummary, error) {
	pay, err := s.store.GetPaymentByAppointment(ctx, appointmentID)
	if errors.Is(err, ErrNotFound) {
		return OrderSummary{}, ErrPaymentNotReady
	}
	if err != nil {
		return OrderSummary{}, err
	}
	if !ops && pay.PatientID != callerID {
		return OrderSummary{}, ErrForbidden
	}
	return summaryFor(pay, time.Time{}), nil
}

// SweepExpiredPromoReservations releases reservations nobody paid for.
//
// It is a tidiness and reporting job, not a correctness dependency: Apply
// releases a code's own stale reservations before it reads the budget, so a
// code is never permanently burned even if this never runs. What the sweeper
// adds is that a reservation reaches a terminal status with a timestamp on
// it, so "how many people applied this code and did not pay" is answerable.
func (s *Service) SweepExpiredPromoReservations(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 200
	}
	now := s.now().UTC()

	ids, err := s.store.ExpiredReservationIDs(ctx, now, limit)
	if err != nil {
		return 0, err
	}

	released := 0
	for _, id := range ids {
		err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
			red, ok, err := tx.LockRedemption(ctx, id)
			if err != nil || !ok {
				return err
			}
			if red.Status != PromoReserved || red.ExpiresAt.After(now) {
				return nil // somebody paid, or refreshed the hold, in the meantime
			}
			if _, err := lockCodesInOrder(ctx, tx, []uuid.UUID{red.PromoCodeID}); err != nil {
				return err
			}
			if err := releaseRedemption(ctx, tx, red, ReleaseExpired, now); err != nil {
				return err
			}

			// The payment keeps its gross price back, so a patient who
			// returns after the hold lapsed is quoted the real fee rather
			// than a discount they no longer hold.
			pay, err := tx.LockPayment(ctx, red.PaymentID)
			if err != nil {
				return err
			}
			if pay.Status.Settled() || pay.PromoCode != red.Code {
				return nil
			}
			pay.DiscountCents = 0
			pay.PromoCode = ""
			pay.AmountCents = pay.GrossAmountCents
			return tx.UpdatePayment(ctx, &pay)
		})
		if err != nil {
			return released, err
		}
		released++
	}
	return released, nil
}

// consumePromo marks the payment's reservation spent. Called from inside the
// capture transaction, so a promotion is consumed if and only if the money
// was taken.
func consumePromo(ctx context.Context, tx Tx, paymentID uuid.UUID, at time.Time) error {
	red, ok, err := tx.FindLiveRedemptionForPayment(ctx, paymentID)
	if err != nil || !ok || red.Status != PromoReserved {
		return err
	}
	return tx.SetRedemptionStatus(ctx, red.ID, PromoConsumed, "", at)
}

// releasePromoFor returns a reserved code to its pool. Called when a payment
// fails or its appointment is cancelled: the patient did not get the
// consultation, so they keep the promotion.
func releasePromoFor(ctx context.Context, tx Tx, paymentID uuid.UUID, reason string, at time.Time) error {
	red, ok, err := tx.FindLiveRedemptionForPayment(ctx, paymentID)
	if err != nil || !ok || red.Status != PromoReserved {
		return err
	}
	if _, err := lockCodesInOrder(ctx, tx, []uuid.UUID{red.PromoCodeID}); err != nil {
		return err
	}
	return releaseRedemption(ctx, tx, red, reason, at)
}

// --- helpers ---------------------------------------------------------------

// promoMutable refuses to change the amount of a payment that is past the
// point where changing it is meaningful.
//
// The provider-intent check is the one that matters. Once an intent exists at
// the rail it is denominated in a fixed amount; moving ours underneath it
// means the patient is charged the old figure and shown the new one. So a
// promotion goes on before the intent is created, which is exactly the order
// the payment screen already uses -- apply the code, then tap Pay.
func promoMutable(p Payment) error {
	if p.Status.Settled() {
		return fmt.Errorf("%w: this consultation is already paid", ErrPromoNotApplicable)
	}
	if p.Status == StatusRequiresAction || p.Status == StatusRequiresPIN {
		return fmt.Errorf("%w: a payment is already in progress; cancel it before changing the price", ErrPromoNotApplicable)
	}
	if p.ProviderIntentID != "" {
		return fmt.Errorf("%w: the payment has already been quoted to the provider at %d %s",
			ErrPromoNotApplicable, p.AmountCents, p.Currency)
	}
	return nil
}

func summaryFor(p Payment, holdUntil time.Time) OrderSummary {
	out := OrderSummary{
		PaymentID:            p.ID,
		AppointmentID:        p.AppointmentID,
		ConsultationFeeCents: p.GrossAmountCents,
		DiscountCents:        p.DiscountCents,
		TotalCents:           p.AmountCents,
		Currency:             p.Currency,
		AppliedPromoCode:     p.PromoCode,
	}
	if !holdUntil.IsZero() {
		t := holdUntil.UTC()
		out.PromoExpiresAt = &t
	}
	return out
}

// lockCodesInOrder takes FOR UPDATE on every named code, lowest id first.
// The ordering is the deadlock defence; see the package comment above.
func lockCodesInOrder(ctx context.Context, tx Tx, ids []uuid.UUID) (map[uuid.UUID]PromoCode, error) {
	sorted := append([]uuid.UUID(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].String() < sorted[j].String() })

	out := make(map[uuid.UUID]PromoCode, len(sorted))
	for _, id := range sorted {
		if _, seen := out[id]; seen {
			continue
		}
		c, err := tx.LockPromoCodeByID(ctx, id)
		if err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, nil
}

// releaseRedemption flips one reservation to released and gives its slot back
// to the code. Both halves are on the caller's transaction and the caller
// already holds the code's row lock, so the counter cannot drift from the
// rows it counts.
func releaseRedemption(ctx context.Context, tx Tx, red PromoRedemption, reason string, at time.Time) error {
	if red.Status != PromoReserved {
		return nil
	}
	if err := tx.SetRedemptionStatus(ctx, red.ID, PromoReleased, reason, at); err != nil {
		return err
	}
	return tx.AdjustRedemptionCount(ctx, red.PromoCodeID, -1)
}

// --- administration --------------------------------------------------------

// CreatePromoCodeInput is an ops request to issue a promotion.
type CreatePromoCodeInput struct {
	Code             string
	Description      string
	DiscountType     PromoDiscountType
	PercentBps       int
	AmountOffCents   int64
	MaxDiscountCents int64
	MinAmountCents   int64
	Currency         string
	ValidFrom        *time.Time
	ValidUntil       *time.Time
	MaxRedemptions   *int
	MaxPerUser       int
}

// CreatePromoCode issues a promotion.
//
// This exists because a promo-code redemption engine with no way to create a
// promo code is not a feature, it is a schema. It is deliberately restricted
// to finance and super_admin at the transport layer: a code is a standing
// instruction to give money away.
func (s *Service) CreatePromoCode(ctx context.Context, in CreatePromoCodeInput) (PromoCode, error) {
	code := NormalizePromoCode(in.Code)
	if len(code) < 3 {
		return PromoCode{}, fmt.Errorf("%w: a code must be at least 3 characters of letters, digits, hyphen or underscore", ErrPromoNotApplicable)
	}

	currency := strings.ToUpper(strings.TrimSpace(in.Currency))
	if currency == "" {
		currency = s.cfg.Currency
	}
	from := s.now().UTC()
	if in.ValidFrom != nil {
		from = in.ValidFrom.UTC()
	}
	perUser := in.MaxPerUser
	if perUser <= 0 {
		perUser = 1
	}

	c := PromoCode{
		ID:               uuid.New(),
		Code:             code,
		Description:      in.Description,
		DiscountType:     in.DiscountType,
		PercentBps:       in.PercentBps,
		AmountOffCents:   in.AmountOffCents,
		MaxDiscountCents: in.MaxDiscountCents,
		MinAmountCents:   in.MinAmountCents,
		Currency:         currency,
		ValidFrom:        from,
		ValidUntil:       in.ValidUntil,
		MaxRedemptions:   in.MaxRedemptions,
		MaxPerUser:       perUser,
		Active:           true,
	}
	if err := c.validateShape(); err != nil {
		return PromoCode{}, err
	}

	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		return tx.InsertPromoCode(ctx, &c)
	})
	if err != nil {
		return PromoCode{}, err
	}
	return c, nil
}

// validateShape mirrors the promo_codes_shape and promo_codes_window CHECKs in
// Go, so an ops mistake comes back as a readable 422 rather than a constraint
// name.
func (c PromoCode) validateShape() error {
	switch c.DiscountType {
	case PromoPercent:
		if c.PercentBps <= 0 || c.PercentBps > 10000 {
			return fmt.Errorf("%w: a percentage code needs percent_bps between 1 and 10000", ErrPromoNotApplicable)
		}
		if c.AmountOffCents != 0 {
			return fmt.Errorf("%w: a percentage code must not also set amount_off_cents", ErrPromoNotApplicable)
		}
	case PromoFixed:
		if c.AmountOffCents <= 0 {
			return fmt.Errorf("%w: a fixed code needs amount_off_cents above zero", ErrPromoNotApplicable)
		}
		if c.PercentBps != 0 {
			return fmt.Errorf("%w: a fixed code must not also set percent_bps", ErrPromoNotApplicable)
		}
		if c.MaxDiscountCents != 0 {
			return fmt.Errorf("%w: max_discount_cents caps a percentage, not a fixed amount", ErrPromoNotApplicable)
		}
	default:
		return fmt.Errorf("%w: discount_type must be percent or fixed", ErrPromoNotApplicable)
	}
	if c.ValidUntil != nil && !c.ValidUntil.After(c.ValidFrom) {
		return fmt.Errorf("%w: valid_until must be after valid_from", ErrPromoNotApplicable)
	}
	if c.MaxRedemptions != nil && *c.MaxRedemptions <= 0 {
		return fmt.Errorf("%w: max_redemptions must be above zero, or absent for unlimited", ErrPromoNotApplicable)
	}
	if len(c.Currency) != 3 {
		return fmt.Errorf("%w: currency must be a three-letter ISO-4217 code", ErrPromoNotApplicable)
	}
	return nil
}

// ListPromoCodes is the ops view of what is on offer.
func (s *Service) ListPromoCodes(ctx context.Context, includeInactive bool, page Page) ([]PromoCode, int64, error) {
	return s.store.ListPromoCodes(ctx, includeInactive, page)
}

// DeactivatePromoCode stops a code being redeemed.
//
// Reservations already held are left alone on purpose: a patient part-way
// through a checkout keeps the price they were quoted. Withdrawing a
// promotion is a decision about the future, not a repricing of carts already
// in flight.
func (s *Service) DeactivatePromoCode(ctx context.Context, code string) error {
	normalised := NormalizePromoCode(code)
	if normalised == "" {
		return ErrPromoUnknown
	}
	return s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		return tx.DeactivatePromoCode(ctx, normalised)
	})
}
