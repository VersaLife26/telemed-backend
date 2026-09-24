package scheduling

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"

	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/logger"
)

// Service holds the scheduling business rules. It never sees an http.Request
// and never writes SQL; it composes repository calls into transactions and
// decides policy.
type Service struct {
	pool    database.Pool
	repo    *Repository
	locker  SlotLocker
	queue   WaitlistQueue
	outbox  EventPublisher
	clock   Clock
	loc     *time.Location
	log     zerolog.Logger
	metrics *Metrics
}

// Options configures the Service. Everything external arrives through an
// interface so a test can substitute a clock, drop the Redis lock, or assert on
// the events that were enqueued.
type Options struct {
	Pool       database.Pool
	Repository *Repository
	Locker     SlotLocker
	Queue      WaitlistQueue
	Outbox     EventPublisher
	Clock      Clock
	Location   *time.Location
	Logger     zerolog.Logger
	Metrics    *Metrics
}

// NewService builds the scheduling service, defaulting the optional parts.
func NewService(o Options) *Service {
	if o.Repository == nil {
		o.Repository = NewRepository()
	}
	if o.Locker == nil {
		// No locker configured means no early reject. That is a degraded mode,
		// not a broken one (ADR-007).
		o.Locker = NoopLocker{}
	}
	if o.Queue == nil {
		o.Queue = NoopWaitlistQueue{}
	}
	if o.Clock == nil {
		o.Clock = SystemClock{}
	}
	if o.Location == nil {
		o.Location = time.UTC
	}
	if o.Metrics == nil {
		o.Metrics = NewMetrics(nil)
	}
	return &Service{
		pool:    o.Pool,
		repo:    o.Repository,
		locker:  o.Locker,
		queue:   o.Queue,
		outbox:  o.Outbox,
		clock:   o.Clock,
		loc:     o.Location,
		log:     o.Logger,
		metrics: o.Metrics,
	}
}

// Location returns the business timezone used for civil-date arithmetic.
func (s *Service) Location() *time.Location { return s.loc }

// Repo exposes the repository for the maintenance jobs and the invariant
// checker. It is not a general-purpose escape hatch: handlers must not use it.
func (s *Service) Repo() *Repository { return s.repo }

// Pool exposes the connection pool for read-only invariant queries.
func (s *Service) Pool() database.Pool { return s.pool }

func maskID(id uuid.UUID) string { return logger.MaskID(id.String()) }

// derefUUID flattens an optional payment id for the canonical
// events.AppointmentConfirmed, which declares PaymentID as a value.
//
// uuid.Nil is the honest encoding of "confirmed with no payment id attached" --
// a manual admin confirmation, or a provider that settled without returning
// one. A consumer that requires a payment must check for Nil rather than trust
// the field's presence, which is exactly the check a *uuid.UUID would have
// forced anyway.
func derefUUID(p *uuid.UUID) uuid.UUID {
	if p == nil {
		return uuid.Nil
	}
	return *p
}

// ---------------------------------------------------------------------------
// Availability
// ---------------------------------------------------------------------------

// ListSlots returns a doctor's bookable slots on one civil date in the business
// timezone. The date is interpreted in Asia/Colombo, not UTC: a patient asking
// for "2026-08-21" means the Sri Lankan day, which spans 18:30 the previous day
// to 18:30 that day in UTC.
func (s *Service) ListSlots(ctx context.Context, doctorID uuid.UUID, date Date) ([]Slot, error) {
	from := date.StartOfDay(s.loc)
	to := date.EndOfDay(s.loc)

	slots, err := s.repo.ListAvailableSlots(ctx, s.pool, doctorID, from.UTC(), to.UTC())
	if err != nil {
		return nil, err
	}

	// A slot that starts in the next few seconds is not really bookable: by the
	// time the patient completes checkout the consultation would have started.
	cutoff := s.clock.Now().Add(2 * time.Minute)
	out := slots[:0]
	for i := range slots {
		if slots[i].StartAt.After(cutoff) {
			out = append(out, slots[i])
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Booking -- the critical path
// ---------------------------------------------------------------------------

// BookSlotInput is a booking request, already authenticated.
type BookSlotInput struct {
	SlotID    uuid.UUID
	PatientID uuid.UUID
	// DoctorID, when set, must match the slot's doctor. The mobile clients send
	// it, and checking it turns a client-side bug into a 404 instead of a
	// patient booking a cardiologist they never chose.
	DoctorID *uuid.UUID
	Intake   json.RawMessage

	// VisitPatientName and VisitPatientDOB snapshot who this consultation is
	// for at booking time (the account holder or someone else). Scheduling does
	// not trust family_member_id without an ownership read from user-service.
	VisitPatientName string
	VisitPatientDOB  time.Time
	// Optional; validated by the handler.
	VisitPatientSex       string
	VisitPatientWeightKg  *float64
	VisitPatientAllergies string

	// There is deliberately NO FamilyMemberID here.
	//
	// Booking for a dependant means asserting which PERSON a medical
	// appointment is for, and this service cannot check that assertion:
	// family_members lives in telemed_user, and scheduling holds no read of
	// it -- no gRPC client, no projection, nothing. `grep -rn FamilyMember`
	// over this repo used to return the DTO, this field, the model and the
	// INSERT, and no lookup anywhere.
	//
	// So the field was an unverified identity claim written straight onto the
	// appointment row: patient A supplies patient B's dependant id and the
	// booking, the consultation it becomes and the record hanging off it are
	// all attributed to a child A has no relationship with. That is the HTTP
	// half of the review's F5, which was closed on the gRPC side by
	// withdrawing BookSlot outright.
	//
	// Removing it from the input rather than zeroing it inside BookSlot is the
	// point: a future verified path has to add the field back deliberately,
	// next to the ownership check, instead of inheriting a field that silently
	// stopped working. Appointment.FamilyMemberID remains -- the column and
	// the response field are untouched, so rows that already carry one still
	// read back correctly.
	//
	// Reopening it needs an ownership read from user-service. See
	// docs/DESIGN.md.
}

// BookSlot books a slot for a patient, exactly once, under any amount of
// concurrency.
//
// The correctness argument, in order of what actually does the work:
//
//  1. Redis SET NX (5s TTL) is a CHEAP EARLY REJECT. Under a herd on one
//     popular slot it turns 99 database transactions into 99 Redis round trips.
//     It is not a correctness layer: Redis can drop the key on a failover, and
//     a lock with a TTL is not a mutex. If Redis is down we log and continue
//     (ADR-007). The release is token-guarded, so a caller whose TTL expired
//     mid-transaction cannot delete the lock somebody else has since taken
//     (ADR-006).
//
//  2. SELECT ... FOR UPDATE inside ONE transaction is the actual mutex. Every
//     concurrent booker for the slot queues on the row lock; under READ
//     COMMITTED the loser re-reads the row after the lock is granted and
//     observes the winner's committed BOOKED.
//
//  3. UPDATE ... WHERE version = $n, with RowsAffected() checked, catches any
//     path that reached the update without holding the row lock.
//
//  4. uq_appointments_slot_live is the last line: even a bug in 1-3 cannot
//     produce two live appointments for one slot, because Postgres will refuse
//     the second insert with 23505. That becomes a clean 409 SLOT_UNAVAILABLE.
//
// The outbox enqueue is inside the same transaction (ADR-005). The source
// documentation writes it after Commit, which loses the event whenever the
// process dies in that window -- a slot booked with nobody notified, which is
// the exact failure the outbox pattern exists to prevent.
func (s *Service) BookSlot(ctx context.Context, in BookSlotInput) (Appointment, error) {
	start := time.Now()
	appt, err := s.bookSlot(ctx, in)
	s.metrics.observeBooking(err, time.Since(start))
	return appt, err
}

func (s *Service) bookSlot(ctx context.Context, in BookSlotInput) (Appointment, error) {
	now := s.clock.Now()

	// The prepayment decision is read before the lock: it is a property of the
	// patient's history, not of the slot, and holding a lock while doing it
	// would lengthen the critical section for nothing.
	stats, err := s.repo.GetNoShowStats(ctx, s.pool, in.PatientID)
	if err != nil {
		return Appointment{}, err
	}
	prepaymentRequired := RequiresPrepayment(stats)

	// --- layer 1: cheap early reject -----------------------------------
	lockKey := SlotLockKey(in.SlotID)
	token, acquired, lockErr := s.locker.Acquire(ctx, lockKey, SlotLockTTL)
	switch {
	case lockErr != nil:
		// Fail open. Postgres is the truth; a Redis outage must degrade
		// throughput, never availability or correctness.
		s.log.Warn().Err(lockErr).Str("slot_id", maskID(in.SlotID)).
			Msg("slot lock unavailable, proceeding on Postgres alone")
	case !acquired:
		return Appointment{}, ErrSlotLocked
	default:
		defer func() {
			// The request context may already be cancelled by the time this
			// runs, and an unreleased lock would stall the slot for its whole
			// TTL. Use a detached context with a tight deadline.
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			if err := s.locker.Release(releaseCtx, lockKey, token); err != nil {
				// Losing the lock is self-healing: it carries a TTL. Worth a
				// line, not worth failing the booking that already committed.
				s.log.Debug().Err(err).Str("slot_id", maskID(in.SlotID)).Msg("slot lock release")
			}
		}()
	}

	// --- layers 2-4: one transaction ------------------------------------
	appointmentID := uuid.New()
	var appt Appointment

	err = database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		slot, err := s.repo.LockSlot(ctx, tx, in.SlotID)
		if err != nil {
			return err
		}

		// Mismatched doctor collapses to "not found" rather than "wrong
		// doctor": the API must not confirm which doctor owns an arbitrary id.
		if in.DoctorID != nil && *in.DoctorID != slot.DoctorID {
			return ErrSlotNotFound
		}
		if !slot.StartAt.After(now) {
			return ErrSlotInPast
		}
		if !slot.Bookable(in.PatientID, now) {
			if slot.Status == SlotBlocked && slot.ReservedUntil != nil && slot.ReservedUntil.After(now) {
				return ErrSlotReserved
			}
			return ErrSlotUnavailable
		}

		// One patient cannot be in two consultations at once, including with
		// two different doctors -- which the per-slot unique index cannot see.
		overlapping, err := s.repo.PatientHasOverlappingAppointment(ctx, tx, in.PatientID, slot.StartAt, slot.EndAt)
		if err != nil {
			return err
		}
		if overlapping {
			return ErrDuplicateBooking
		}

		// --- the quote -------------------------------------------------
		//
		// Read inside the transaction, and read BEFORE the appointment is
		// written, so a booking can never come into existence without a price.
		// A doctor with no pricing row fails the whole transaction here: the
		// slot is not marked booked, no event is enqueued, and the patient gets
		// a clear 422 instead of an appointment nobody can ever pay for.
		//
		// This value is a QUOTE. Once written it is never recomputed --
		// doctor_pricing may move underneath it a second later and this
		// appointment keeps the number the patient was shown.
		pricing, err := s.repo.GetDoctorPricing(ctx, tx, slot.DoctorID)
		if err != nil {
			return err
		}
		if !pricing.Quotable() {
			// A row exists but carries no usable fee. Same refusal as no row
			// at all: zero is a bug in the projection, not a free consultation.
			return fmt.Errorf("%w: doctor %s has a pricing row with fee_cents=%d",
				ErrDoctorNotPriced, maskID(slot.DoctorID), pricing.FeeCents)
		}

		var visitDOB *time.Time
		if !in.VisitPatientDOB.IsZero() {
			d := in.VisitPatientDOB.UTC()
			visitDOB = &d
		}
		appt = Appointment{
			ID:                    appointmentID,
			PatientID:             in.PatientID,
			DoctorID:              slot.DoctorID,
			SlotID:                slot.ID,
			SlotStartAt:           slot.StartAt,
			SlotEndAt:             slot.EndAt,
			Status:                AppointmentPendingPayment,
			Intake:                in.Intake,
			VisitPatientName:      strings.TrimSpace(in.VisitPatientName),
			VisitPatientDOB:       visitDOB,
			VisitPatientSex:       strings.TrimSpace(in.VisitPatientSex),
			VisitPatientWeightKg:  in.VisitPatientWeightKg,
			VisitPatientAllergies: strings.TrimSpace(in.VisitPatientAllergies),
			PrepaymentRequired:    prepaymentRequired,
			AmountCents:           pricing.FeeCents,
			Currency:              pricing.Currency,
			Specialty:             pricing.Specialty,
		}
		if err := s.repo.InsertAppointment(ctx, tx, &appt); err != nil {
			// Layer 4 firing. Another transaction committed a live appointment
			// for this slot between our FOR UPDATE and our insert, which should
			// be impossible -- so record it, then answer honestly.
			if database.IsUniqueViolation(err) {
				s.log.Warn().Str("slot_id", maskID(in.SlotID)).
					Msg("unique index rejected a duplicate booking; row lock was bypassed")
				s.metrics.UniqueViolations.Inc()
				return ErrSlotUnavailable
			}
			return err
		}

		rows, err := s.repo.MarkSlotBooked(ctx, tx, slot.ID, slot.StartAt, appointmentID, slot.Version)
		if err != nil {
			return err
		}
		if rows != 1 {
			// Layer 3 firing: the row moved underneath us.
			s.metrics.VersionConflicts.Inc()
			return ErrVersionConflict
		}

		// Booking counts toward the patient's denominator immediately, so the
		// no-show rate cannot be gamed by never completing.
		if err := s.repo.BumpNoShowCounter(ctx, tx, in.PatientID, "booked", now); err != nil {
			return err
		}

		// A waitlisted patient taking their reserved slot closes their entry.
		if slot.IsReservedFor(in.PatientID, now) {
			if _, err := s.repo.MarkWaitlistBookedBySlot(ctx, tx, slot.ID, in.PatientID); err != nil {
				return err
			}
		}

		expiresAt := now.Add(UnpaidBookingWindow)
		if err := s.outbox.Enqueue(ctx, tx, events.SubjectAppointmentCreated, appointmentID.String(),
			events.AppointmentCreated{
				AppointmentID: appointmentID,
				PatientID:     in.PatientID,
				DoctorID:      slot.DoctorID,
				SlotID:        slot.ID,
				StartAt:       slot.StartAt,
				EndAt:         slot.EndAt,
				Status:        string(AppointmentPendingPayment),
				// The three fields whose absence meant no patient could pay.
				AmountCents:        appt.AmountCents,
				Currency:           appt.Currency,
				Specialty:          appt.Specialty,
				PrepaymentRequired: prepaymentRequired,
				ExpiresAt:          expiresAt,
				CreatedAt:          now,
			}); err != nil {
			return err
		}
		return s.outbox.Enqueue(ctx, tx, events.SubjectSlotBooked, slot.ID.String(), events.SlotBooked{
			SlotID:        slot.ID,
			DoctorID:      slot.DoctorID,
			AppointmentID: appointmentID,
			StartAt:       slot.StartAt,
			EndAt:         slot.EndAt,
		})
	})
	if err != nil {
		return Appointment{}, err
	}

	// Nothing PHI-bearing here: ids are masked and intake is never logged.
	s.log.Info().
		Str("appointment_id", maskID(appointmentID)).
		Str("slot_id", maskID(in.SlotID)).
		Bool("prepayment_required", prepaymentRequired).
		Int64("amount_cents", appt.AmountCents).
		Str("currency", appt.Currency).
		Msg("slot booked")
	return appt, nil
}

// ---------------------------------------------------------------------------
// Cancellation
// ---------------------------------------------------------------------------

// CancelInput is a cancellation request.
type CancelInput struct {
	AppointmentID uuid.UUID
	// ActorID is the authenticated caller: the patient's user id, the doctor's
	// doctor id, or an administrator's user id.
	ActorID   uuid.UUID
	ActorRole string // "patient" | "doctor" | "admin"
	Reason    string
	// Force lets an administrator cancel an appointment whose slot has already
	// started -- resolving a stuck consultation, per the admin runbook.
	Force bool
}

// CancelAppointment releases the slot and publishes the refund decision.
//
// The refund policy is decided here and only here. Publishing the *decision*
// rather than the raw timestamps means the two-hour window is defined once; if
// the payment service re-derived it, the two definitions would drift the first
// time somebody changed one of them.
func (s *Service) CancelAppointment(ctx context.Context, in CancelInput) (Appointment, error) {
	now := s.clock.Now()
	var appt Appointment
	var released bool

	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		// Lock order is appointment then slot, everywhere. Booking takes only
		// the slot lock and inserts a new appointment row, so there is no cycle.
		a, err := s.repo.LockAppointment(ctx, tx, in.AppointmentID)
		if err != nil {
			return err
		}
		if err := authorizeAppointment(a, in.ActorID, in.ActorRole); err != nil {
			return err
		}
		if a.Status != AppointmentPendingPayment && a.Status != AppointmentConfirmed {
			return ErrAppointmentNotCancellable
		}
		if !in.Force && !a.SlotStartAt.After(now) {
			return ErrAppointmentAlreadyStarted
		}

		doctorInitiated := in.ActorRole != "patient"
		policy := RefundPolicyFor(a.SlotStartAt, now, doctorInitiated)

		actor := in.ActorID
		rows, err := s.repo.CancelAppointment(ctx, tx, a.ID, a.Version, &actor, in.ActorRole, in.Reason, policy, now)
		if err != nil {
			return err
		}
		if rows != 1 {
			return ErrVersionConflict
		}

		slot, err := s.repo.LockSlot(ctx, tx, a.SlotID)
		switch {
		case errors.Is(err, ErrSlotNotFound):
			// The slot has been archived out from under a very old appointment.
			// The cancellation still stands; there is simply nothing to free.
			s.log.Warn().Str("appointment_id", maskID(a.ID)).Msg("cancelling appointment whose slot is archived")
		case err != nil:
			return err
		default:
			// Only release a slot this appointment actually holds. If an
			// administrator already re-blocked it, leave their intent alone.
			if slot.Status == SlotBooked && slot.AppointmentID != nil && *slot.AppointmentID == a.ID {
				n, err := s.repo.ReleaseSlot(ctx, tx, slot.ID, slot.StartAt, slot.Version)
				if err != nil {
					return err
				}
				if n != 1 {
					return ErrVersionConflict
				}
				released = true
			}
		}

		if err := s.repo.BumpNoShowCounter(ctx, tx, a.PatientID, "cancelled", now); err != nil {
			return err
		}

		a.Status = AppointmentCancelled
		a.CancelledAt = &now
		a.CancelledBy = &actor
		a.CancelledByRole = in.ActorRole
		a.CancellationReason = in.Reason
		a.RefundPolicy = &policy
		a.Version++
		a.UpdatedAt = now
		appt = a

		if err := s.outbox.Enqueue(ctx, tx, events.SubjectAppointmentCancelled, a.ID.String(),
			events.AppointmentCancelled{
				AppointmentID: a.ID,
				PatientID:     a.PatientID,
				DoctorID:      a.DoctorID,
				SlotID:        a.SlotID,
				StartAt:       a.SlotStartAt,
				// The canonical CancelledBy is the ROLE, not the actor's id.
				// The actor uuid stays in this service's cancelled_by column
				// where it is auditable; broadcasting which admin cancelled a
				// consultation to every consumer on the bus buys nothing.
				CancelledBy: in.ActorRole,
				// F20(d): in.Reason is caller-authored free text (up to 500
				// characters, patient or doctor or admin) and it is NOT
				// broadcast. eventReasonCode passes machine codes and drops
				// prose. The text itself is already committed to
				// appointments.cancellation_reason a few lines above, which is
				// where a party to this appointment reads it.
				Reason: eventReasonCode(in.Reason),
				NoShow: false,
				// Scheduling owns the cancellation policy. payment-service must
				// not re-derive the window from StartAt and CancelledAt, or the
				// policy exists in two places and drifts.
				RefundPolicy:  string(policy),
				RefundPercent: policy.Percent(),
				// The moment cancellation was REQUESTED. Pricing the refund off
				// receipt time silently turns a full refund into a partial one
				// whenever this event sits in a queue past the policy boundary.
				CancelledAt: now,
			}); err != nil {
			return err
		}
		if released {
			return s.outbox.Enqueue(ctx, tx, events.SubjectSlotReleased, a.SlotID.String(), events.SlotReleased{
				SlotID:   a.SlotID,
				DoctorID: a.DoctorID,
				StartAt:  a.SlotStartAt,
				EndAt:    a.SlotEndAt,
				Reason:   "cancelled",
			})
		}
		return nil
	})
	if err != nil {
		return Appointment{}, err
	}

	s.log.Info().
		Str("appointment_id", maskID(in.AppointmentID)).
		Str("actor_role", in.ActorRole).
		Str("refund_policy", string(*appt.RefundPolicy)).
		Bool("slot_released", released).
		Msg("appointment cancelled")

	// Promotion runs after the commit, in its own transaction. Doing it inside
	// would hold the slot's row lock while we talk to Redis; failing it here
	// leaves the slot plainly AVAILABLE, which is correct, just less kind to
	// the waitlist. The offer sweeper picks up the slack.
	if released {
		if err := s.PromoteWaitlist(context.WithoutCancel(ctx), appt.DoctorID, appt.SlotID); err != nil {
			s.log.Error().Err(err).Str("slot_id", maskID(appt.SlotID)).Msg("waitlist promotion failed")
		}
	}
	return appt, nil
}

// Actor roles. Every authorization decision in this service switches on one of
// these, and they are constants rather than bare literals so that a typo is a
// compile error instead of a silent fall-through to the default branch.
//
// ActorService and ActorSystem are NOT reachable from a request body. A caller
// cannot name their own role anywhere: the HTTP handlers derive it from the
// verified token (handler.go's actor()), the gRPC handlers derive it from the
// verified service principal, and ActorSystem is set only by this package's own
// jobs and consumers.
const (
	ActorPatient = "patient"
	ActorDoctor  = "doctor"
	ActorAdmin   = "admin"
	// ActorService is an authenticated internal caller holding a service-role
	// token -- the gRPC mesh. It reads; it is not a party and it never writes.
	ActorService = "service"
	// ActorSystem is this service acting on its own behalf: the unpaid-timeout
	// sweeper, the no-show job, the leave cascade.
	ActorSystem = "system"
)

// authorizeAppointment enforces ownership. Failures collapse into
// ErrAppointmentNotFound so the API cannot be used to probe which appointment
// ids exist.
//
// The nil-actor guard is the structural half of F5. The specific bug was the
// gRPC GetAppointment handler treating an empty actor_id as an admin, but the
// reason that bug could exist is that this function accepted a zero actor for
// the two roles that do not compare it -- so any caller that reached it having
// forgotten to set an actor got the widest possible answer. Refusing uuid.Nil
// for EVERY role makes the whole class impossible rather than fixing the one
// instance: "unset" can never again be more powerful than "set".
func authorizeAppointment(a Appointment, actorID uuid.UUID, role string) error {
	if actorID == uuid.Nil {
		return ErrForbidden
	}
	switch role {
	case ActorPatient:
		if a.PatientID != actorID {
			return ErrAppointmentNotFound
		}
	case ActorDoctor:
		if a.DoctorID != actorID {
			return ErrAppointmentNotFound
		}
	case ActorAdmin:
		// An administrator resolving a named dispute reads the appointment they
		// were given the id of. Note what this is NOT: a way to enumerate. The
		// bulk read is gone from the patient surface entirely -- see
		// ListMyAppointments and ListAppointmentsForAdmin.
		return nil
	case ActorService:
		// A verified internal caller. consultation-service legitimately
		// resolves appointments it is not a party to; the projection it gets
		// back over gRPC carries no intake, so this is an operational read and
		// not a clinical one.
		return nil
	default:
		return ErrForbidden
	}
	return nil
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// LastSelfVisitWeightKg is the patient's most recent self-reported weight; see
// Repository.LastSelfVisitWeightKg for which bookings count.
func (s *Service) LastSelfVisitWeightKg(ctx context.Context, patientID uuid.UUID) (*float64, error) {
	return s.repo.LastSelfVisitWeightKg(ctx, s.pool, patientID)
}

// GetAppointment reads one appointment, enforcing ownership.
func (s *Service) GetAppointment(ctx context.Context, id, actorID uuid.UUID, role string) (Appointment, error) {
	a, err := s.repo.GetAppointment(ctx, s.pool, id)
	if err != nil {
		return Appointment{}, err
	}
	if err := authorizeAppointment(a, actorID, role); err != nil {
		return Appointment{}, err
	}
	return a, nil
}

// ListMyAppointments pages the caller's OWN appointments. "My" is the whole
// contract: this returns rows the caller is a party to, and nothing else.
//
// The admin branch is gone, and its removal is F4. It used to apply no filter
// at all, so a single GET /api/v1/appointments with any of the five admin-role
// tokens returned the entire appointments table -- every patient, every doctor,
// every consultation time on the platform, paged. The comment that stood here
// defended it with "the admin surface is IP-allowlisted and audited
// separately". That was true of /api/v1/admin/*; it was not true of this route,
// which is auth: "authenticated" in the gateway's table with no roles, no
// IPAllowlist, no restrictOrigin and no audit. The prefix was doing the work
// the middleware was supposed to do.
//
// Where the line is drawn, and why there:
//
//   - Resolving a double-booking means looking at ONE doctor's calendar over a
//     bounded window. That is a real operational need and it survives, on
//     ListAppointmentsForAdmin, behind the IP allowlist.
//   - Force-cancelling means acting on ONE appointment whose id you were
//     given. That is unchanged.
//   - Reading a named patient's appointment history is neither of those. It is
//     a clinical timeline -- who they saw, how often, and for a specialist,
//     what for -- and no dispute needs it. There is no filter that makes it
//     available, deliberately: ListAppointmentsForAdmin refuses a patient_id
//     rather than accepting one.
func (s *Service) ListMyAppointments(ctx context.Context, actorID uuid.UUID, role string, status *AppointmentStatus, limit, offset int) ([]Appointment, int64, error) {
	if actorID == uuid.Nil {
		return nil, 0, ErrForbidden
	}
	f := AppointmentFilter{Status: status, Limit: limit, Offset: offset}
	switch role {
	case ActorPatient:
		f.PatientID = &actorID
	case ActorDoctor:
		f.DoctorID = &actorID
	default:
		return nil, 0, ErrForbidden
	}
	return s.repo.ListAppointments(ctx, s.pool, f)
}

// AdminListInput is a scoped administrative listing. Every field that bounds
// the query is required, because an administrative read that is not bounded is
// a bulk PHI export.
type AdminListInput struct {
	// DoctorID is required. The query this endpoint exists to answer is "what
	// does this doctor's calendar look like around the clash", and a doctor is
	// the axis that answers it.
	DoctorID uuid.UUID
	// From and To bound the window. Required, and capped at
	// MaxAdminListWindow.
	From, To time.Time
	Status   *AppointmentStatus
	Limit    int
	Offset   int
}

// MaxAdminListWindow caps an administrative listing. A month is comfortably
// more than a double-booking investigation needs and far less than a year of a
// doctor's practice.
const MaxAdminListWindow = 31 * 24 * time.Hour

// ListAppointmentsForAdmin is the scoped replacement for the unfiltered admin
// listing removed from ListMyAppointments. It is mounted only under /admin,
// which carries IPAllowlist + RequireRole(AdminRoles...).
//
// It never accepts a patient_id: see ListMyAppointments for why. The rows it
// returns are rendered without intake by the handler.
func (s *Service) ListAppointmentsForAdmin(ctx context.Context, in AdminListInput) ([]Appointment, int64, error) {
	if in.DoctorID == uuid.Nil {
		return nil, 0, fmt.Errorf("%w: doctor_id is required", ErrAdminScopeRequired)
	}
	if in.From.IsZero() || in.To.IsZero() {
		return nil, 0, fmt.Errorf("%w: from and to are required", ErrAdminScopeRequired)
	}
	if !in.To.After(in.From) {
		return nil, 0, fmt.Errorf("%w: to must be after from", ErrAdminScopeRequired)
	}
	if in.To.Sub(in.From) > MaxAdminListWindow {
		return nil, 0, fmt.Errorf("%w: window may not exceed %d days",
			ErrAdminScopeRequired, int(MaxAdminListWindow/(24*time.Hour)))
	}

	from, to := in.From, in.To
	return s.repo.ListAppointments(ctx, s.pool, AppointmentFilter{
		DoctorID: &in.DoctorID,
		From:     &from,
		To:       &to,
		Status:   in.Status,
		Limit:    in.Limit,
		Offset:   in.Offset,
	})
}

// ---------------------------------------------------------------------------
// Terminal transitions
// ---------------------------------------------------------------------------

// MarkTerminal records a completed consultation or a no-show.
func (s *Service) MarkTerminal(ctx context.Context, appointmentID, actorID uuid.UUID, role string, status AppointmentStatus) (Appointment, error) {
	if status != AppointmentCompleted && status != AppointmentNoShow {
		return Appointment{}, fmt.Errorf("scheduling: %w: %s is not a terminal status", ErrForbidden, status)
	}
	now := s.clock.Now()
	var appt Appointment

	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		a, err := s.repo.LockAppointment(ctx, tx, appointmentID)
		if err != nil {
			return err
		}
		if role != "system" {
			if err := authorizeAppointment(a, actorID, role); err != nil {
				return err
			}
			// A patient may not declare their own no-show, and may not close
			// out a consultation to dodge one.
			if role == "patient" {
				return ErrForbidden
			}
		}
		if a.Status != AppointmentConfirmed {
			return ErrAppointmentNotCancellable
		}

		rows, err := s.repo.MarkAppointmentTerminal(ctx, tx, a.ID, status, now)
		if err != nil {
			return err
		}
		if rows != 1 {
			return ErrVersionConflict
		}

		outcome := "completed"
		subject := events.SubjectAppointmentCompleted
		if status == AppointmentNoShow {
			outcome = "no_show"
			subject = events.SubjectAppointmentNoShow
		}
		if err := s.repo.BumpNoShowCounter(ctx, tx, a.PatientID, outcome, now); err != nil {
			return err
		}

		a.Status = status
		a.Version++
		a.UpdatedAt = now
		if status == AppointmentNoShow {
			a.NoShowAt = &now
		} else {
			a.CompletedAt = &now
		}
		appt = a

		// events.AppointmentTerminal carries no refund policy, and that is
		// correct rather than a loss: a completion refunds nothing and a
		// no-show is priced by the cancellation event, so a second refund
		// signal here would be a second place for the policy to live.
		return s.outbox.Enqueue(ctx, tx, subject, a.ID.String(), events.AppointmentTerminal{
			AppointmentID: a.ID,
			PatientID:     a.PatientID,
			DoctorID:      a.DoctorID,
			StartAt:       a.SlotStartAt,
			OccurredAt:    now,
		})
	})
	if err != nil {
		return Appointment{}, err
	}
	return appt, nil
}

// ---------------------------------------------------------------------------
// Administrative overrides
// ---------------------------------------------------------------------------

// BlockSlot withdraws a slot from the market. It refuses to silently discard a
// booking: an administrator who wants to free a booked slot force-cancels the
// appointment first, which is the path that publishes a refund decision.
func (s *Service) BlockSlot(ctx context.Context, slotID uuid.UUID, status SlotStatus, reason string) (Slot, error) {
	if status != SlotBlocked && status != SlotCancelled && status != SlotAvailable {
		return Slot{}, fmt.Errorf("scheduling: %w: cannot set slot to %s", ErrForbidden, status)
	}
	var out Slot
	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		slot, err := s.repo.LockSlot(ctx, tx, slotID)
		if err != nil {
			return err
		}
		if slot.Status == SlotBooked {
			return ErrSlotUnavailable
		}
		rows, err := s.repo.SetSlotStatus(ctx, tx, slot.ID, slot.StartAt, status, slot.Version)
		if err != nil {
			return err
		}
		if rows != 1 {
			return ErrVersionConflict
		}
		slot.Status = status
		slot.Version++
		slot.UpdatedAt = s.clock.Now()
		slot.ReservedFor, slot.ReservedUntil = nil, nil
		out = slot

		if status == SlotAvailable {
			return s.outbox.Enqueue(ctx, tx, events.SubjectSlotReleased, slot.ID.String(), events.SlotReleased{
				SlotID:   slot.ID,
				DoctorID: slot.DoctorID,
				StartAt:  slot.StartAt,
				EndAt:    slot.EndAt,
				Reason:   "admin_override",
			})
		}
		return nil
	})
	if err != nil {
		return Slot{}, err
	}
	s.log.Warn().
		Str("slot_id", maskID(slotID)).
		Str("status", string(status)).
		Str("reason", reason).
		Msg("slot status overridden by administrator")
	return out, nil
}

// SetHoliday is the administrative path, kept as a thin wrapper over AddHoliday
// so there is exactly one implementation of what a holiday does.
//
// It preserves the behaviour this endpoint has always had -- excluding the day
// from future generation runs and leaving already-materialised slots alone --
// unless the caller opts in with applyToExisting. That default is deliberate
// and is not the same choice made for a doctor registering their own leave:
//
//   - Platform-wide (nil doctorID) it is the ONLY safe choice. Withdrawing
//     every doctor's calendar for a national holiday is a mass cancellation
//     across the whole marketplace; it needs an operational procedure with a
//     rollback plan, not a POST that quietly does it. AddHoliday refuses to
//     touch existing slots for a platform-wide entry regardless of this flag.
//   - Doctor-scoped, opting in makes the admin path do exactly what the
//     doctor's own endpoint does, cancellations and refunds included.
//
// A doctor registering their OWN leave gets applyToExisting = true, because
// leave that patients can still book into is not leave.
func (s *Service) SetHoliday(ctx context.Context, doctorID *uuid.UUID, date Date, reason string,
	applyToExisting, cancelBooked bool,
) (HolidayEffect, error) {
	effect, err := s.AddHoliday(ctx, AddHolidayInput{
		DoctorID:        doctorID,
		Date:            date,
		Reason:          reason,
		ApplyToExisting: applyToExisting,
		CancelBooked:    cancelBooked,
	})
	if err != nil {
		return effect, err
	}
	scope := "platform"
	if doctorID != nil {
		scope = maskID(*doctorID)
	}
	s.log.Warn().
		Str("scope", scope).
		Str("date", date.String()).
		Str("reason", reason).
		Bool("applied_to_existing", applyToExisting).
		Msg("holiday recorded by administrator")
	return effect, nil
}

// ---------------------------------------------------------------------------
// Payment-driven transitions (called by the event consumers)
// ---------------------------------------------------------------------------

// ConfirmAppointment is the payment.succeeded path.
func (s *Service) ConfirmAppointment(ctx context.Context, tx pgx.Tx, appointmentID uuid.UUID, paymentID *uuid.UUID) error {
	now := s.clock.Now()
	a, err := s.repo.LockAppointment(ctx, tx, appointmentID)
	if err != nil {
		return err
	}
	if a.Status == AppointmentConfirmed {
		return nil // already confirmed; at-least-once delivery is normal
	}
	if a.Status != AppointmentPendingPayment {
		// Paying for a cancelled appointment is a real scenario (a webhook
		// racing the unpaid sweeper). Refunding is the payment service's job;
		// ours is to refuse to resurrect a released slot.
		s.log.Warn().
			Str("appointment_id", maskID(appointmentID)).
			Str("status", string(a.Status)).
			Msg("payment succeeded for an appointment that is no longer pending")
		return nil
	}
	rows, err := s.repo.ConfirmAppointment(ctx, tx, appointmentID, paymentID, now)
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrVersionConflict
	}
	return s.outbox.Enqueue(ctx, tx, events.SubjectAppointmentConfirmed, appointmentID.String(),
		events.AppointmentConfirmed{
			AppointmentID: a.ID,
			PatientID:     a.PatientID,
			DoctorID:      a.DoctorID,
			SlotID:        a.SlotID,
			StartAt:       a.SlotStartAt,
			EndAt:         a.SlotEndAt,
			PaymentID:     derefUUID(paymentID),
			ConfirmedAt:   now,
		})
}

// ReleaseForFailedPayment is the payment.failed and unpaid-timeout path: cancel
// the appointment, hand the slot back, and say why.
func (s *Service) ReleaseForFailedPayment(ctx context.Context, tx pgx.Tx, appointmentID uuid.UUID, reason string) (released bool, doctorID, slotID uuid.UUID, err error) {
	now := s.clock.Now()

	a, err := s.repo.LockAppointment(ctx, tx, appointmentID)
	if err != nil {
		return false, uuid.Nil, uuid.Nil, err
	}
	if a.Status != AppointmentPendingPayment {
		return false, a.DoctorID, a.SlotID, nil
	}

	policy := RefundNone
	rows, err := s.repo.CancelAppointment(ctx, tx, a.ID, a.Version, nil, "system", reason, policy, now)
	if err != nil {
		return false, uuid.Nil, uuid.Nil, err
	}
	if rows != 1 {
		return false, uuid.Nil, uuid.Nil, ErrVersionConflict
	}

	slot, err := s.repo.LockSlot(ctx, tx, a.SlotID)
	if err != nil && !errors.Is(err, ErrSlotNotFound) {
		return false, uuid.Nil, uuid.Nil, err
	}
	if err == nil && slot.Status == SlotBooked && slot.AppointmentID != nil && *slot.AppointmentID == a.ID {
		n, relErr := s.repo.ReleaseSlot(ctx, tx, slot.ID, slot.StartAt, slot.Version)
		if relErr != nil {
			return false, uuid.Nil, uuid.Nil, relErr
		}
		if n != 1 {
			return false, uuid.Nil, uuid.Nil, ErrVersionConflict
		}
		released = true
	}

	if err := s.outbox.Enqueue(ctx, tx, events.SubjectAppointmentCancelled, a.ID.String(),
		events.AppointmentCancelled{
			AppointmentID: a.ID,
			PatientID:     a.PatientID,
			DoctorID:      a.DoctorID,
			SlotID:        a.SlotID,
			StartAt:       a.SlotStartAt,
			CancelledBy:   "system",
			// The payment provider's failure text is not ours to broadcast
			// either -- "card declined for J. Perera" is a real Stripe string.
			// The "payment_failed" fallback the consumer supplies is a code and
			// survives; provider prose does not.
			Reason:        eventReasonCode(reason),
			NoShow:        false,
			RefundPolicy:  string(policy),
			RefundPercent: policy.Percent(),
			CancelledAt:   now,
		}); err != nil {
		return false, uuid.Nil, uuid.Nil, err
	}

	if released {
		if err := s.outbox.Enqueue(ctx, tx, events.SubjectSlotReleased, a.SlotID.String(), events.SlotReleased{
			SlotID:   a.SlotID,
			DoctorID: a.DoctorID,
			StartAt:  a.SlotStartAt,
			EndAt:    a.SlotEndAt,
			Reason:   eventReasonCode(reason),
		}); err != nil {
			return false, uuid.Nil, uuid.Nil, err
		}
	}
	return released, a.DoctorID, a.SlotID, nil
}
