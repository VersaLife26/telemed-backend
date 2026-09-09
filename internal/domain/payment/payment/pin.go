package payment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"telemed/internal/platform/events"
	"telemed/internal/platform/logger"
)

// Carrier-billing PIN challenges.
//
// Dialog Ideamart authorises a debit with a PIN sent to the subscriber's
// handset. That is three steps -- request PIN, patient types it, debit -- and
// the middle one lasts minutes, so it is a state and not a pause inside a
// function call.
//
// The payment already moved pending -> requires_pin -> succeeded|failed. What
// was missing was everything *about* the requires_pin state: when the PIN
// stops being valid at the carrier, how many times the patient has guessed,
// and which handset it went to. Without those:
//
//   - a PIN typed six minutes late was sent to the carrier anyway and came
//     back with an opaque status code the patient could make nothing of;
//   - one mistyped digit failed the whole payment, because the old code had
//     no notion of "wrong, try again" and mapped every provider rejection
//     straight to StatusFailed;
//   - nothing bounded guessing, and a four-digit PIN is 10,000 guesses.
//
// PINChallenge is that state, with an expiry and an attempt counter. The PIN
// itself is never stored, never hashed, never logged: it goes from the request
// body to the carrier and is discarded.

// PINChallengeStatus is the lifecycle of one challenge.
type PINChallengeStatus string

const (
	PINPending    PINChallengeStatus = "pending"
	PINConfirmed  PINChallengeStatus = "confirmed"
	PINFailed     PINChallengeStatus = "failed"
	PINExpired    PINChallengeStatus = "expired"
	PINSuperseded PINChallengeStatus = "superseded"
)

// PINChallenge is one outstanding SMS authorisation.
type PINChallenge struct {
	ID        uuid.UUID    `json:"id"`
	PaymentID uuid.UUID    `json:"payment_id"`
	Provider  ProviderName `json:"provider"`

	// Reference is the carrier's correlation id for this challenge. It is not
	// a secret and it is not the PIN; it is what the debit is made against.
	Reference string `json:"-"`
	// MaskedMSISDN is already masked when it gets here.
	MaskedMSISDN string `json:"masked_msisdn,omitempty"`

	Status      PINChallengeStatus `json:"status"`
	Attempts    int                `json:"attempts"`
	MaxAttempts int                `json:"max_attempts"`

	ExpiresAt     time.Time  `json:"expires_at"`
	ConfirmedAt   *time.Time `json:"confirmed_at,omitempty"`
	SettledAt     *time.Time `json:"settled_at,omitempty"`
	FailureReason string     `json:"failure_reason,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Version   int       `json:"-"`
}

// AttemptsRemaining is what the client renders under the PIN field.
func (c PINChallenge) AttemptsRemaining() int {
	n := c.MaxAttempts - c.Attempts
	if n < 0 {
		return 0
	}
	return n
}

// Live reports whether the challenge can still be answered.
func (c PINChallenge) Live(now time.Time) bool {
	return c.Status == PINPending && now.Before(c.ExpiresAt) && c.Attempts < c.MaxAttempts
}

// PINChallengeView is the client-facing shape. It carries no reference and no
// PIN: everything the patient's app needs is the deadline and how many tries
// are left.
type PINChallengeView struct {
	PaymentID         uuid.UUID `json:"payment_id"`
	AppointmentID     uuid.UUID `json:"appointment_id"`
	MaskedMSISDN      string    `json:"masked_msisdn,omitempty"`
	ExpiresAt         time.Time `json:"expires_at"`
	AttemptsRemaining int       `json:"attempts_remaining"`
	AmountCents       int64     `json:"amount_cents"`
	Currency          string    `json:"currency"`
}

func toPINChallengeView(c PINChallenge, p Payment) PINChallengeView {
	return PINChallengeView{
		PaymentID:         p.ID,
		AppointmentID:     p.AppointmentID,
		MaskedMSISDN:      c.MaskedMSISDN,
		ExpiresAt:         c.ExpiresAt,
		AttemptsRemaining: c.AttemptsRemaining(),
		AmountCents:       p.AmountCents,
		Currency:          p.Currency,
	}
}

// PINConfig tunes the challenge window.
type PINConfig struct {
	// TTL is how long a challenge may be answered for. It must not exceed the
	// carrier's own PIN lifetime, or the platform will accept a code the
	// carrier has already forgotten and fail at the debit instead of at the
	// door. Ideamart's documented window is five minutes.
	TTL time.Duration
	// MaxAttempts is how many guesses a patient gets before the challenge --
	// and with it the payment -- fails. Three is the telco convention.
	MaxAttempts int
	// MaxResends bounds how many challenges may be opened against ONE payment
	// inside ResendWindow.
	//
	// MaxAttempts is per CHALLENGE, and a challenge was free to reopen:
	// openPINChallenge supersedes whatever was pending and inserts a fresh row
	// with Attempts back at zero. Two things followed. The 3-guess allowance
	// against a 4-digit PIN was multiplied by however many times the caller
	// re-requested, and every request drives a real carrier SMS to a phone
	// number taken from the request body -- this service does not hold the
	// patient's number, by design, so it cannot check it belongs to them. At
	// the 120/min per-principal rate limit that is roughly 40 SMS a minute to
	// an arbitrary handset, at platform cost.
	MaxResends int
	// ResendWindow is the period MaxResends is counted over.
	ResendWindow time.Duration
}

func (c PINConfig) withDefaults() PINConfig {
	if c.TTL <= 0 {
		c.TTL = 5 * time.Minute
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.MaxResends <= 0 {
		// Generous for a patient who mistyped their number once and did not
		// receive the first SMS; nowhere near an SMS pump.
		c.MaxResends = 5
	}
	if c.ResendWindow <= 0 {
		c.ResendWindow = time.Hour
	}
	return c
}

// ErrPINResendLimit means too many PIN challenges have been opened against one
// payment. It is deliberately distinct from ErrPINAttemptsExceeded: that one
// means the guesses ran out, this one means the SMSes did.
var ErrPINResendLimit = errors.New("payment: too many PIN requests for this payment")

// --- errors ----------------------------------------------------------------

var (
	// ErrPINNotOutstanding means no challenge is waiting to be answered.
	ErrPINNotOutstanding = errors.New("payment: no PIN challenge is outstanding for this payment")
	// ErrPINExpired means the carrier's PIN window has closed. The payment is
	// returned to pending so a new PIN can be requested.
	ErrPINExpired = errors.New("payment: the PIN has expired; request a new one")
	// ErrPINInvalid means the carrier rejected the code and the patient has
	// attempts left.
	ErrPINInvalid = errors.New("payment: that PIN is not correct")
	// ErrPINAttemptsExceeded means the allowance is used up and the payment
	// has failed.
	ErrPINAttemptsExceeded = errors.New("payment: too many incorrect PIN attempts")
)

// --- request -------------------------------------------------------------

// RequestPINInput asks the carrier to send a PIN to the patient's handset.
type RequestPINInput struct {
	AppointmentID uuid.UUID
	CallerID      uuid.UUID
	CallerIsOps   bool
	// Phone is the patient's E.164 mobile number. Carrier billing cannot work
	// without it, and this service does not hold one -- user-service does --
	// so the client supplies it and it is validated, never logged unmasked,
	// and never persisted unmasked.
	Phone string
}

// RequestPIN opens (or re-opens) a carrier-billing challenge for an
// appointment's payment.
//
// It delegates to CreateIntent rather than reimplementing it, so the pricing
// freeze, the idempotency key and the provider-registry lookup are the same
// code that every other rail goes through. CreateIntent records the challenge
// row when the rail reports requires_pin, which means `POST /payments/intent`
// with provider=dialog and this endpoint produce identical state -- there is
// no second, subtly different carrier-billing path.
func (s *Service) RequestPIN(ctx context.Context, in RequestPINInput) (PINChallengeView, error) {
	if in.Phone == "" {
		return PINChallengeView{}, fmt.Errorf("%w: carrier billing needs the patient's mobile number", ErrProviderRejected)
	}

	view, err := s.CreateIntent(ctx, CreateIntentInput{
		AppointmentID: in.AppointmentID,
		CallerID:      in.CallerID,
		CallerIsOps:   in.CallerIsOps,
		Provider:      ProviderDialog,
		PatientPhone:  in.Phone,
	})
	if err != nil {
		return PINChallengeView{}, err
	}
	if view.Payment.Status.Settled() {
		return PINChallengeView{}, ErrAlreadySettled
	}
	if view.PINChallenge == nil {
		// The rail reported something other than requires_pin. That is a
		// provider contract violation for this rail, not a client error.
		return PINChallengeView{}, fmt.Errorf("%w: the carrier did not issue a PIN challenge", ErrProviderUnavailable)
	}
	return *view.PINChallenge, nil
}

// openPINChallenge records a freshly issued challenge, superseding whatever
// was outstanding. It runs on the caller's transaction, so the payment
// reaching requires_pin and the challenge existing are one atomic fact --
// there is no window in which the status says "waiting for a PIN" and nothing
// records which PIN.
func (s *Service) openPINChallenge(ctx context.Context, tx Tx, p Payment, res IntentResult, maskedMSISDN string, now time.Time) (PINChallenge, error) {
	// Bound the resends before superseding anything. See PINConfig.MaxResends:
	// the 3-guess allowance is per challenge and a challenge was free to
	// reopen, so this is both the brute-force multiplier and the SMS pump.
	opened, err := tx.CountPINChallengesSince(ctx, p.ID, now.Add(-s.pin.ResendWindow))
	if err != nil {
		return PINChallenge{}, err
	}
	if opened >= s.pin.MaxResends {
		return PINChallenge{}, fmt.Errorf("%w: %d requests in %s", ErrPINResendLimit, opened, s.pin.ResendWindow)
	}

	if _, err := tx.SupersedePendingPINChallenges(ctx, p.ID, now); err != nil {
		return PINChallenge{}, err
	}

	expires := now.Add(s.pin.TTL)
	// The carrier's own deadline wins when it is the earlier of the two: a
	// challenge we would still accept but the carrier has forgotten is worse
	// than one we refuse a moment early.
	if res.ExpiresAt != nil && !res.ExpiresAt.IsZero() && res.ExpiresAt.Before(expires) {
		expires = res.ExpiresAt.UTC()
	}

	reference := res.Reference
	if reference == "" {
		reference = res.ProviderIntentID
	}
	if reference == "" {
		return PINChallenge{}, fmt.Errorf("%w: the carrier issued a PIN with no reference to debit against", ErrProviderUnavailable)
	}

	c := PINChallenge{
		ID:           uuid.New(),
		PaymentID:    p.ID,
		Provider:     p.Provider,
		Reference:    reference,
		MaskedMSISDN: maskedMSISDN,
		Status:       PINPending,
		MaxAttempts:  s.pin.MaxAttempts,
		ExpiresAt:    expires,
	}
	if err := tx.InsertPINChallenge(ctx, &c); err != nil {
		return PINChallenge{}, err
	}
	return c, nil
}

// --- confirm ---------------------------------------------------------------

// ConfirmPIN completes a Dialog carrier-billing payment.
//
// The shape is three phases, in this order, and the order is the whole point:
//
//  1. a transaction that validates the challenge and *durably counts the
//     attempt* before anything is sent to the carrier;
//  2. the carrier call, outside any transaction, because it is network I/O
//     that can take seconds and must not hold a row lock;
//  3. a transaction that applies the outcome.
//
// Counting the attempt first means a crash between phases costs the patient
// one guess rather than giving them unlimited ones -- the safe direction. The
// one exception is a carrier that never answered: phase 3 gives that attempt
// back, because no guess reached anybody and charging the patient for our
// timeout is not defensible.
//
// Every phase distinguishes an *error* (roll the transaction back) from an
// *outcome* (commit what was written, then tell the caller). An expired
// challenge, an exhausted allowance and a wrong PIN are all outcomes: the
// attempt counter, the retired challenge and the failed payment are exactly
// the state that has to survive, so they are never carried out of the closure
// as an error.
func (s *Service) ConfirmPIN(ctx context.Context, in ConfirmPINInput) (Payment, error) {
	p, err := s.store.GetPayment(ctx, in.PaymentID)
	if err != nil {
		return Payment{}, err
	}
	if !in.CallerIsOps && p.PatientID != in.CallerID {
		return Payment{}, ErrForbidden
	}
	if p.Status.Settled() {
		return p, nil // already paid; typing the PIN twice is harmless
	}

	// --- phase 1: claim an attempt ---------------------------------------
	//
	// The challenge is found before the rail is resolved, deliberately. A
	// payment carrying a live challenge is on a PIN rail by construction --
	// only a PIN rail can have issued one -- so the challenge, not the
	// payment's provider column, is the authority on which rail to debit.
	// Resolving the rail first meant a card payment sent to this endpoint
	// answered "this provider does not do PINs" (501) when the true and far
	// more useful answer is "there is no PIN outstanding here" (409).
	now := s.now().UTC()
	var (
		challenge PINChallenge
		outcome   error
		settled   bool
	)
	err = s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		locked, err := tx.LockPayment(ctx, p.ID)
		if err != nil {
			return err
		}
		p = locked
		if locked.Status.Settled() {
			settled = true
			return nil
		}

		c, found, err := tx.LockPendingPINChallenge(ctx, locked.ID)
		if err != nil {
			return err
		}
		if !found {
			outcome = ErrPINNotOutstanding
			return nil
		}

		if !now.Before(c.ExpiresAt) {
			// Expired. Retire the challenge and put the payment back where a
			// new PIN can be requested from, rather than leaving it parked in
			// requires_pin with nothing that can move it.
			c.Status = PINExpired
			c.FailureReason = "pin expired before it was entered"
			if err := tx.UpdatePINChallenge(ctx, &c); err != nil {
				return err
			}
			if err := s.resetForNewChallenge(ctx, tx, &locked); err != nil {
				return err
			}
			p, outcome = locked, ErrPINExpired
			return nil
		}
		if c.Attempts >= c.MaxAttempts {
			if err := s.failChallenge(ctx, tx, &locked, &c, "too many incorrect PIN attempts", now); err != nil {
				return err
			}
			p, outcome = locked, ErrPINAttemptsExceeded
			return nil
		}

		c.Attempts++
		if err := tx.UpdatePINChallenge(ctx, &c); err != nil {
			return err
		}
		challenge = c
		return nil
	})
	switch {
	case err != nil:
		return Payment{}, err
	case settled:
		return p, nil
	case outcome != nil:
		return p, outcome
	}

	// --- phase 2: the carrier --------------------------------------------
	rail, err := s.providers.Get(challenge.Provider)
	if err != nil {
		return Payment{}, err
	}
	pinRail, ok := rail.(PINProvider)
	if !ok {
		return Payment{}, fmt.Errorf("%w: %s does not use a PIN flow", ErrUnsupported, challenge.Provider)
	}

	res, confirmErr := pinRail.ConfirmPIN(ctx, PINConfirmation{
		PaymentID:      p.ID,
		Reference:      challenge.Reference,
		PIN:            in.PIN,
		AmountCents:    p.AmountCents,
		Currency:       p.Currency,
		IdempotencyKey: p.IdempotencyKey,
	})

	// --- phase 3: apply ---------------------------------------------------
	settledAt := s.now().UTC()
	out := p
	outcome = nil
	err = s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		locked, err := tx.LockPayment(ctx, p.ID)
		if err != nil {
			return err
		}
		out = locked
		if locked.Status.Settled() {
			return nil // a webhook won the race; keep its truth
		}
		c, found, err := tx.LockPendingPINChallenge(ctx, locked.ID)
		if err != nil {
			return err
		}
		if !found {
			// Somebody superseded the challenge while the carrier was
			// thinking. Do not act on a result for a challenge that is no
			// longer the live one.
			outcome = ErrPINNotOutstanding
			return nil
		}

		switch {
		case confirmErr == nil:
			c.Status = PINConfirmed
			c.ConfirmedAt = &settledAt
			c.SettledAt = &settledAt
			if err := tx.UpdatePINChallenge(ctx, &c); err != nil {
				return err
			}
			if res.ProviderIntentID != "" {
				locked.ProviderIntentID = res.ProviderIntentID
			}
			if res.Status.Status() != StatusSucceeded {
				// The carrier verified the PIN but did not settle. Leave the
				// payment where the rail says it is rather than inventing a
				// capture nobody made.
				locked.Status = res.Status.Status()
				if err := tx.UpdatePayment(ctx, &locked); err != nil {
					return err
				}
				out = locked
				return nil
			}
			if err := s.applyCapture(ctx, tx, &locked, res.ProviderFeeCents); err != nil {
				return err
			}
			out = locked
			return nil

		case errors.Is(confirmErr, ErrProviderUnavailable):
			// No guess reached the carrier. Give the attempt back: the
			// patient must not pay for our timeout.
			if c.Attempts > 0 {
				c.Attempts--
			}
			if err := tx.UpdatePINChallenge(ctx, &c); err != nil {
				return err
			}
			outcome = confirmErr
			return nil

		default:
			// The carrier decided, and the answer was no.
			if c.Attempts < c.MaxAttempts {
				outcome = ErrPINInvalid
				return nil
			}
			if err := s.failChallenge(ctx, tx, &locked, &c, "too many incorrect PIN attempts", settledAt); err != nil {
				return err
			}
			out, outcome = locked, ErrPINAttemptsExceeded
			return nil
		}
	})
	if err != nil {
		return Payment{}, err
	}
	return out, outcome
}

// resetForNewChallenge returns a payment to pending so a fresh PIN can be
// requested, clearing the provider handles that belonged to the dead
// challenge. Leaving them set would make promoMutable and CreateIntent both
// believe a live quote exists at the carrier when none does.
func (s *Service) resetForNewChallenge(ctx context.Context, tx Tx, p *Payment) error {
	p.Status = StatusPending
	p.ProviderIntentID = ""
	p.ProviderReference = ""
	return tx.UpdatePayment(ctx, p)
}

// failChallenge fails both the challenge and the payment, releases any promo
// code the patient was holding, and announces the failure.
func (s *Service) failChallenge(ctx context.Context, tx Tx, p *Payment, c *PINChallenge, reason string, at time.Time) error {
	c.Status = PINFailed
	c.FailureReason = reason
	c.SettledAt = &at
	if err := tx.UpdatePINChallenge(ctx, c); err != nil {
		return err
	}

	p.Status = StatusFailed
	p.FailureReason = reason
	if err := tx.UpdatePayment(ctx, p); err != nil {
		return err
	}
	if err := releasePromoFor(ctx, tx, p.ID, ReleasePaymentFail, at); err != nil {
		return err
	}
	return tx.Enqueue(ctx, events.SubjectPaymentFailed, p.ID.String(), s.paymentEvent(*p))
}

// --- maintenance -----------------------------------------------------------

// SweepExpiredPINChallenges retires challenges nobody answered.
//
// ConfirmPIN already refuses an expired challenge, so this is not what makes
// the expiry correct. What it adds is that a patient who walked away leaves a
// payment in `pending` rather than one parked in `requires_pin` forever, which
// is the difference between an operator seeing "abandoned" and seeing a queue
// of payments that look stuck.
func (s *Service) SweepExpiredPINChallenges(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 200
	}
	now := s.now().UTC()

	ids, err := s.store.ExpiredPINChallengeIDs(ctx, now, limit)
	if err != nil {
		return 0, err
	}

	swept := 0
	for _, id := range ids {
		err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
			c, found, err := tx.LockPINChallenge(ctx, id)
			if err != nil || !found {
				return err
			}
			if c.Status != PINPending || c.ExpiresAt.After(now) {
				return nil
			}
			c.Status = PINExpired
			c.FailureReason = "pin expired before it was entered"
			if err := tx.UpdatePINChallenge(ctx, &c); err != nil {
				return err
			}

			pay, err := tx.LockPayment(ctx, c.PaymentID)
			if err != nil {
				return err
			}
			if pay.Status != StatusRequiresPIN {
				return nil
			}
			pay.Status = StatusPending
			pay.ProviderIntentID = ""
			pay.ProviderReference = ""
			return tx.UpdatePayment(ctx, &pay)
		})
		if err != nil {
			return swept, err
		}
		swept++
	}
	return swept, nil
}

// maskPhone is the one place a phone number is turned into something safe to
// persist or log. It exists so no caller in this package is tempted to write
// the raw value into a column "just for support".
func maskPhone(phone string) string { return logger.MaskPhone(phone) }
