package scheduling

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
)

// Holiday is one non-working day. A nil DoctorID means platform-wide -- a Poya
// day, Independence Day, a national closure -- and belongs to the
// administrator; a set DoctorID is one doctor's own leave and belongs to them.
type Holiday struct {
	ID        uuid.UUID
	DoctorID  *uuid.UUID
	Date      Date
	Reason    string
	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PlatformWide reports whether this entry closes the whole platform rather than
// one doctor's calendar.
func (h Holiday) PlatformWide() bool { return h.DoctorID == nil }

// HolidayEffect is what registering (or lifting) a holiday actually did to the
// calendar. It is returned to the caller and rendered to the doctor, because
// "your leave is saved" is not a sufficient answer when the save cancelled four
// people's consultations.
type HolidayEffect struct {
	Holiday Holiday
	// SlotsWithdrawn is how many already-generated, unbooked slots were taken
	// off the market.
	SlotsWithdrawn int
	// SlotsRestored is how many withdrawn slots were put back, when leave is
	// lifted.
	SlotsRestored int
	// AppointmentsCancelled is how many patients were cancelled and refunded.
	AppointmentsCancelled int
	// BlockedByAppointments is how many live appointments stopped the request,
	// when the caller did not ask for them to be cancelled. It is non-zero only
	// alongside ErrHolidayHasBookings.
	BlockedByAppointments int
}

// HolidayLeaveReason is the cancellation reason stamped on an appointment that
// a doctor's leave displaced. It is a constant because notification-service
// keys template selection off the reason string, and a typo there is a patient
// who is told nothing.
const HolidayLeaveReason = "doctor_on_leave"

// slotWithdrawalReason is carried on events.SlotWithdrawn so an operator
// reading a replayed stream can tell leave from whatever reason comes next.
const slotWithdrawalReason = "holiday"

// AddHolidayInput is a request to register a non-working day.
type AddHolidayInput struct {
	// DoctorID nil means platform-wide, which only the admin surface may ask
	// for.
	DoctorID *uuid.UUID
	Date     Date
	Reason   string

	// CancelBooked is the doctor's explicit "yes, cancel my patients".
	//
	// It defaults to false and the request FAILS with ErrHolidayHasBookings
	// when the day already has live appointments. That refusal is the whole
	// design: a doctor tapping a date in a calendar picker does not
	// necessarily know six people are booked that morning, and neither
	// available outcome of guessing is acceptable. Registering the leave and
	// leaving the appointments standing tells the doctor they are off while
	// patients still expect them. Registering it and cancelling silently
	// destroys six consultations with no one deciding to.
	//
	// So the service refuses, reports the count, and makes the doctor choose.
	CancelBooked bool

	// ApplyToExisting withdraws slots that were already generated for the day.
	//
	// True is the right default for a doctor registering their own leave: the
	// leave is worthless if patients can still book into it. It is exposed as a
	// field because the platform-wide admin path deliberately does NOT do this
	// -- see AddHoliday.
	ApplyToExisting bool
}

// AddHoliday registers a non-working day and makes it true of the calendar that
// already exists, not only of the calendar that will be generated tomorrow.
//
// THE PROBLEM THIS SOLVES
// Slots are materialised up to advance_days ahead. A holiday registered today
// for a date inside that window arrives after its slots already exist, so
// excluding it from future generation runs changes nothing at all: the rows are
// already there, already AVAILABLE, and patients keep booking them. Before this
// method, SetHoliday's own doc comment said as much -- "It only affects future
// generation runs" -- which made doctor-registered leave a no-op for exactly
// the four weeks a doctor is most likely to be planning.
//
// WHAT HAPPENS TO EACH KIND OF SLOT
//   - AVAILABLE, or BLOCKED by a lapsed waitlist reservation: set to CANCELLED
//     and announced as events.SlotWithdrawn. CANCELLED is the status the model
//     already defines as "the doctor withdrew the hour", and withdrawn is a
//     different fact from released -- released puts a slot back on the market,
//     which on a holiday is precisely wrong.
//   - BOOKED: the appointment is cancelled with a FULL refund (the platform
//     broke the appointment, so the patient is made whole regardless of
//     notice), events.AppointmentCancelled is published so the patient is told,
//     and only then is the slot withdrawn. Nothing vanishes silently.
//
// The waitlist is deliberately NOT promoted into the freed slots. Promotion
// exists to fill a cancellation; filling a slot on a day the doctor is away
// would re-create the problem this method just solved.
//
// CONCURRENCY
// Everything happens in ONE transaction, and the lock order is the platform's:
// appointments first, then slots. Booking takes only the slot lock, so a
// booking racing this call either commits first -- and is then found by the
// appointment lock, counted, and either blocks the request or is cancelled --
// or blocks on the slot lock and afterwards observes a CANCELLED slot and fails
// with ErrSlotUnavailable. There is no interleaving that leaves a patient
// booked into a withdrawn slot; TestHolidayRaceWithBooking asserts it.
func (s *Service) AddHoliday(ctx context.Context, in AddHolidayInput) (HolidayEffect, error) {
	if in.Date.IsZero() {
		return HolidayEffect{}, ErrHolidayDateInvalid
	}
	now := s.clock.Now()
	today := DateIn(now, s.loc)
	if in.Date.Before(today) {
		// Leave in the past changes nothing that can still be booked and would
		// only pollute the generator's exclusion set.
		return HolidayEffect{}, ErrHolidayDateInPast
	}

	var effect HolidayEffect

	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		effect = HolidayEffect{}

		from := in.Date.StartOfDay(s.loc).UTC()
		to := in.Date.EndOfDay(s.loc).UTC()

		// A platform-wide holiday touches no existing slot: withdrawing every
		// doctor's calendar for a national holiday is a mass cancellation, and
		// that belongs to a deliberate operational procedure with a rollback
		// plan, not to a single POST. It still excludes the day from every
		// future generation run, which is what the admin path has always done.
		if in.DoctorID == nil || !in.ApplyToExisting {
			h, err := s.repo.UpsertHoliday(ctx, tx, in.DoctorID, in.Date, in.Reason)
			if err != nil {
				return err
			}
			effect.Holiday = h
			return nil
		}
		doctorID := *in.DoctorID

		// Lock order: appointments, then slots -- the same order
		// CancelAppointment uses. Reversing it here would close a deadlock
		// cycle with every concurrent cancellation on the platform.
		live, err := s.repo.LockLiveAppointmentsInRange(ctx, tx, doctorID, from, to)
		if err != nil {
			return err
		}

		slots, err := s.repo.LockSlotsInRange(ctx, tx, doctorID, from, to)
		if err != nil {
			return err
		}

		// Re-read now that every slot for the day is locked. A booking that
		// committed between the appointment lock above and the slot lock just
		// taken is invisible to the first query and fully visible to this one.
		// Aborting is correct and converges on retry: the caller re-issues an
		// idempotent request and the second attempt sees the new appointment.
		recheck, err := s.repo.LockLiveAppointmentsInRange(ctx, tx, doctorID, from, to)
		if err != nil {
			return err
		}
		if len(recheck) != len(live) {
			return ErrVersionConflict
		}
		live = recheck

		if len(live) > 0 && !in.CancelBooked {
			effect.BlockedByAppointments = len(live)
			return ErrHolidayHasBookings
		}

		h, err := s.repo.UpsertHoliday(ctx, tx, in.DoctorID, in.Date, in.Reason)
		if err != nil {
			return err
		}
		effect.Holiday = h

		for i := range live {
			if err := s.cancelForLeave(ctx, tx, live[i], in.Reason, now); err != nil {
				return err
			}
			effect.AppointmentsCancelled++
		}

		for i := range slots {
			withdrawn, err := s.withdrawSlot(ctx, tx, slots[i])
			if err != nil {
				return err
			}
			if withdrawn {
				effect.SlotsWithdrawn++
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrHolidayHasBookings) {
			return effect, err
		}
		return HolidayEffect{}, err
	}

	scope := "platform"
	if in.DoctorID != nil {
		scope = maskID(*in.DoctorID)
	}
	s.log.Info().
		Str("scope", scope).
		Str("date", in.Date.String()).
		Int("slots_withdrawn", effect.SlotsWithdrawn).
		Int("appointments_cancelled", effect.AppointmentsCancelled).
		Msg("holiday registered")
	return effect, nil
}

// cancelForLeave cancels one appointment displaced by a doctor's leave.
//
// The refund is FULL, unconditionally. RefundPolicyFor already says so for any
// doctor-initiated cancellation, and it is the right rule: the patient did
// nothing, and charging them a late-cancellation fee because their doctor took
// leave would be indefensible. It is spelled out here rather than derived so
// that a future change to the notice window cannot quietly start charging these
// patients.
func (s *Service) cancelForLeave(ctx context.Context, tx pgx.Tx, a Appointment, reason string, now time.Time) error {
	policy := RefundFull

	// The doctor's own words about why they are away belong on the appointment
	// row, where the patient they displaced can read them over the API. Before
	// F20(d) they were stored nowhere and broadcast everywhere: the row got the
	// bare code and the sentence went onto events.AppointmentCancelled, out to
	// every consumer on the shared stream and into an SMS body. That is exactly
	// backwards. Row, not bus.
	stored := HolidayLeaveReason
	if reason != "" {
		stored = HolidayLeaveReason + ": " + reason
	}

	rows, err := s.repo.CancelAppointment(ctx, tx, a.ID, a.Version, nil, "doctor", stored, policy, now)
	if err != nil {
		return err
	}
	if rows != 1 {
		// The row moved under a lock we hold, which should be impossible.
		return ErrVersionConflict
	}

	if err := s.repo.BumpNoShowCounter(ctx, tx, a.PatientID, "cancelled", now); err != nil {
		return err
	}

	// cancelled_by is nil in the row above and the ROLE travels on the event,
	// same as every other cancellation: the doctor did not personally press a
	// button on this appointment, they registered leave and the platform acted.
	// Naming a specific actor uuid would misrepresent that.
	// The event carries the CODE only. Every consumer that steers on this
	// field -- notification's template choice, the two analytics projectors --
	// wants "doctor_on_leave", and none of them wants the sentence.
	return s.outbox.Enqueue(ctx, tx, events.SubjectAppointmentCancelled, a.ID.String(),
		events.AppointmentCancelled{
			AppointmentID: a.ID,
			PatientID:     a.PatientID,
			DoctorID:      a.DoctorID,
			SlotID:        a.SlotID,
			StartAt:       a.SlotStartAt,
			CancelledBy:   "doctor",
			Reason:        eventReasonCode(HolidayLeaveReason),
			NoShow:        false,
			RefundPolicy:  string(policy),
			RefundPercent: policy.Percent(),
			CancelledAt:   now,
		})
}

// withdrawSlot takes one slot off the market permanently. It reports whether it
// changed anything: a slot already CANCELLED is left alone, which is what makes
// re-registering the same holiday a no-op instead of an event storm.
func (s *Service) withdrawSlot(ctx context.Context, tx pgx.Tx, slot Slot) (bool, error) {
	if slot.Status == SlotCancelled {
		return false, nil
	}
	rows, err := s.repo.SetSlotStatus(ctx, tx, slot.ID, slot.StartAt, SlotCancelled, slot.Version)
	if err != nil {
		return false, err
	}
	if rows != 1 {
		return false, ErrVersionConflict
	}
	err = s.outbox.Enqueue(ctx, tx, events.SubjectSlotWithdrawn, slot.ID.String(), events.SlotWithdrawn{
		SlotID:   slot.ID,
		DoctorID: slot.DoctorID,
		StartAt:  slot.StartAt,
		EndAt:    slot.EndAt,
		Reason:   slotWithdrawalReason,
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// ListDoctorHolidays returns the leave that applies to one doctor: their own
// entries and the platform-wide ones, merged, because both stop them being
// booked and a screen that showed only one would be wrong about the other.
//
// PlatformWide() distinguishes them, and only the doctor's own can be deleted.
func (s *Service) ListDoctorHolidays(ctx context.Context, doctorID uuid.UUID, from, to Date) ([]Holiday, error) {
	if from.IsZero() {
		from = DateIn(s.clock.Now(), s.loc)
	}
	if to.IsZero() {
		to = from.AddDays(365)
	}
	if to.Before(from) {
		return nil, ErrHolidayRangeInvalid
	}
	return s.repo.ListDoctorHolidays(ctx, s.pool, doctorID, from, to)
}

// RemoveHoliday lifts leave and puts the day back on the market.
//
// Restoring is not simply "delete the row and wait for tonight's generation
// run". CopySlots merges ON CONFLICT (doctor_id, start_at) DO NOTHING, so the
// CANCELLED rows AddHoliday left behind would block re-generation of exactly
// the slots they occupy -- the day would stay empty until those rows aged out
// of the archive window. So the cancelled slots are revived in place, and a
// generation pass afterwards fills anything that was never materialised.
//
// Appointments cancelled by the leave are NOT restored. They were cancelled,
// the patients were told, refunds were issued, and some of those patients have
// booked elsewhere. Silently reinstating a consultation somebody was told was
// off is worse than making them book again.
func (s *Service) RemoveHoliday(ctx context.Context, doctorID, holidayID uuid.UUID) (HolidayEffect, error) {
	var effect HolidayEffect
	now := s.clock.Now()

	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		effect = HolidayEffect{}

		h, err := s.repo.LockDoctorHoliday(ctx, tx, doctorID, holidayID)
		if err != nil {
			return err
		}
		effect.Holiday = h

		if err := s.repo.SoftDeleteHoliday(ctx, tx, h.ID); err != nil {
			return err
		}

		// Only future slots come back. Reviving this morning's withdrawn slots
		// would put un-bookable rows on the market and make ListSlots' own
		// two-minute cutoff the only thing hiding them.
		from := h.Date.StartOfDay(s.loc).UTC()
		if from.Before(now) {
			from = now
		}
		to := h.Date.EndOfDay(s.loc).UTC()
		if !to.After(from) {
			return nil
		}

		slots, err := s.repo.LockSlotsInRange(ctx, tx, doctorID, from, to)
		if err != nil {
			return err
		}
		for i := range slots {
			if slots[i].Status != SlotCancelled {
				continue
			}
			rows, err := s.repo.ReleaseSlot(ctx, tx, slots[i].ID, slots[i].StartAt, slots[i].Version)
			if err != nil {
				return err
			}
			if rows != 1 {
				return ErrVersionConflict
			}
			effect.SlotsRestored++
			if err := s.outbox.Enqueue(ctx, tx, events.SubjectSlotReleased, slots[i].ID.String(),
				events.SlotReleased{
					SlotID:   slots[i].ID,
					DoctorID: slots[i].DoctorID,
					StartAt:  slots[i].StartAt,
					EndAt:    slots[i].EndAt,
					Reason:   holidayLiftedReason,
				}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return HolidayEffect{}, err
	}

	// Generation runs after the commit, in its own transaction. Holding the
	// day's slot locks while planning and COPYing the next thirty days would
	// serialise every booking for this doctor behind it, and the day is already
	// correct without it -- generation only adds slots that were never
	// materialised in the first place.
	if _, err := s.GenerateForDoctor(context.WithoutCancel(ctx), doctorID); err != nil &&
		!errors.Is(err, ErrDoctorNotConfigured) {
		s.log.Error().Err(err).Str("doctor_id", maskID(doctorID)).
			Msg("holiday lifted but slot regeneration failed; the nightly run will catch up")
	}

	s.log.Info().
		Str("doctor_id", maskID(doctorID)).
		Str("date", effect.Holiday.Date.String()).
		Int("slots_restored", effect.SlotsRestored).
		Msg("holiday lifted")
	return effect, nil
}

// CountLiveAppointmentsOn reports how many patients are booked with a doctor on
// one civil date. It backs the preflight the doctor app shows before asking
// "cancel these and refund them?", so the doctor sees the number before they
// commit rather than in the error that stops them.
func (s *Service) CountLiveAppointmentsOn(ctx context.Context, doctorID uuid.UUID, date Date) (int, error) {
	if date.IsZero() {
		return 0, ErrHolidayDateInvalid
	}
	from := date.StartOfDay(s.loc).UTC()
	to := date.EndOfDay(s.loc).UTC()
	n, err := s.repo.CountLiveAppointmentsInRange(ctx, s.pool, doctorID, from, to)
	if err != nil {
		return 0, fmt.Errorf("scheduling: count live appointments: %w", err)
	}
	return n, nil
}
