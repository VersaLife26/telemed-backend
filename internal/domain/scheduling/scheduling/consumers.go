package scheduling

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"

	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/observability"
)

// ConsumerName is the JetStream durable name. Changing it re-reads the whole
// stream from the beginning, so it is a constant rather than a config key.
const ConsumerName = "scheduling-service"

// Consumers subscribes this service to the events it reacts to.
//
// Every handler is idempotent on the envelope id, recorded in consumed_events
// inside the same transaction as its effect. JetStream guarantees at-least-once
// and nothing more; a handler that assumed exactly-once would double-confirm an
// appointment the first time the broker redelivered.
type Consumers struct {
	svc *Service
	sub events.Subscriber
	log zerolog.Logger
	met *observability.Metrics
}

// NewConsumers wires the subscriber.
func NewConsumers(svc *Service, sub events.Subscriber, met *observability.Metrics, log zerolog.Logger) *Consumers {
	return &Consumers{svc: svc, sub: sub, log: log, met: met}
}

// Subjects lists what this service consumes. Exported so a test can assert the
// set, and so the README's event table can be checked against the code.
func (c *Consumers) Subjects() []events.Subject {
	return []events.Subject{
		events.SubjectDoctorApproved,
		// doctor.updated is what keeps the pricing projection from going stale.
		// Without it a doctor who changes their fee, specialty or status after
		// approval is quoted at their approval-day price forever.
		events.SubjectDoctorUpdated,
		events.SubjectPaymentAuthorized,
		events.SubjectPaymentSucceeded,
		events.SubjectPaymentFailed,
		// Administrator commands. This service owns the appointment lifecycle,
		// so it is the only place that can carry them out.
		events.SubjectAdminAppointmentForceCancel,
		events.SubjectAdminDoubleBookingResolveRequested,
		events.SubjectConsultationPatientNoShow,
	}
}

// Run blocks until ctx is cancelled.
func (c *Consumers) Run(ctx context.Context) error {
	return c.sub.Subscribe(ctx, ConsumerName, c.Subjects(), c.Handle)
}

// Handle dispatches one envelope. An error returns the message to JetStream for
// redelivery with backoff; nil acknowledges it.
func (c *Consumers) Handle(ctx context.Context, env events.Envelope) error {
	var err error
	switch env.Subject {
	case events.SubjectDoctorApproved:
		err = c.handleDoctorApproved(ctx, env)
	case events.SubjectDoctorUpdated:
		err = c.handleDoctorUpdated(ctx, env)
	case events.SubjectPaymentAuthorized, events.SubjectPaymentSucceeded:
		err = c.handlePaymentSucceeded(ctx, env)
	case events.SubjectPaymentFailed:
		err = c.handlePaymentFailed(ctx, env)
	case events.SubjectAdminAppointmentForceCancel:
		err = c.handleAdminForceCancel(ctx, env)
	case events.SubjectAdminDoubleBookingResolveRequested:
		err = c.handleAdminResolveDoubleBooking(ctx, env)
	case events.SubjectConsultationPatientNoShow:
		err = c.handleConsultationPatientNoShow(ctx, env)
	default:
		// Subscribing to a subject we do not handle is a wiring bug, but
		// nak-ing forever would wedge the consumer. Ack and complain.
		c.log.Warn().Str("subject", string(env.Subject)).Msg("no handler for subject; acknowledging")
		return nil
	}

	result := "ok"
	if err != nil {
		result = "error"
	}
	if c.met != nil {
		c.met.EventsConsumed.WithLabelValues(string(env.Subject), result).Inc()
	}
	return err
}

// handleDoctorApproved seeds a newly verified doctor's schedule configuration
// and materialises their first thirty days.
//
// The event is the only channel: doctor-service owns working hours, this service
// mirrors them. There is no synchronous call back to doctor-service, which is
// what keeps a doctor-service outage from stopping bookings for the 499 doctors
// already mirrored (ADR-004).
func (c *Consumers) handleDoctorApproved(ctx context.Context, env events.Envelope) error {
	// The canonical payload -- the same struct doctor-service publishes -- is
	// authoritative for identity and for PRICE. It is what makes this service
	// able to quote a booking at all.
	var approved events.DoctorApproved
	if err := env.Decode(&approved); err != nil {
		// A payload we cannot parse will not parse on redelivery either.
		c.log.Error().Err(err).Str("event_id", env.ID.String()).Msg("dropping unparseable doctor.approved")
		return nil
	}
	if approved.DoctorID == uuid.Nil {
		c.log.Error().Str("event_id", env.ID.String()).Msg("doctor.approved without a doctor_id")
		return nil
	}

	// The scheduling preferences are decoded separately, from the SAME bytes.
	//
	// events.DoctorApproved now carries working hours and schedule settings,
	// and this decode is how they are read: DoctorScheduleHints is scheduling's
	// own wire type, with the validation parseWorkingHours needs.
	//
	// Before those fields existed, this decode yielded nothing and every
	// approved doctor got DefaultScheduleSettings -- which meant zero slots
	// generated and a doctor who was approved but permanently unbookable, with
	// nothing logged to say why.
	var hints DoctorScheduleHints
	if err := env.Decode(&hints); err != nil {
		c.log.Warn().Err(err).Str("event_id", env.ID.String()).
			Msg("doctor.approved carried unusable scheduling hints; using defaults")
		hints = DoctorScheduleHints{}
	}
	p := hints
	p.DoctorID = approved.DoctorID

	pricing := DoctorPricing{
		DoctorID:    approved.DoctorID,
		Specialty:   approved.Specialty,
		FeeCents:    approved.FeeCents,
		Currency:    approved.Currency,
		Languages:   approved.Languages,
		Status:      "approved",
		LastEventAt: eventTime(approved.ApprovedAt, env.OccurredAt),
	}
	if pricing.Currency == "" {
		pricing.Currency = DefaultCurrency
	}
	if pricing.FeeCents <= 0 {
		// Loud, because it is unrecoverable from here and it is exactly the
		// failure mode that made the platform unable to take money. The row is
		// still written -- with a zero fee it will not satisfy Quotable(), so
		// bookings for this doctor fail with a clear 422 rather than becoming
		// unpayable appointments -- but an operator has to know.
		c.log.Error().
			Str("doctor_id", maskID(approved.DoctorID)).
			Str("event_id", env.ID.String()).
			Msg("doctor.approved carried no fee: this doctor cannot be booked " +
				"until doctor-service republishes a price")
	}

	settings := DefaultScheduleSettings(p.DoctorID)
	if p.Timezone != "" {
		if _, err := time.LoadLocation(p.Timezone); err != nil {
			c.log.Warn().Err(err).Str("timezone", p.Timezone).
				Msg("doctor.approved carried an unknown timezone; using the platform default")
		} else {
			settings.Timezone = p.Timezone
		}
	}
	if p.SlotDurationMinutes > 0 {
		settings.SlotDurationMinutes = p.SlotDurationMinutes
	}
	if p.BufferMinutes != nil && *p.BufferMinutes >= 0 {
		settings.BufferMinutes = *p.BufferMinutes
	}
	if p.MaxPerDay > 0 {
		settings.MaxPerDay = p.MaxPerDay
	}
	if p.AdvanceDays > 0 {
		settings.AdvanceDays = p.AdvanceDays
	}

	hours, err := parseWorkingHours(p.DoctorID, p.WorkingHours)
	if err != nil {
		c.log.Error().Err(err).Str("doctor_id", maskID(p.DoctorID)).
			Msg("doctor.approved carried unusable working hours; storing settings only")
		hours = nil
	}

	fresh := false
	err = database.InTx(ctx, c.svc.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		first, err := c.svc.repo.MarkEventConsumed(ctx, tx, ConsumerName, env.ID, string(env.Subject))
		if err != nil {
			return err
		}
		if !first {
			return nil
		}
		fresh = true
		// The pricing upsert is inside the same transaction as the idempotency
		// marker, so a crash between them cannot leave the marker written and
		// the price missing -- which would mean the event is never redelivered
		// and the doctor is permanently unbookable.
		if _, err := c.svc.repo.UpsertDoctorPricing(ctx, tx, pricing); err != nil {
			return err
		}
		if err := c.svc.repo.UpsertScheduleSettings(ctx, tx, settings); err != nil {
			return err
		}
		if len(hours) > 0 {
			return c.svc.repo.ReplaceWorkingHours(ctx, tx, p.DoctorID, hours)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !fresh {
		return nil
	}

	// Generation runs outside the idempotency transaction on purpose: it is
	// itself idempotent, it can take a second, and the nightly cron is the
	// backstop if it fails here. Making it part of the transaction would mean a
	// transient generation error replays the settings write forever.
	if _, err := c.svc.GenerateForDoctor(ctx, p.DoctorID); err != nil {
		if errors.Is(err, ErrDoctorNotConfigured) {
			c.log.Info().Str("doctor_id", maskID(p.DoctorID)).
				Msg("doctor approved without working hours; slots will appear once they publish a schedule")
			return nil
		}
		c.log.Error().Err(err).Str("doctor_id", maskID(p.DoctorID)).
			Msg("initial slot generation failed; the nightly job will retry")
	}
	return nil
}

// handleDoctorUpdated refreshes the pricing projection.
//
// This handler is what makes the locked-quote design honest. A doctor raising
// their fee updates the LIST PRICE here and nothing else: every appointment
// already booked keeps the amount_cents it was stamped with, because that is
// what the patient agreed to. Re-pricing existing appointments from this event
// would be charging people a number they never saw.
//
// Unlike the other handlers this one does NOT gate on consumed_events. The
// upsert is guarded on last_event_at instead, which is strictly stronger: it
// makes a redelivery a no-op AND makes an out-of-order delivery a no-op, which
// an envelope-id check cannot do. JetStream gives at-least-once with no
// ordering guarantee, so "have I seen this id" is the wrong question -- "is
// this newer than what I have" is.
func (c *Consumers) handleDoctorUpdated(ctx context.Context, env events.Envelope) error {
	var p events.DoctorUpdated
	if err := env.Decode(&p); err != nil {
		c.log.Error().Err(err).Str("event_id", env.ID.String()).Msg("dropping unparseable doctor.updated")
		return nil
	}
	if p.DoctorID == uuid.Nil {
		c.log.Error().Str("event_id", env.ID.String()).Msg("doctor.updated without a doctor_id")
		return nil
	}

	pricing := DoctorPricing{
		DoctorID:    p.DoctorID,
		Specialty:   p.Specialty,
		FeeCents:    p.FeeCents,
		Currency:    p.Currency,
		Languages:   p.Languages,
		Status:      p.Status,
		LastEventAt: eventTime(p.UpdatedAt, env.OccurredAt),
	}
	if pricing.Currency == "" {
		pricing.Currency = DefaultCurrency
	}

	// The schedule half. doctor.updated is published on every availability
	// edit, so ignoring these fields meant a doctor who added Saturday mornings
	// kept generating no Saturday slots -- the pattern was applied on approval
	// and then frozen for the life of the account.
	//
	// Only fields the producer actually sent are applied. A doctor.updated that
	// carries a fee change and no schedule must not reset the schedule to
	// defaults, so an absent field leaves the stored value alone.
	// A doctor.updated can legitimately arrive for a doctor scheduling has no
	// settings row for -- an out-of-order delivery that beat doctor.approved,
	// or a profile edit on an account approved before this projection existed.
	// Erroring there would park the event in the redelivery loop forever, so
	// fall back to the defaults the approval path would have created.
	settings, err := c.svc.repo.GetScheduleSettings(ctx, c.svc.pool, p.DoctorID)
	settingsChanged := false
	if err != nil {
		if !errors.Is(err, ErrDoctorNotConfigured) {
			return err
		}
		settings = DefaultScheduleSettings(p.DoctorID)
		settingsChanged = true
	}
	if p.Timezone != "" {
		if _, err := time.LoadLocation(p.Timezone); err != nil {
			c.log.Warn().Err(err).Str("timezone", p.Timezone).
				Msg("doctor.updated carried an unknown timezone; keeping the current one")
		} else if settings.Timezone != p.Timezone {
			settings.Timezone = p.Timezone
			settingsChanged = true
		}
	}
	if p.SlotDurationMinutes > 0 && settings.SlotDurationMinutes != p.SlotDurationMinutes {
		settings.SlotDurationMinutes = p.SlotDurationMinutes
		settingsChanged = true
	}
	// BufferMinutes is a pointer because zero is meaningful: a doctor wanting
	// back-to-back consultations is saying something different from a doctor
	// whose preference never arrived.
	if p.BufferMinutes != nil && *p.BufferMinutes >= 0 && settings.BufferMinutes != *p.BufferMinutes {
		settings.BufferMinutes = *p.BufferMinutes
		settingsChanged = true
	}
	if p.MaxPerDay > 0 && settings.MaxPerDay != p.MaxPerDay {
		settings.MaxPerDay = p.MaxPerDay
		settingsChanged = true
	}

	// Working hours are decoded from the SAME bytes into scheduling's own
	// wire type, which is what parseWorkingHours validates against.
	var hints DoctorScheduleHints
	if dErr := env.Decode(&hints); dErr != nil {
		c.log.Warn().Err(dErr).Str("event_id", env.ID.String()).
			Msg("doctor.updated carried unusable scheduling hints; applying the rest")
		hints = DoctorScheduleHints{}
	}

	hours, hErr := parseWorkingHours(p.DoctorID, hints.WorkingHours)
	if hErr != nil {
		c.log.Error().Err(hErr).Str("doctor_id", maskID(p.DoctorID)).
			Msg("doctor.updated carried unusable working hours; applying the rest")
		hours = nil
	}

	applied := false
	hoursChanged := false
	err = database.InTx(ctx, c.svc.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var uErr error
		// last_event_at guards this, so a stale or duplicated delivery is a
		// no-op for the schedule as well as the price -- the two must move
		// together or a redelivered older event could roll the pattern back
		// while leaving the new fee in place.
		applied, uErr = c.svc.repo.UpsertDoctorPricing(ctx, tx, pricing)
		if uErr != nil || !applied {
			return uErr
		}
		if settingsChanged {
			if sErr := c.svc.repo.UpsertScheduleSettings(ctx, tx, settings); sErr != nil {
				return sErr
			}
		}
		if len(hours) == 0 {
			return nil
		}
		// doctor.updated is published on EVERY profile edit -- a new fee, a new
		// bio, a new photo -- and carries the working hours every time. Diffing
		// against what we already hold is what stops a bio edit from
		// reconciling a month of slots, which is a plan, a row lock on every
		// slot in the window and a COPY for a change that touched no hour.
		current, cErr := c.svc.repo.ListWorkingHours(ctx, tx, p.DoctorID)
		if cErr != nil {
			return cErr
		}
		if sameWeeklyPattern(current, hours) {
			return nil
		}
		hoursChanged = true
		return c.svc.repo.ReplaceWorkingHours(ctx, tx, p.DoctorID, hours)
	})
	if err != nil {
		return err
	}
	if !applied {
		// A redelivery, or an event older than what we already hold. Refusing
		// it is the point of last_event_at.
		c.log.Debug().Str("doctor_id", maskID(p.DoctorID)).
			Msg("doctor.updated ignored as stale or duplicate")
		return nil
	}

	c.log.Info().
		Str("doctor_id", maskID(p.DoctorID)).
		Int64("fee_cents", pricing.FeeCents).
		Str("status", pricing.Status).
		Bool("settings_changed", settingsChanged).
		Bool("hours_changed", hoursChanged).
		Int("working_hours", len(hours)).
		Msg("doctor projection updated")

	if !hoursChanged && !settingsChanged {
		return nil
	}

	// The edit has to reach the slots themselves, not just the pattern they are
	// generated from. Mirroring the hours and stopping here is what made the
	// availability editor decorative for a month at a time: a doctor who opened
	// up Saturday saw an empty Saturday in the patient app until the 00:00 cron
	// caught up, which for the coming weekend is too late.
	//
	// Errors are logged, not returned. Returning one nak-s the envelope, and
	// the redelivery would find its own last_event_at already written and skip
	// the whole handler -- so a retry here cannot work, while the nightly run
	// can.
	if _, err := c.svc.SyncScheduleChange(ctx, p.DoctorID); err != nil {
		if errors.Is(err, ErrDoctorNotConfigured) {
			c.log.Info().Str("doctor_id", maskID(p.DoctorID)).
				Msg("doctor updated without working hours; slots will appear once they publish a schedule")
			return nil
		}
		c.log.Error().Err(err).Str("doctor_id", maskID(p.DoctorID)).
			Msg("slot sync after doctor.updated failed; the nightly job will retry generation")
	}

	return nil
}

// sameWeeklyPattern reports whether two sets of working hours describe the same
// clinic week. Identity and ordering are deliberately ignored: the mirror's
// rows carry their own ids, and the event's order is whatever doctor-service's
// query returned.
func sameWeeklyPattern(a, b []WorkingHour) bool {
	if len(a) != len(b) {
		return false
	}
	sorted := func(in []WorkingHour) []WorkingHour {
		out := make([]WorkingHour, len(in))
		copy(out, in)
		sort.Slice(out, func(i, j int) bool {
			if out[i].DayOfWeek != out[j].DayOfWeek {
				return out[i].DayOfWeek < out[j].DayOfWeek
			}
			return out[i].StartMinute < out[j].StartMinute
		})
		return out
	}
	x, y := sorted(a), sorted(b)
	for i := range x {
		if x[i].DayOfWeek != y[i].DayOfWeek ||
			x[i].StartMinute != y[i].StartMinute ||
			x[i].EndMinute != y[i].EndMinute ||
			x[i].IsAvailable != y[i].IsAvailable {
			return false
		}
	}
	return true
}

// eventTime picks the producer's own timestamp for the fact, falling back to
// the envelope's.
//
// The producer's field is preferred because it is when the change HAPPENED; the
// envelope's is when it was enqueued, which drifts under a relay backlog. The
// fallback matters for a producer that forgets to set it: a zero time would
// lose every ordering comparison forever and freeze the projection.
func eventTime(fromPayload, fromEnvelope time.Time) time.Time {
	if !fromPayload.IsZero() {
		return fromPayload.UTC()
	}
	if !fromEnvelope.IsZero() {
		return fromEnvelope.UTC()
	}
	return time.Now().UTC()
}

// handlePaymentSucceeded confirms the appointment the payment belongs to.
func (c *Consumers) handlePaymentSucceeded(ctx context.Context, env events.Envelope) error {
	var p PaymentResultPayload
	if err := env.Decode(&p); err != nil {
		c.log.Error().Err(err).Str("event_id", env.ID.String()).Msg("dropping unparseable payment.succeeded")
		return nil
	}
	if p.AppointmentID == uuid.Nil {
		return nil
	}

	return database.InTx(ctx, c.svc.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		first, err := c.svc.repo.MarkEventConsumed(ctx, tx, ConsumerName, env.ID, string(env.Subject))
		if err != nil {
			return err
		}
		if !first {
			return nil
		}
		if err := c.svc.ConfirmAppointment(ctx, tx, p.AppointmentID, p.PaymentID); err != nil {
			if errors.Is(err, ErrAppointmentNotFound) {
				// A payment for an appointment we have no record of is a real
				// operational event -- a cross-environment webhook, usually --
				// and retrying will not conjure the row.
				c.log.Error().Str("appointment_id", maskID(p.AppointmentID)).
					Msg("payment.succeeded for an unknown appointment")
				return nil
			}
			return err
		}
		return nil
	})
}

// handlePaymentFailed releases the slot the failed payment was holding.
func (c *Consumers) handlePaymentFailed(ctx context.Context, env events.Envelope) error {
	var p PaymentResultPayload
	if err := env.Decode(&p); err != nil {
		c.log.Error().Err(err).Str("event_id", env.ID.String()).Msg("dropping unparseable payment.failed")
		return nil
	}
	if p.AppointmentID == uuid.Nil {
		return nil
	}

	reason := p.FailureText()
	if reason == "" {
		reason = paymentFailedReason
	}

	var released bool
	var doctorID, slotID uuid.UUID
	err := database.InTx(ctx, c.svc.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		first, err := c.svc.repo.MarkEventConsumed(ctx, tx, ConsumerName, env.ID, string(env.Subject))
		if err != nil {
			return err
		}
		if !first {
			return nil
		}
		released, doctorID, slotID, err = c.svc.ReleaseForFailedPayment(ctx, tx, p.AppointmentID, reason)
		if errors.Is(err, ErrAppointmentNotFound) {
			c.log.Error().Str("appointment_id", maskID(p.AppointmentID)).
				Msg("payment.failed for an unknown appointment")
			return nil
		}
		return err
	})
	if err != nil {
		return err
	}
	if released {
		if err := c.svc.PromoteWaitlist(ctx, doctorID, slotID); err != nil {
			c.log.Error().Err(err).Msg("waitlist promotion after failed payment failed")
		}
	}
	return nil
}

// parseWorkingHours converts the event's wall-clock strings into
// minutes-since-midnight. It accepts "HH:MM" and "HH:MM:SS" because both appear
// in the wild depending on whether the value came from a Postgres TIME column or
// from a form field.
func parseWorkingHours(doctorID uuid.UUID, in []WorkingHourPayload) ([]WorkingHour, error) {
	out := make([]WorkingHour, 0, len(in))
	for _, h := range in {
		if h.DayOfWeek < 0 || h.DayOfWeek > 6 {
			return nil, fmt.Errorf("scheduling: day_of_week %d out of range", h.DayOfWeek)
		}
		startMin, err := parseClock(h.StartTime)
		if err != nil {
			return nil, err
		}
		endMin, err := parseClock(h.EndTime)
		if err != nil {
			return nil, err
		}
		if endMin <= startMin {
			return nil, fmt.Errorf("scheduling: working hour %s-%s does not advance", h.StartTime, h.EndTime)
		}
		available := true
		if h.IsAvailable != nil {
			available = *h.IsAvailable
		}
		out = append(out, WorkingHour{
			DoctorID:    doctorID,
			DayOfWeek:   time.Weekday(h.DayOfWeek),
			StartMinute: startMin,
			EndMinute:   endMin,
			IsAvailable: available,
		})
	}
	return out, nil
}

// parseClock reads "HH:MM" or "HH:MM:SS" into minutes since midnight. Seconds
// are accepted and discarded: a clinic that opens at 09:00:30 is a typo, not a
// requirement.
func parseClock(s string) (int, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("scheduling: %q is not a HH:MM time", s)
	}
	hh, err := strconv.Atoi(parts[0])
	if err != nil || hh < 0 || hh > 23 {
		return 0, fmt.Errorf("scheduling: %q has an invalid hour", s)
	}
	mm, err := strconv.Atoi(parts[1])
	if err != nil || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("scheduling: %q has an invalid minute", s)
	}
	return hh*60 + mm, nil
}

// --- administrator commands -------------------------------------------------
//
// Both handlers below carry out a decision an administrator already made and
// that the admin service already wrote to its audit log. They are deliberately
// NOT guarded by consumed_events: CancelAppointment takes a row lock and
// refuses anything that is not pending_payment or confirmed, so a redelivery
// finds the appointment already cancelled and returns
// ErrAppointmentNotCancellable. Adding a consumed_events row would put the
// idempotency marker in a different transaction from the effect -- committing
// the marker for a cancellation that then failed is how an administrator's
// force-cancel silently does nothing.

// forceCancel applies one administrator-authorised cancellation.
//
// Force is true: the whole reason this command exists is to cancel an
// appointment whose slot has already started, which is what an operator needs
// when a consultation is stuck.
func (c *Consumers) forceCancel(ctx context.Context, appointmentID, adminID uuid.UUID, reason string, env events.Envelope) error {
	_, err := c.svc.CancelAppointment(ctx, CancelInput{
		AppointmentID: appointmentID,
		ActorID:       adminID,
		ActorRole:     "admin",
		Reason:        reason,
		Force:         true,
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrAppointmentNotCancellable):
		// Already cancelled or already completed. Redelivery cannot change
		// that, and either way the administrator's intent now holds.
		c.log.Info().Str("appointment_id", maskID(appointmentID)).
			Str("event_id", env.ID.String()).
			Msg("admin cancellation: appointment was no longer cancellable")
		return nil
	case errors.Is(err, ErrAppointmentNotFound):
		c.log.Error().Str("appointment_id", maskID(appointmentID)).
			Str("event_id", env.ID.String()).
			Msg("admin cancellation for an unknown appointment")
		return nil
	default:
		return err
	}
}

// handleAdminForceCancel cancels one appointment on an administrator's
// authority.
func (c *Consumers) handleAdminForceCancel(ctx context.Context, env events.Envelope) error {
	var cmd events.AdminAppointmentForceCancelRequested
	if err := env.Decode(&cmd); err != nil {
		c.log.Error().Err(err).Str("event_id", env.ID.String()).
			Msg("dropping unparseable admin.appointment_force_cancel_requested")
		return nil
	}
	if cmd.AppointmentID == uuid.Nil {
		c.log.Error().Str("event_id", env.ID.String()).
			Msg("admin.appointment_force_cancel_requested without an appointment_id")
		return nil
	}
	return c.forceCancel(ctx, cmd.AppointmentID, cmd.AdminID, cmd.Reason, env)
}

func (c *Consumers) handleConsultationPatientNoShow(ctx context.Context, env events.Envelope) error {
	var p events.ConsultationPatientNoShow
	if err := env.Decode(&p); err != nil {
		c.log.Error().Err(err).Str("event_id", env.ID.String()).
			Msg("dropping unparseable consultation.patient_no_show")
		return nil
	}
	if p.AppointmentID == uuid.Nil {
		c.log.Error().Str("event_id", env.ID.String()).
			Msg("consultation.patient_no_show without an appointment_id")
		return nil
	}
	_, err := c.svc.MarkTerminal(ctx, p.AppointmentID, uuid.Nil, "system", AppointmentCompleted)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrAppointmentNotCancellable):
		c.log.Info().Str("appointment_id", maskID(p.AppointmentID)).
			Str("event_id", env.ID.String()).
			Msg("unused slot: appointment was already closed")
		return nil
	case errors.Is(err, ErrAppointmentNotFound):
		c.log.Error().Str("appointment_id", maskID(p.AppointmentID)).
			Str("event_id", env.ID.String()).
			Msg("unused slot closed for an unknown appointment")
		return nil
	default:
		return err
	}
}

// handleAdminResolveDoubleBooking cancels the losing half of a double booking.
//
// Only the cancelled side is touched. The kept appointment needs no change --
// it is already in the state the administrator chose -- and writing to it
// anyway would bump its version under whoever is looking at it.
func (c *Consumers) handleAdminResolveDoubleBooking(ctx context.Context, env events.Envelope) error {
	var cmd events.AdminDoubleBookingResolveRequested
	if err := env.Decode(&cmd); err != nil {
		c.log.Error().Err(err).Str("event_id", env.ID.String()).
			Msg("dropping unparseable admin.double_booking_resolve_requested")
		return nil
	}
	if cmd.CancelAppointmentID == uuid.Nil {
		c.log.Error().Str("event_id", env.ID.String()).
			Msg("admin.double_booking_resolve_requested without a cancel_appointment_id")
		return nil
	}
	if cmd.CancelAppointmentID == cmd.KeepAppointmentID {
		// Cancelling the appointment being kept would resolve the conflict by
		// destroying both sides of it.
		c.log.Error().Str("event_id", env.ID.String()).
			Msg("admin.double_booking_resolve_requested names the same appointment twice")
		return nil
	}
	return c.forceCancel(ctx, cmd.CancelAppointmentID, cmd.AdminID, cmd.Reason, env)
}
