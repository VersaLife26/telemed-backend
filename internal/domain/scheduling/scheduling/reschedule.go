package scheduling

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
)

// CreateRescheduleInput is a doctor proposing a new instant for a confirmed
// booking they cannot attend.
type CreateRescheduleInput struct {
	AppointmentID   uuid.UUID
	ActorID         uuid.UUID
	ActorRole       string
	ProposedStartAt time.Time
	Reason          string
}

// DecideRescheduleInput is a patient or administrator accepting or declining
// a pending request. Expiry uses ActorSystem and OutcomeExpired.
type DecideRescheduleInput struct {
	RequestID uuid.UUID
	ActorID   uuid.UUID
	ActorRole string
	Outcome   RescheduleStatus // accepted | declined | expired
}

// CreateRescheduleRequest holds the proposed time and notifies admin + patient.
func (s *Service) CreateRescheduleRequest(ctx context.Context, in CreateRescheduleInput) (RescheduleRequest, error) {
	if in.ActorRole != ActorDoctor {
		return RescheduleRequest{}, ErrForbidden
	}
	now := s.clock.Now()
	proposedStart := in.ProposedStartAt.UTC()
	if !proposedStart.After(now) {
		return RescheduleRequest{}, ErrProposedTimeInPast
	}
	reason := strings.TrimSpace(in.Reason)
	if len(reason) > 500 {
		reason = reason[:500]
	}

	var out RescheduleRequest
	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		a, err := s.repo.LockAppointment(ctx, tx, in.AppointmentID)
		if err != nil {
			return err
		}
		if err := authorizeAppointment(a, in.ActorID, in.ActorRole); err != nil {
			return err
		}
		if a.Status != AppointmentConfirmed {
			return ErrAppointmentNotReschedulable
		}
		if !a.SlotStartAt.After(now) {
			return ErrAppointmentAlreadyStarted
		}
		if proposedStart.Equal(a.SlotStartAt) {
			return ErrProposedTimeUnchanged
		}

		_, err = s.repo.GetPendingRescheduleByAppointment(ctx, tx, a.ID)
		if err == nil {
			return ErrRescheduleAlreadyPending
		}
		if !errors.Is(err, ErrRescheduleNotFound) {
			return err
		}

		duration := a.SlotEndAt.Sub(a.SlotStartAt)
		if duration <= 0 {
			return ErrAppointmentNotReschedulable
		}
		proposedEnd := proposedStart.Add(duration)

		patientOverlap, err := s.repo.PatientHasOverlappingAppointmentExcluding(ctx, tx, a.PatientID, a.ID, proposedStart, proposedEnd)
		if err != nil {
			return err
		}
		if patientOverlap {
			return ErrDuplicateBooking
		}
		doctorOverlap, err := s.repo.DoctorHasOverlappingAppointmentExcluding(ctx, tx, a.DoctorID, a.ID, proposedStart, proposedEnd)
		if err != nil {
			return err
		}
		if doctorOverlap {
			return ErrDuplicateBooking
		}

		if _, err := s.repo.EnsurePartitions(ctx, tx, proposedStart, 2); err != nil {
			return err
		}

		slot, created, err := s.holdProposedSlot(ctx, tx, a.DoctorID, a.PatientID, proposedStart, proposedEnd)
		if err != nil {
			return err
		}
		// An already-generated AVAILABLE slot may already be in the doctor
		// search projection. SlotBooked takes it off the market for the hold;
		// decline emits SlotReleased to put it back.
		if !created {
			if err := s.outbox.Enqueue(ctx, tx, events.SubjectSlotBooked, slot.ID.String(), events.SlotBooked{
				SlotID:        slot.ID,
				DoctorID:      slot.DoctorID,
				AppointmentID: a.ID,
				StartAt:       slot.StartAt,
				EndAt:         slot.EndAt,
			}); err != nil {
				return err
			}
		}

		req := RescheduleRequest{
			ID:                  uuid.New(),
			AppointmentID:       a.ID,
			PatientID:           a.PatientID,
			DoctorID:            a.DoctorID,
			OriginalSlotID:      a.SlotID,
			OriginalStartAt:     a.SlotStartAt,
			OriginalEndAt:       a.SlotEndAt,
			ProposedSlotID:      slot.ID,
			ProposedStartAt:     slot.StartAt,
			ProposedEndAt:       slot.EndAt,
			ProposedSlotCreated: created,
			Reason:              reason,
			Status:              ReschedulePending,
		}
		if err := s.repo.InsertRescheduleRequest(ctx, tx, &req); err != nil {
			if IsPendingRescheduleConflict(err) {
				return ErrRescheduleAlreadyPending
			}
			return err
		}
		out = req

		return s.outbox.Enqueue(ctx, tx, events.SubjectAppointmentRescheduleRequested, req.ID.String(),
			events.AppointmentRescheduleRequested{
				RequestID:      req.ID,
				AppointmentID:  req.AppointmentID,
				PatientID:      req.PatientID,
				DoctorID:       req.DoctorID,
				OriginalSlotID: req.OriginalSlotID,
				OriginalStart:  req.OriginalStartAt,
				OriginalEnd:    req.OriginalEndAt,
				ProposedSlotID: req.ProposedSlotID,
				ProposedStart:  req.ProposedStartAt,
				ProposedEnd:    req.ProposedEndAt,
				RequestedAt:    now,
			})
	})
	if err != nil {
		return RescheduleRequest{}, err
	}
	s.log.Info().
		Str("request_id", maskID(out.ID)).
		Str("appointment_id", maskID(out.AppointmentID)).
		Msg("reschedule requested")
	return out, nil
}

func (s *Service) holdProposedSlot(ctx context.Context, tx pgx.Tx, doctorID, patientID uuid.UUID, startAt, endAt time.Time) (Slot, bool, error) {
	slot, err := s.repo.LockSlotByDoctorStart(ctx, tx, doctorID, startAt)
	switch {
	case errors.Is(err, ErrSlotNotFound):
		inserted := Slot{
			ID:          uuid.New(),
			DoctorID:    doctorID,
			StartAt:     startAt,
			EndAt:       endAt,
			Status:      SlotBlocked,
			ReservedFor: &patientID,
		}
		out, insErr := s.repo.InsertAdHocBlockedSlot(ctx, tx, inserted)
		if insErr == nil {
			return out, true, nil
		}
		if !database.IsUniqueViolation(insErr) {
			return Slot{}, false, insErr
		}
		slot, err = s.repo.LockSlotByDoctorStart(ctx, tx, doctorID, startAt)
		if err != nil {
			return Slot{}, false, err
		}
	case err != nil:
		return Slot{}, false, err
	}

	if slot.Status != SlotAvailable {
		return Slot{}, false, ErrSlotUnavailable
	}
	n, err := s.repo.HoldSlotForReschedule(ctx, tx, slot.ID, slot.StartAt, patientID, slot.Version)
	if err != nil {
		return Slot{}, false, err
	}
	if n != 1 {
		return Slot{}, false, ErrVersionConflict
	}
	slot.Status = SlotBlocked
	slot.ReservedFor = &patientID
	slot.ReservedUntil = nil
	slot.Version++
	return slot, false, nil
}

// ListRescheduleRequestsForAppointment returns requests the caller may see.
func (s *Service) ListRescheduleRequestsForAppointment(ctx context.Context, appointmentID, actorID uuid.UUID, role string) ([]RescheduleRequest, error) {
	if _, err := s.GetAppointment(ctx, appointmentID, actorID, role); err != nil {
		return nil, err
	}
	return s.repo.ListRescheduleRequestsForAppointment(ctx, s.pool, appointmentID)
}

// ListPendingRescheduleRequests is the admin queue. Admin-only: the handler
// already sits behind the admin route group.
func (s *Service) ListPendingRescheduleRequests(ctx context.Context, limit, offset int) ([]RescheduleRequest, int64, error) {
	return s.repo.ListPendingRescheduleRequests(ctx, s.pool, limit, offset)
}

// AcceptRescheduleRequest moves the same paid appointment onto the held slot.
func (s *Service) AcceptRescheduleRequest(ctx context.Context, in DecideRescheduleInput) (RescheduleRequest, Appointment, error) {
	in.Outcome = RescheduleAccepted
	return s.decideReschedule(ctx, in)
}

// DeclineRescheduleRequest drops the hold and cancels the original with a full
// refund — the doctor asked to move, the patient (or admin) said no.
func (s *Service) DeclineRescheduleRequest(ctx context.Context, in DecideRescheduleInput) (RescheduleRequest, Appointment, error) {
	if in.Outcome != RescheduleExpired {
		in.Outcome = RescheduleDeclined
	}
	return s.decideReschedule(ctx, in)
}

func (s *Service) decideReschedule(ctx context.Context, in DecideRescheduleInput) (RescheduleRequest, Appointment, error) {
	switch in.ActorRole {
	case ActorPatient, ActorAdmin, ActorSystem:
	default:
		return RescheduleRequest{}, Appointment{}, ErrForbidden
	}
	if in.Outcome != RescheduleAccepted && in.Outcome != RescheduleDeclined && in.Outcome != RescheduleExpired {
		return RescheduleRequest{}, Appointment{}, ErrForbidden
	}

	now := s.clock.Now()
	var (
		outReq    RescheduleRequest
		outAppt   Appointment
		released  bool
		promoteID uuid.UUID
		doctorID  uuid.UUID
	)

	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		peek, err := s.repo.GetRescheduleRequest(ctx, tx, in.RequestID)
		if err != nil {
			return err
		}

		a, err := s.repo.LockAppointment(ctx, tx, peek.AppointmentID)
		if err != nil {
			return err
		}
		req, err := s.repo.LockRescheduleRequest(ctx, tx, in.RequestID)
		if err != nil {
			return err
		}
		if req.AppointmentID != a.ID {
			return ErrRescheduleNotFound
		}
		if err := authorizeRescheduleDecision(req, in.ActorID, in.ActorRole); err != nil {
			return err
		}
		if req.Status != ReschedulePending {
			return ErrRescheduleNotPending
		}

		// Lock both slots in start_at order so this path cannot deadlock
		// against CancelAppointment (appointment, then original slot).
		firstID, secondID := req.OriginalSlotID, req.ProposedSlotID
		if req.ProposedStartAt.Before(req.OriginalStartAt) {
			firstID, secondID = req.ProposedSlotID, req.OriginalSlotID
		}
		if _, err := s.repo.LockSlot(ctx, tx, firstID); err != nil && !errors.Is(err, ErrSlotNotFound) {
			return err
		}
		if firstID != secondID {
			if _, err := s.repo.LockSlot(ctx, tx, secondID); err != nil && !errors.Is(err, ErrSlotNotFound) {
				return err
			}
		}

		actor := in.ActorID
		var actorPtr *uuid.UUID
		if in.ActorRole != ActorSystem {
			actorPtr = &actor
		}
		n, err := s.repo.DecideRescheduleRequest(ctx, tx, req.ID, req.Version, in.Outcome, actorPtr, in.ActorRole, now)
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrVersionConflict
		}
		req.Status = in.Outcome
		req.DecidedBy = actorPtr
		req.DecidedByRole = in.ActorRole
		req.DecidedAt = &now
		req.Version++
		req.UpdatedAt = now

		if in.Outcome == RescheduleAccepted {
			if a.Status != AppointmentConfirmed {
				return ErrAppointmentNotReschedulable
			}
			if err := s.acceptMove(ctx, tx, &a, req, now); err != nil {
				return err
			}
			released = true
			promoteID = req.OriginalSlotID
			doctorID = a.DoctorID
			outAppt = a
			outReq = req
			return nil
		}

		if err := s.dropHeldProposedSlot(ctx, tx, req); err != nil {
			return err
		}
		if a.Status == AppointmentPendingPayment || a.Status == AppointmentConfirmed {
			cancelled, origReleased, err := s.cancelForRescheduleDecline(ctx, tx, a, in.ActorRole, actorPtr, req.Reason, now)
			if err != nil {
				return err
			}
			outAppt = cancelled
			if origReleased {
				released = true
				promoteID = cancelled.SlotID
				doctorID = cancelled.DoctorID
			}
		} else {
			outAppt = a
		}
		outReq = req
		return nil
	})
	if err != nil {
		return RescheduleRequest{}, Appointment{}, err
	}

	if released {
		if err := s.PromoteWaitlist(context.WithoutCancel(ctx), doctorID, promoteID); err != nil {
			s.log.Error().Err(err).Str("slot_id", maskID(promoteID)).Msg("waitlist promotion after reschedule failed")
		}
	}
	s.log.Info().
		Str("request_id", maskID(outReq.ID)).
		Str("outcome", string(outReq.Status)).
		Str("actor_role", in.ActorRole).
		Msg("reschedule decided")
	return outReq, outAppt, nil
}

func authorizeRescheduleDecision(req RescheduleRequest, actorID uuid.UUID, role string) error {
	switch role {
	case ActorAdmin, ActorSystem:
		if role == ActorAdmin && actorID == uuid.Nil {
			return ErrForbidden
		}
		return nil
	case ActorPatient:
		if actorID == uuid.Nil || req.PatientID != actorID {
			return ErrRescheduleNotFound
		}
		return nil
	default:
		return ErrForbidden
	}
}

func (s *Service) acceptMove(ctx context.Context, tx pgx.Tx, a *Appointment, req RescheduleRequest, now time.Time) error {
	proposed, err := s.repo.LockSlot(ctx, tx, req.ProposedSlotID)
	if err != nil {
		return err
	}
	n, err := s.repo.MarkSlotBooked(ctx, tx, proposed.ID, proposed.StartAt, a.ID, proposed.Version)
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrVersionConflict
	}
	if err := s.outbox.Enqueue(ctx, tx, events.SubjectSlotBooked, proposed.ID.String(), events.SlotBooked{
		SlotID:        proposed.ID,
		DoctorID:      a.DoctorID,
		AppointmentID: a.ID,
		StartAt:       proposed.StartAt,
		EndAt:         proposed.EndAt,
	}); err != nil {
		return err
	}

	moved, err := s.repo.MoveAppointment(ctx, tx, a.ID, a.Version, proposed.ID, req.ProposedStartAt, req.ProposedEndAt)
	if err != nil {
		return err
	}
	if moved != 1 {
		return ErrVersionConflict
	}

	original, err := s.repo.LockSlot(ctx, tx, req.OriginalSlotID)
	if err != nil && !errors.Is(err, ErrSlotNotFound) {
		return err
	}
	if err == nil && original.Status == SlotBooked && original.AppointmentID != nil && *original.AppointmentID == a.ID {
		n, relErr := s.repo.ReleaseSlot(ctx, tx, original.ID, original.StartAt, original.Version)
		if relErr != nil {
			return relErr
		}
		if n != 1 {
			return ErrVersionConflict
		}
		if err := s.outbox.Enqueue(ctx, tx, events.SubjectSlotReleased, original.ID.String(), events.SlotReleased{
			SlotID:   original.ID,
			DoctorID: a.DoctorID,
			StartAt:  original.StartAt,
			EndAt:    original.EndAt,
			Reason:   eventReasonCode(rescheduledSlotReason),
		}); err != nil {
			return err
		}
	}

	a.SlotID = proposed.ID
	a.SlotStartAt = req.ProposedStartAt
	a.SlotEndAt = req.ProposedEndAt
	a.Version++
	a.UpdatedAt = now

	return s.outbox.Enqueue(ctx, tx, events.SubjectAppointmentRescheduled, a.ID.String(),
		events.AppointmentRescheduled{
			RequestID:      req.ID,
			AppointmentID:  a.ID,
			PatientID:      a.PatientID,
			DoctorID:       a.DoctorID,
			OriginalSlotID: req.OriginalSlotID,
			OriginalStart:  req.OriginalStartAt,
			ProposedSlotID: proposed.ID,
			ProposedStart:  req.ProposedStartAt,
			ProposedEnd:    req.ProposedEndAt,
			DecidedByRole:  req.DecidedByRole,
			RescheduledAt:  now,
		})
}

func (s *Service) dropHeldProposedSlot(ctx context.Context, tx pgx.Tx, req RescheduleRequest) error {
	slot, err := s.repo.LockSlot(ctx, tx, req.ProposedSlotID)
	if errors.Is(err, ErrSlotNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if slot.Status != SlotBlocked {
		return nil
	}
	if req.ProposedSlotCreated {
		n, err := s.repo.SetSlotStatus(ctx, tx, slot.ID, slot.StartAt, SlotCancelled, slot.Version)
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrVersionConflict
		}
		return nil
	}
	n, err := s.repo.ReleaseSlot(ctx, tx, slot.ID, slot.StartAt, slot.Version)
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrVersionConflict
	}
	return s.outbox.Enqueue(ctx, tx, events.SubjectSlotReleased, slot.ID.String(), events.SlotReleased{
		SlotID:   slot.ID,
		DoctorID: slot.DoctorID,
		StartAt:  slot.StartAt,
		EndAt:    slot.EndAt,
		Reason:   eventReasonCode(rescheduleDeclinedReasonCode),
	})
}

func (s *Service) cancelForRescheduleDecline(ctx context.Context, tx pgx.Tx, a Appointment, role string, actor *uuid.UUID, doctorReason string, now time.Time) (Appointment, bool, error) {
	stored := rescheduleDeclinedReasonCode
	if doctorReason != "" {
		stored = rescheduleDeclinedReasonCode + ": " + doctorReason
	}
	policy := RefundFull
	cancelledByRole := role
	if role == ActorSystem {
		cancelledByRole = ActorSystem
	}

	n, err := s.repo.CancelAppointment(ctx, tx, a.ID, a.Version, actor, cancelledByRole, stored, policy, now)
	if err != nil {
		return Appointment{}, false, err
	}
	if n != 1 {
		return Appointment{}, false, ErrVersionConflict
	}
	if err := s.repo.BumpNoShowCounter(ctx, tx, a.PatientID, "cancelled", now); err != nil {
		return Appointment{}, false, err
	}

	released := false
	slot, err := s.repo.LockSlot(ctx, tx, a.SlotID)
	switch {
	case errors.Is(err, ErrSlotNotFound):
	case err != nil:
		return Appointment{}, false, err
	default:
		if slot.Status == SlotBooked && slot.AppointmentID != nil && *slot.AppointmentID == a.ID {
			n, relErr := s.repo.ReleaseSlot(ctx, tx, slot.ID, slot.StartAt, slot.Version)
			if relErr != nil {
				return Appointment{}, false, relErr
			}
			if n != 1 {
				return Appointment{}, false, ErrVersionConflict
			}
			released = true
		}
	}

	a.Status = AppointmentCancelled
	a.CancelledAt = &now
	a.CancelledBy = actor
	a.CancelledByRole = cancelledByRole
	a.CancellationReason = stored
	a.RefundPolicy = &policy
	a.Version++
	a.UpdatedAt = now

	if err := s.outbox.Enqueue(ctx, tx, events.SubjectAppointmentCancelled, a.ID.String(),
		events.AppointmentCancelled{
			AppointmentID: a.ID,
			PatientID:     a.PatientID,
			DoctorID:      a.DoctorID,
			SlotID:        a.SlotID,
			StartAt:       a.SlotStartAt,
			CancelledBy:   cancelledByRole,
			Reason:        eventReasonCode(rescheduleDeclinedReasonCode),
			NoShow:        false,
			RefundPolicy:  string(policy),
			RefundPercent: policy.Percent(),
			CancelledAt:   now,
		}); err != nil {
		return Appointment{}, false, err
	}
	if released {
		if err := s.outbox.Enqueue(ctx, tx, events.SubjectSlotReleased, a.SlotID.String(), events.SlotReleased{
			SlotID:   a.SlotID,
			DoctorID: a.DoctorID,
			StartAt:  a.SlotStartAt,
			EndAt:    a.SlotEndAt,
			Reason:   eventReasonCode(rescheduleDeclinedReasonCode),
		}); err != nil {
			return Appointment{}, false, err
		}
	}
	return a, released, nil
}

// SweepExpiredRescheduleRequests declines any pending request whose original
// start has passed: the doctor never showed, so the patient is refunded in full.
func (s *Service) SweepExpiredRescheduleRequests(ctx context.Context) (int, error) {
	now := s.clock.Now()
	const batch = 100
	stale, err := s.repo.ListExpiredPendingRescheduleRequests(ctx, s.pool, now, batch)
	if err != nil {
		return 0, err
	}
	swept := 0
	for i := range stale {
		_, _, err := s.DeclineRescheduleRequest(ctx, DecideRescheduleInput{
			RequestID: stale[i].ID,
			ActorID:   uuid.Nil,
			ActorRole: ActorSystem,
			Outcome:   RescheduleExpired,
		})
		if err != nil {
			s.log.Error().Err(err).Str("request_id", maskID(stale[i].ID)).Msg("reschedule expiry failed")
			continue
		}
		swept++
	}
	if swept > 0 {
		s.log.Info().Int("expired", swept).Msg("pending reschedule requests expired")
	}
	return swept, nil
}
