package analytics

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"

	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
)

// DurableName is the JetStream durable consumer name. Fixed and stable: a
// rename starts a brand-new consumer at DeliverAllPolicy and replays the
// platform's entire event history through this projection.
//
// That replay is actually SAFE here -- every write is last-writer-wins on a
// natural key and every rollup is recomputed rather than incremented -- which
// is the property that makes rebuilding the projection from scratch a viable
// recovery procedure rather than a corruption event. See docs/RUNBOOK.md.
// DurableName must not contain "." -- JetStream rejects a durable whose name
// contains a subject-token separator, and the consumer then fails to start.
// That failure was silent in effect: the projection never ran, so
// ?available=now returned nothing against 1,577 real slots.
const DurableName = "doctor-service_analytics-projection"

var uuidZero uuid.UUID

// Consumer folds the platform's appointment, consultation and money events into
// the per-doctor analytics projection.
type Consumer struct {
	pool database.Pool
	repo *Repository
	loc  *time.Location
	log  zerolog.Logger
}

// NewConsumer builds a Consumer. loc is the business timezone (Asia/Colombo),
// used to bucket instants into the calendar days and hours a doctor recognises
// as their own working week.
func NewConsumer(pool database.Pool, loc *time.Location, log zerolog.Logger) *Consumer {
	return &Consumer{pool: pool, repo: NewRepository(), loc: loc, log: log}
}

// Subjects lists what this projection consumes.
//
// payout.sent is here alongside the five obvious ones because "payout status"
// on the earnings screen is otherwise unanswerable. Without it the honest reply
// for every period is "unknown", and a screen that renders "pending" for money
// already in the doctor's bank generates a support ticket the platform cannot
// resolve.
func (c *Consumer) Subjects() []events.Subject {
	return []events.Subject{
		events.SubjectAppointmentCompleted,
		events.SubjectAppointmentNoShow,
		events.SubjectAppointmentCancelled,
		events.SubjectConsultationEnded,
		events.SubjectPaymentSucceeded,
		events.SubjectPayoutSent,
	}
}

// Run subscribes and blocks until ctx is cancelled. Call it in its own
// goroutine at boot, same as the outbox relay and the availability projection.
func (c *Consumer) Run(ctx context.Context, sub events.Subscriber) error {
	return sub.Subscribe(ctx, DurableName, c.Subjects(), c.handle)
}

func (c *Consumer) handle(ctx context.Context, env events.Envelope) error {
	switch env.Subject {
	case events.SubjectAppointmentCompleted:
		return c.applyTerminal(ctx, env, OutcomeCompleted)
	case events.SubjectAppointmentNoShow:
		return c.applyTerminal(ctx, env, OutcomeNoShow)
	case events.SubjectAppointmentCancelled:
		return c.applyCancelled(ctx, env)
	case events.SubjectConsultationEnded:
		return c.applyConsultation(ctx, env)
	case events.SubjectPaymentSucceeded:
		return c.applyPayment(ctx, env)
	case events.SubjectPayoutSent:
		return c.applyPayout(ctx, env)
	default:
		// A subject we did not ask for should not be deliverable, but a
		// misconfigured filter must not wedge the consumer loop.
		c.log.Warn().Str("subject", string(env.Subject)).
			Msg("analytics consumer: unexpected subject, ignoring")
		return nil
	}
}

// drop logs an event that will never become valid and acknowledges it.
// Returning an error instead would park a malformed payload in the redelivery
// loop forever and block the events behind it.
func (c *Consumer) drop(env events.Envelope, reason string, err error) error {
	e := c.log.Error()
	if err != nil {
		e = e.Err(err)
	}
	e.Str("event_id", env.ID.String()).Str("subject", string(env.Subject)).
		Msg("analytics consumer: " + reason + ", dropping")
	return nil
}

// eventTime prefers the producer's own timestamp for WHEN THE FACT HAPPENED
// over the envelope's, which is when it was enqueued and drifts under a relay
// backlog. The fallback matters for a producer that forgets to set it: a zero
// time loses every ordering comparison forever and freezes that row.
func eventTime(fromPayload, fromEnvelope time.Time) time.Time {
	if !fromPayload.IsZero() {
		return fromPayload.UTC()
	}
	if !fromEnvelope.IsZero() {
		return fromEnvelope.UTC()
	}
	return time.Now().UTC()
}

func (c *Consumer) applyTerminal(ctx context.Context, env events.Envelope, outcome Outcome) error {
	var p events.AppointmentTerminal
	if err := env.Decode(&p); err != nil {
		return c.drop(env, "unparseable appointment terminal payload", err)
	}
	if p.AppointmentID == uuidZero || p.DoctorID == uuidZero {
		return c.drop(env, "appointment terminal payload missing ids", nil)
	}
	if p.StartAt.IsZero() {
		// The hour-of-week bucket IS the appointment's start. Without it the
		// peak-hours chart would gain a phantom column at midnight Thursday,
		// which is worse than gaining nothing.
		return c.drop(env, "appointment terminal payload has no start_at", nil)
	}
	return c.applyAppointment(ctx, env, p.AppointmentID, p.DoctorID, p.StartAt, outcome,
		eventTime(p.OccurredAt, env.OccurredAt))
}

func (c *Consumer) applyCancelled(ctx context.Context, env events.Envelope) error {
	var p events.AppointmentCancelled
	if err := env.Decode(&p); err != nil {
		return c.drop(env, "unparseable appointment.cancelled payload", err)
	}
	if p.AppointmentID == uuidZero || p.DoctorID == uuidZero {
		return c.drop(env, "appointment.cancelled payload missing ids", nil)
	}
	if p.StartAt.IsZero() {
		return c.drop(env, "appointment.cancelled payload has no start_at", nil)
	}
	// A cancellation carrying NoShow is scheduling telling us the patient did
	// not turn up and was cancelled as a consequence. It is a no-show for
	// analytics purposes, and counting it as an ordinary cancellation would
	// understate exactly the metric this screen exists to show.
	outcome := OutcomeCancelled
	if p.NoShow {
		outcome = OutcomeNoShow
	}
	return c.applyAppointment(ctx, env, p.AppointmentID, p.DoctorID, p.StartAt, outcome,
		eventTime(p.CancelledAt, env.OccurredAt))
}

func (c *Consumer) applyAppointment(ctx context.Context, env events.Envelope,
	appointmentID, doctorID uuid.UUID, startAt time.Time, outcome Outcome, at time.Time,
) error {
	local := startAt.In(c.loc)
	fact := AppointmentFact{
		AppointmentID: appointmentID,
		DoctorID:      doctorID,
		StartAt:       startAt.UTC(),
		LocalDate:     dateIn(startAt, c.loc),
		DayOfWeek:     int(local.Weekday()),
		HourOfDay:     local.Hour(),
		Outcome:       outcome,
		LastEventID:   env.ID,
		LastEventAt:   at,
	}

	err := database.InTx(ctx, c.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := c.repo.ApplyAppointment(ctx, tx, fact)
		return err
	})
	if err != nil {
		return fmt.Errorf("analytics consumer: apply %s for appointment %s: %w",
			env.Subject, appointmentID, err)
	}
	return nil
}

func (c *Consumer) applyConsultation(ctx context.Context, env events.Envelope) error {
	var p events.ConsultationEnded
	if err := env.Decode(&p); err != nil {
		return c.drop(env, "unparseable consultation.ended payload", err)
	}
	if p.ConsultationID == uuidZero || p.DoctorID == uuidZero {
		return c.drop(env, "consultation.ended payload missing ids", nil)
	}
	if p.DurationSeconds < 0 {
		return c.drop(env, "consultation.ended payload has a negative duration", nil)
	}

	// events.ConsultationEnded carries no appointment start, so the call is
	// dated by when it ENDED. The two differ only for a consultation that
	// straddles local midnight, and "the day the call finished" is a defensible
	// reading of that case in any event.
	endedAt := eventTime(p.EndedAt, env.OccurredAt)
	fact := ConsultationFact{
		ConsultationID:  p.ConsultationID,
		DoctorID:        p.DoctorID,
		AppointmentID:   p.AppointmentID,
		EndedAt:         endedAt,
		LocalDate:       dateIn(endedAt, c.loc),
		DurationSeconds: p.DurationSeconds,
		EndReason:       p.EndReason,
		LastEventID:     env.ID,
		LastEventAt:     endedAt,
	}

	err := database.InTx(ctx, c.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := c.repo.ApplyConsultation(ctx, tx, fact)
		return err
	})
	if err != nil {
		return fmt.Errorf("analytics consumer: apply consultation %s: %w", p.ConsultationID, err)
	}
	return nil
}

func (c *Consumer) applyPayment(ctx context.Context, env events.Envelope) error {
	var p events.PaymentSucceeded
	if err := env.Decode(&p); err != nil {
		return c.drop(env, "unparseable payment.succeeded payload", err)
	}
	if p.PaymentID == uuidZero || p.DoctorID == uuidZero {
		return c.drop(env, "payment.succeeded payload missing ids", nil)
	}
	if p.AmountCents < 0 || p.CommissionCents < 0 || p.PayoutCents < 0 {
		return c.drop(env, "payment.succeeded payload carries a negative amount", nil)
	}

	// PayoutCents is what payment-service says the doctor earns, and it is
	// taken verbatim. Re-deriving it here as amount - commission would put a
	// second copy of the commission rule in a service that does not own it, and
	// the first time the two disagreed a doctor's earnings screen and their
	// bank statement would show different numbers.
	net := p.PayoutCents
	currency := p.Currency
	if currency == "" {
		// An empty currency would land in the rollup as "mixed" and make the
		// day's total unrenderable. LKR is the platform's only currency and
		// saying so loudly beats storing a blank.
		c.log.Warn().Str("event_id", env.ID.String()).Str("payment_id", p.PaymentID.String()).
			Msg("analytics consumer: payment.succeeded carried no currency, assuming LKR")
		currency = DefaultCurrency
	}

	succeededAt := eventTime(p.SucceededAt, env.OccurredAt)
	fact := PaymentFact{
		PaymentID:       p.PaymentID,
		DoctorID:        p.DoctorID,
		AppointmentID:   p.AppointmentID,
		SucceededAt:     succeededAt,
		LocalDate:       dateIn(succeededAt, c.loc),
		GrossCents:      p.AmountCents,
		CommissionCents: p.CommissionCents,
		NetCents:        net,
		Currency:        currency,
		LastEventID:     env.ID,
		LastEventAt:     succeededAt,
	}

	err := database.InTx(ctx, c.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := c.repo.ApplyPayment(ctx, tx, fact)
		return err
	})
	if err != nil {
		return fmt.Errorf("analytics consumer: apply payment %s: %w", p.PaymentID, err)
	}
	return nil
}

func (c *Consumer) applyPayout(ctx context.Context, env events.Envelope) error {
	var p events.PayoutSent
	if err := env.Decode(&p); err != nil {
		return c.drop(env, "unparseable payout.sent payload", err)
	}
	if p.PayoutID == uuidZero || p.DoctorID == uuidZero {
		return c.drop(env, "payout.sent payload missing ids", nil)
	}

	// events.PayoutSent declares PeriodStart/PeriodEnd as "YYYY-MM-DD" strings
	// and payment-service currently sends RFC3339 there -- a live mismatch
	// recorded in _shared/INTEGRATION-FIXES.md. Both forms are accepted here
	// rather than waiting for the producer to be corrected, because the
	// alternative is dropping every payout event and reporting a doctor as
	// unpaid for money already in their account.
	start, err := parsePeriodDate(p.PeriodStart, c.loc)
	if err != nil {
		return c.drop(env, "payout.sent has an unparseable period_start", err)
	}
	end, err := parsePeriodDate(p.PeriodEnd, c.loc)
	if err != nil {
		return c.drop(env, "payout.sent has an unparseable period_end", err)
	}
	if end.Before(start) {
		return c.drop(env, "payout.sent period ends before it starts", nil)
	}

	currency := p.Currency
	if currency == "" {
		currency = DefaultCurrency
	}
	sentAt := eventTime(p.SentAt, env.OccurredAt)
	fact := PayoutFact{
		PayoutID:    p.PayoutID,
		DoctorID:    p.DoctorID,
		AmountCents: p.AmountCents,
		Currency:    currency,
		PeriodStart: start,
		PeriodEnd:   end,
		TransferID:  p.TransferID,
		SentAt:      sentAt,
		LastEventID: env.ID,
		LastEventAt: sentAt,
	}

	err = database.InTx(ctx, c.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := c.repo.ApplyPayout(ctx, tx, fact)
		return err
	})
	if err != nil {
		return fmt.Errorf("analytics consumer: apply payout %s: %w", p.PayoutID, err)
	}
	return nil
}

// parsePeriodDate accepts both the declared "YYYY-MM-DD" and the RFC3339 that
// payment-service actually sends, and returns UTC midnight of the civil date in
// the business timezone.
//
// An RFC3339 instant is resolved in loc BEFORE the date is taken: a payout
// period ending "2026-08-31T18:30:00Z" is the 1st of September in UTC and the
// 31st of August in Colombo, and the 31st is the day the doctor was told.
func parsePeriodDate(raw string, loc *time.Location) (time.Time, error) {
	if raw == "" {
		return time.Time{}, fmt.Errorf("analytics: empty period date")
	}
	if d, err := time.Parse(time.DateOnly, raw); err == nil {
		return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC), nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("analytics: parse period date %q: %w", raw, err)
	}
	return dateIn(t, loc), nil
}
