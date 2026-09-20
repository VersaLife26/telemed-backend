package scheduling

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
)

// SlotPlan is one intended slot, already resolved to UTC instants.
type SlotPlan struct {
	StartAt time.Time
	EndAt   time.Time
}

// PlanDay lays out one doctor's slots for one civil date.
//
// Timezone handling is the whole point of this function, so it is worth being
// explicit. Clinic hours are wall-clock facts: "I see patients from 09:00".
// They are stored as TIME, with no zone, precisely because 09:00 is not an
// instant. This function is the single place that turns a wall-clock fact into
// an instant, by constructing it in the doctor's own location and converting to
// UTC.
//
// Sri Lanka has not observed DST since 1996 and Asia/Colombo is a flat +05:30,
// so the conversion looks trivially like "subtract 5h30m" -- and hardcoding
// that offset is exactly the bug this function exists to prevent. The platform
// will take doctors in other zones, and tzdata is the only thing that will
// still be right in 2050. TestSlotGeneration_TimezoneOffset asserts the
// difference, and asserts that the naive UTC construction would have been wrong.
//
// Every slot boundary is built with time.Date from wall-clock minutes rather
// than by adding a Duration to the previous one. Adding durations is subtly
// wrong in a zone that does observe DST: a clinic window straddling a
// spring-forward would have every slot after the transition drift an hour off
// the wall clock the doctor actually keeps.
//
// The layout follows the documentation's worked example: with a 15 minute
// consultation and a 5 minute buffer, 09:00-12:00 yields 09:00-09:15,
// 09:20-09:35, 09:40-09:55, ... Slots advance by duration+buffer and a slot is
// only emitted if it ends within the working window.
func PlanDay(settings ScheduleSettings, hours []WorkingHour, date Date, loc *time.Location) []SlotPlan {
	stepMinutes := settings.SlotDurationMinutes + settings.BufferMinutes
	durationMinutes := settings.SlotDurationMinutes
	if stepMinutes <= 0 || durationMinutes <= 0 {
		return nil
	}

	weekday := date.Weekday()

	// at builds the instant for a wall-clock offset from local midnight. Hours
	// beyond 23 normalise into the next day, which is what a clinic running to
	// 24:00 means.
	at := func(minutes int) time.Time {
		return time.Date(date.Year, date.Month, date.Day, minutes/60, minutes%60, 0, 0, loc)
	}

	var plans []SlotPlan
	for _, h := range hours {
		if h.DayOfWeek != weekday || !h.IsAvailable {
			continue
		}
		for m := h.StartMinute; m+durationMinutes <= h.EndMinute; m += stepMinutes {
			start, end := at(m), at(m+durationMinutes)
			// A DST spring-forward can collapse a wall-clock interval to zero
			// length. Postgres would reject it (end_at > start_at); drop it here
			// so one badly-timed clinic hour cannot fail a whole generation run.
			if !end.After(start) {
				continue
			}
			plans = append(plans, SlotPlan{StartAt: start.UTC(), EndAt: end.UTC()})
		}
	}

	sort.Slice(plans, func(i, j int) bool { return plans[i].StartAt.Before(plans[j].StartAt) })

	// Two overlapping windows on the same day -- or a DST transition mapping two
	// wall-clock starts onto one instant -- would otherwise produce duplicate
	// starts and a unique-violation storm on (doctor_id, start_at).
	plans = dedupePlans(plans)

	if limit := settings.EffectiveMaxPerDay(); limit > 0 && len(plans) > limit {
		plans = plans[:limit]
	}
	return plans
}

func dedupePlans(plans []SlotPlan) []SlotPlan {
	if len(plans) < 2 {
		return plans
	}
	out := plans[:1]
	for _, p := range plans[1:] {
		if p.StartAt.Equal(out[len(out)-1].StartAt) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// PlanRange lays out days days starting at from, skipping holidays and any slot
// that has already started.
func PlanRange(settings ScheduleSettings, hours []WorkingHour, holidays map[Date]struct{},
	from Date, days int, loc *time.Location, now time.Time,
) []SlotPlan {
	var out []SlotPlan
	for i := 0; i < days; i++ {
		date := from.AddDays(i)
		if _, isHoliday := holidays[date]; isHoliday {
			continue
		}
		for _, p := range PlanDay(settings, hours, date, loc) {
			// Never materialise a slot nobody can book. Re-runs of the daily
			// job would otherwise keep resurrecting this morning's slots.
			if !p.StartAt.After(now) {
				continue
			}
			out = append(out, p)
		}
	}
	return out
}

// GenerationResult reports what one doctor's generation run did.
type GenerationResult struct {
	DoctorID uuid.UUID
	Planned  int
	Inserted int64
	From     Date
	To       Date
}

// schedulePlan is one doctor's intended calendar for the next advance_days:
// the settings, the resolved timezone, the window and the slots the pattern
// asks for.
//
// It exists because generation and reconciliation need exactly the same four
// reads, and an availability edit runs both. Computing it once and handing it
// to each is what keeps one edit from issuing eight queries for one answer.
type schedulePlan struct {
	doctorID uuid.UUID
	settings ScheduleSettings
	loc      *time.Location
	from, to Date
	now      time.Time
	plans    []SlotPlan
}

// planForDoctor resolves what the doctor's calendar should look like. It does
// not touch a single slot row.
func (s *Service) planForDoctor(ctx context.Context, doctorID uuid.UUID) (schedulePlan, error) {
	now := s.clock.Now()

	settings, err := s.repo.GetScheduleSettings(ctx, s.pool, doctorID)
	if err != nil {
		return schedulePlan{}, err
	}
	plan := schedulePlan{doctorID: doctorID, settings: settings, loc: s.loc, now: now}
	if !settings.IsActive {
		return plan, nil
	}

	loc, err := time.LoadLocation(settings.Timezone)
	if err != nil {
		// A doctor row with a corrupt timezone must not take the whole nightly
		// run down; fall back to the platform default and say so loudly.
		s.log.Error().Err(err).
			Str("doctor_id", maskID(doctorID)).
			Str("timezone", settings.Timezone).
			Msg("invalid doctor timezone, falling back to platform default")
		loc = s.loc
	}
	plan.loc = loc

	hours, err := s.repo.ListWorkingHours(ctx, s.pool, doctorID)
	if err != nil {
		return schedulePlan{}, err
	}
	if len(hours) == 0 {
		return schedulePlan{}, ErrDoctorNotConfigured
	}

	plan.from = DateIn(now, loc)
	plan.to = plan.from.AddDays(settings.AdvanceDays - 1)

	holidays, err := s.repo.ListHolidays(ctx, s.pool, doctorID, plan.from, plan.to)
	if err != nil {
		return schedulePlan{}, err
	}

	plan.plans = PlanRange(settings, hours, holidays, plan.from, settings.AdvanceDays, loc, now)
	return plan, nil
}

// GenerateForDoctor materialises the next advance_days of slots for one doctor.
//
// It is idempotent by construction: the plan is deterministic for a given day
// and the merge is ON CONFLICT DO NOTHING, so running it five times in a row
// inserts rows exactly once. That property is what lets the cron job be retried
// blindly after a failure, and it is asserted by
// TestSlotGeneration_IdempotentRerun.
func (s *Service) GenerateForDoctor(ctx context.Context, doctorID uuid.UUID) (GenerationResult, error) {
	plan, err := s.planForDoctor(ctx, doctorID)
	if err != nil {
		return GenerationResult{}, err
	}
	return s.materialise(ctx, plan)
}

// materialise inserts the slots a plan asks for and announces the ones that
// were actually new.
func (s *Service) materialise(ctx context.Context, plan schedulePlan) (GenerationResult, error) {
	doctorID := plan.doctorID
	if !plan.settings.IsActive {
		return GenerationResult{DoctorID: doctorID}, nil
	}

	result := GenerationResult{DoctorID: doctorID, Planned: len(plan.plans), From: plan.from, To: plan.to}
	if len(plan.plans) == 0 {
		return result, nil
	}

	slots := make([]Slot, len(plan.plans))
	for i, p := range plan.plans {
		slots[i] = Slot{
			ID:       uuid.New(),
			DoctorID: doctorID,
			StartAt:  p.StartAt,
			EndAt:    p.EndAt,
			Status:   SlotAvailable,
		}
	}

	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		inserted, err := s.repo.CopySlots(ctx, tx, slots)
		if err != nil {
			return err
		}
		result.Inserted = inserted
		if inserted == 0 {
			// Nothing changed, so nothing happened worth announcing.
			return nil
		}
		return s.outbox.Enqueue(ctx, tx, events.SubjectSlotsGenerated, doctorID.String(), events.SlotsGenerated{
			DoctorID: doctorID,
			// YYYY-MM-DD civil dates in the business timezone, not instants:
			// "the 21st of August" is a day in Colombo, and rendering it as a
			// UTC timestamp makes it the 20th for eighteen and a half hours.
			FromDate: plan.from.String(),
			ToDate:   plan.to.String(),
			Count:    int(inserted),
		})
	})
	if err != nil {
		return GenerationResult{}, err
	}

	s.log.Info().
		Str("doctor_id", maskID(doctorID)).
		Int("planned", result.Planned).
		Int64("inserted", result.Inserted).
		Str("from", plan.from.String()).
		Str("to", plan.to.String()).
		Msg("slots generated")
	return result, nil
}

// ScheduleSyncResult reports what one availability edit did to the calendar
// that already existed, on top of what it generated.
type ScheduleSyncResult struct {
	GenerationResult
	// Withdrawn is how many unbooked slots the new pattern no longer covers.
	Withdrawn int
	// Restored is how many previously withdrawn slots the new pattern covers
	// again.
	Restored int
}

// scheduleChangeReason is carried on the slot events a pattern edit produces,
// so an operator reading a replayed stream can tell an edit from leave.
const scheduleChangeReason = "schedule_changed"

// SyncScheduleChange brings a doctor's materialised slots back in line with the
// weekly pattern they just published.
//
// THE PROBLEM THIS SOLVES
// Slots are materialised up to advance_days ahead, and until this existed the
// ONLY things that created them were doctor.approved and the 00:00 cron. So a
// doctor who added Saturday mornings saw nothing happen: the pattern was
// mirrored correctly, no slot row was written, and patients kept seeing an
// empty Saturday until the next nightly run -- by which time the weekend they
// opened up was often already here. The availability editor looked broken while
// every service involved was behaving exactly as written.
//
// WHAT HAPPENS TO EACH KIND OF SLOT
//   - Planned and missing: inserted, same as any generation run.
//   - Planned but previously CANCELLED (the doctor removed the hour and has now
//     put it back): released in place. It has to be in place, because CopySlots
//     merges ON CONFLICT DO NOTHING, so the cancelled row would otherwise block
//     re-generation of exactly the slot it occupies -- the same trap
//     RemoveHoliday documents.
//   - No longer planned and AVAILABLE: withdrawn. Leaving them would let
//     patients book an hour the doctor has just told us they do not work.
//   - No longer planned and BOOKED, BLOCKED or reserved: LEFT ALONE. A booked
//     slot is a patient with an appointment, and cancelling it is a decision
//     with a refund and an SMS attached -- the doctor makes it explicitly
//     through leave or cancellation, never as a side effect of editing a time
//     window. A BLOCKED slot is an admin override or a live waitlist offer, and
//     neither belongs to this function.
//
// Withdrawal and restoration run in one transaction holding row locks in
// start_at order -- the platform's slot lock order -- so a booking racing an
// edit either commits first and is then seen as BOOKED and left alone, or
// blocks and afterwards observes a CANCELLED slot and fails with
// ErrSlotUnavailable. Generation runs after that commit, for the reason
// RemoveHoliday gives: holding a month of slot locks while planning and
// COPYing would serialise every booking for this doctor behind it.
func (s *Service) SyncScheduleChange(ctx context.Context, doctorID uuid.UUID) (ScheduleSyncResult, error) {
	plan, err := s.planForDoctor(ctx, doctorID)
	if err != nil {
		return ScheduleSyncResult{}, err
	}
	if !plan.settings.IsActive {
		return ScheduleSyncResult{GenerationResult: GenerationResult{DoctorID: doctorID}}, nil
	}

	// Keyed on the instant, not on time.Time: two time.Time values for the same
	// instant compare unequal as map keys when their locations differ, and the
	// plan is built in the doctor's zone while the rows come back from pgx in
	// whatever zone the connection reports.
	wanted := make(map[int64]struct{}, len(plan.plans))
	for _, p := range plan.plans {
		wanted[p.StartAt.Unix()] = struct{}{}
	}

	var out ScheduleSyncResult
	windowEnd := plan.to.EndOfDay(plan.loc).UTC()
	if windowEnd.After(plan.now) {
		err = database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			out.Withdrawn, out.Restored = 0, 0

			// From now, never from the start of the day: an hour that has
			// already begun cannot be withdrawn from anybody, and this morning's
			// elapsed slots are not in the plan either.
			slots, err := s.repo.LockSlotsInRange(ctx, tx, doctorID, plan.now, windowEnd)
			if err != nil {
				return err
			}

			for i := range slots {
				_, planned := wanted[slots[i].StartAt.Unix()]
				switch {
				case planned && slots[i].Status == SlotCancelled:
					if err := s.restoreSlot(ctx, tx, slots[i]); err != nil {
						return err
					}
					out.Restored++
				case !planned && slots[i].Status == SlotAvailable &&
					(slots[i].ReservedUntil == nil || !slots[i].ReservedUntil.After(plan.now)):
					withdrawn, err := s.withdrawSlot(ctx, tx, slots[i], scheduleChangeReason)
					if err != nil {
						return err
					}
					if withdrawn {
						out.Withdrawn++
					}
				}
			}
			return nil
		})
		if err != nil {
			return ScheduleSyncResult{}, err
		}
	}

	gen, err := s.materialise(ctx, plan)
	if err != nil {
		return ScheduleSyncResult{}, err
	}
	out.GenerationResult = gen

	if out.Withdrawn > 0 || out.Restored > 0 {
		s.log.Info().
			Str("doctor_id", maskID(doctorID)).
			Int("withdrawn", out.Withdrawn).
			Int("restored", out.Restored).
			Msg("slots reconciled to the doctor's new schedule")
	}
	return out, nil
}

// restoreSlot puts a slot the doctor had removed from their week back on the
// market, in place. Announced as slot.released, which is what the availability
// projection and the waitlist both read as "bookable again".
func (s *Service) restoreSlot(ctx context.Context, tx pgx.Tx, slot Slot) error {
	rows, err := s.repo.ReleaseSlot(ctx, tx, slot.ID, slot.StartAt, slot.Version)
	if err != nil {
		return err
	}
	if rows != 1 {
		// The row moved under a lock we hold, which should be impossible.
		return ErrVersionConflict
	}
	return s.outbox.Enqueue(ctx, tx, events.SubjectSlotReleased, slot.ID.String(), events.SlotReleased{
		SlotID:   slot.ID,
		DoctorID: slot.DoctorID,
		StartAt:  slot.StartAt,
		EndAt:    slot.EndAt,
		Reason:   scheduleChangeReason,
	})
}

// GenerateAll runs generation for every active doctor. One doctor's failure is
// logged and skipped rather than aborting the run: on a platform with 500
// doctors, one bad row must not cost the other 499 their next 30 days.
func (s *Service) GenerateAll(ctx context.Context) (doctors int, inserted int64, err error) {
	ids, err := s.repo.ListActiveDoctors(ctx, s.pool)
	if err != nil {
		return 0, 0, err
	}
	for _, id := range ids {
		select {
		case <-ctx.Done():
			return doctors, inserted, ctx.Err()
		default:
		}
		res, genErr := s.GenerateForDoctor(ctx, id)
		if genErr != nil {
			s.log.Error().Err(genErr).Str("doctor_id", maskID(id)).Msg("slot generation failed for doctor")
			continue
		}
		doctors++
		inserted += res.Inserted
	}
	return doctors, inserted, nil
}

// ArchiveOldSlots moves slots older than SlotArchiveAge into slots_archive, in
// bounded batches.
func (s *Service) ArchiveOldSlots(ctx context.Context) (int64, error) {
	cutoff := s.clock.Now().Add(-SlotArchiveAge)
	const batch = 5000
	var total int64
	for {
		var moved int64
		err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			n, err := s.repo.ArchiveSlotsBefore(ctx, tx, cutoff, batch)
			moved = n
			return err
		})
		if err != nil {
			return total, err
		}
		total += moved
		if moved < batch {
			break
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
	}
	if total > 0 {
		s.log.Info().Int64("archived", total).Time("cutoff", cutoff).Msg("old slots archived")
	}
	return total, nil
}

// EnsurePartitionRunway keeps PartitionRunwayMonths of monthly partitions ahead
// of today. Cheap and idempotent, so it runs daily rather than monthly: the cost
// of running it too often is a no-op query, and the cost of running it too
// rarely is every insert landing in slots_default.
func (s *Service) EnsurePartitionRunway(ctx context.Context) ([]string, error) {
	names, err := s.repo.EnsurePartitions(ctx, s.pool, s.clock.Now(), PartitionRunwayMonths)
	if err != nil {
		return nil, fmt.Errorf("scheduling: ensure partition runway: %w", err)
	}
	return names, nil
}
