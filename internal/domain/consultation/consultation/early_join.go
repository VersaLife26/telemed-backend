package consultation

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/middleware"
)

const (
	// ReadyForNextOffered is a fresh one-shot ping to the next patient.
	ReadyForNextOffered = "offered"
	// ReadyForNextAlreadyOffered is a second tap while the patient has not
	// answered; no extra notification is sent.
	ReadyForNextAlreadyOffered = "already_offered"
	// ReadyForNextAlreadyWaiting means the next patient is already in the
	// waiting room, so a courtesy ping would only spam them.
	ReadyForNextAlreadyWaiting = "already_waiting"
	// ReadyForNextDeclined means that patient already asked to keep the
	// booked time. We do not offer again, and we do not rewrite later slots.
	ReadyForNextDeclined = "declined"

	EarlyJoinAccepted = "accepted"
	EarlyJoinDeclined = "declined"
)

// ReadyForNextResult is what the doctor sees after tapping "ready for next".
// Status tells the UI whether a ping went out. ScheduledAt is the next
// patient's original booked time and is never rewritten by this flow.
type ReadyForNextResult struct {
	Status         string     `json:"status"`
	AppointmentID  uuid.UUID  `json:"appointment_id"`
	ConsultationID uuid.UUID  `json:"consultation_id"`
	ScheduledAt    time.Time  `json:"scheduled_at"`
	OfferedAt      *time.Time `json:"offered_at,omitempty"`
	Response       *string    `json:"response,omitempty"`
}

// EarlyJoinOffer is the next patient's view of a pending or answered
// "join now / keep my booked time" courtesy.
type EarlyJoinOffer struct {
	AppointmentID  uuid.UUID  `json:"appointment_id"`
	ConsultationID uuid.UUID  `json:"consultation_id"`
	ScheduledAt    time.Time  `json:"scheduled_at"`
	OfferedAt      time.Time  `json:"offered_at"`
	Response       *string    `json:"response,omitempty"`
	RespondedAt    *time.Time `json:"responded_at,omitempty"`
}

func offerFrom(c *Consultation) ReadyForNextResult {
	return ReadyForNextResult{
		AppointmentID:  c.AppointmentID,
		ConsultationID: c.ID,
		ScheduledAt:    c.ScheduledAt,
		OfferedAt:      c.EarlyJoinOfferedAt,
		Response:       c.EarlyJoinResponse,
	}
}

func earlyJoinFrom(c *Consultation) EarlyJoinOffer {
	offered := time.Time{}
	if c.EarlyJoinOfferedAt != nil {
		offered = *c.EarlyJoinOfferedAt
	}
	return EarlyJoinOffer{
		AppointmentID:  c.AppointmentID,
		ConsultationID: c.ID,
		ScheduledAt:    c.ScheduledAt,
		OfferedAt:      offered,
		Response:       c.EarlyJoinResponse,
		RespondedAt:    c.EarlyJoinRespondedAt,
	}
}

// ReadyForNext is the doctor saying they have finished the current visit and
// can see the immediate next patient a few minutes early. finishedAppointmentID
// is the visit they just closed (join-style: appointment id). A nil id means
// "use my latest ended consult".
//
// It pings only that next patient. It never moves this slot or any later one.
func (s *Service) ReadyForNext(ctx context.Context, principal middleware.Principal, finishedAppointmentID uuid.UUID) (*ReadyForNextResult, error) {
	if principal.DoctorID == uuid.Nil {
		return nil, ErrForbidden
	}

	if _, err := s.store.FindActiveForDoctor(ctx, s.pool, principal.DoctorID); err == nil {
		return nil, ErrInvalidState
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	finished, err := s.finishedConsult(ctx, principal.DoctorID, finishedAppointmentID)
	if err != nil {
		return nil, err
	}

	next, err := s.store.FindNextUpcomingForDoctor(ctx, s.pool, principal.DoctorID, finished.ScheduledAt)
	if err != nil {
		return nil, err
	}

	if next.Status == StatusWaiting {
		out := offerFrom(next)
		out.Status = ReadyForNextAlreadyWaiting
		return &out, nil
	}

	if next.EarlyJoinOfferedAt != nil {
		out := offerFrom(next)
		if next.EarlyJoinResponse != nil && *next.EarlyJoinResponse == EarlyJoinDeclined {
			out.Status = ReadyForNextDeclined
			return &out, nil
		}
		out.Status = ReadyForNextAlreadyOffered
		return &out, nil
	}

	now := time.Now().UTC()
	var claimed bool
	err = database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		ok, claimErr := s.store.ClaimEarlyJoinOffered(ctx, tx, next.ID, now)
		if claimErr != nil {
			return claimErr
		}
		if !ok {
			return nil
		}
		claimed = true
		return s.outbox.Enqueue(ctx, tx, events.SubjectConsultationEarlyJoinOffered, next.ID.String(),
			events.ConsultationEarlyJoinOffered{
				SourceConsultationID: finished.ID,
				SourceAppointmentID:  finished.AppointmentID,
				NextConsultationID:   next.ID,
				NextAppointmentID:    next.AppointmentID,
				NextPatientID:        next.PatientID,
				DoctorID:             principal.DoctorID,
				OfferedAt:            now,
			})
	})
	if err != nil {
		return nil, err
	}

	reloaded, err := s.store.GetConsultation(ctx, s.pool, next.ID)
	if err != nil {
		return nil, err
	}
	out := offerFrom(reloaded)
	if claimed {
		out.Status = ReadyForNextOffered
	} else {
		out.Status = ReadyForNextAlreadyOffered
	}
	return &out, nil
}

func (s *Service) finishedConsult(ctx context.Context, doctorID, finishedAppointmentID uuid.UUID) (*Consultation, error) {
	if finishedAppointmentID != uuid.Nil {
		c, err := s.store.GetConsultationByAppointment(ctx, s.pool, finishedAppointmentID)
		if err != nil {
			return nil, err
		}
		if c.DoctorID != doctorID {
			return nil, ErrForbidden
		}
		if !c.Status.terminal() {
			return nil, ErrInvalidState
		}
		return c, nil
	}

	c, err := s.store.FindLatestTerminalForDoctor(ctx, s.pool, doctorID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrInvalidState
		}
		return nil, err
	}
	return c, nil
}

// GetEarlyJoin returns the courtesy offer for this appointment, if any.
func (s *Service) GetEarlyJoin(ctx context.Context, principal middleware.Principal, appointmentID uuid.UUID) (*EarlyJoinOffer, error) {
	c, err := s.store.GetConsultationByAppointment(ctx, s.pool, appointmentID)
	if err != nil {
		return nil, err
	}
	if _, ok := authorizeParty(principal, c); !ok {
		return nil, ErrForbidden
	}
	if c.EarlyJoinOfferedAt == nil {
		return nil, ErrNotFound
	}
	out := earlyJoinFrom(c)
	return &out, nil
}

// RespondEarlyJoin records join-now vs keep-booked-time. It never rewrites
// scheduled_at: accept just means the patient is willing to use the existing
// join/waiting-room path a few minutes early.
func (s *Service) RespondEarlyJoin(ctx context.Context, principal middleware.Principal, appointmentID uuid.UUID, accept bool) (*EarlyJoinOffer, error) {
	c, err := s.store.GetConsultationByAppointment(ctx, s.pool, appointmentID)
	if err != nil {
		return nil, err
	}
	role, ok := authorizeParty(principal, c)
	if !ok || role != RolePatient {
		return nil, ErrForbidden
	}
	if c.EarlyJoinOfferedAt == nil {
		return nil, ErrNotFound
	}

	want := EarlyJoinDeclined
	if accept {
		want = EarlyJoinAccepted
	}
	if c.EarlyJoinResponse != nil {
		if *c.EarlyJoinResponse == want {
			out := earlyJoinFrom(c)
			return &out, nil
		}
		return nil, ErrInvalidState
	}

	now := time.Now().UTC()
	var claimed bool
	err = database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		ok, claimErr := s.store.SetEarlyJoinResponse(ctx, tx, c.ID, want, now)
		if claimErr != nil {
			return claimErr
		}
		claimed = ok
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !claimed {
		reloaded, getErr := s.store.GetConsultation(ctx, s.pool, c.ID)
		if getErr != nil {
			return nil, getErr
		}
		if reloaded.EarlyJoinResponse != nil && *reloaded.EarlyJoinResponse == want {
			out := earlyJoinFrom(reloaded)
			return &out, nil
		}
		return nil, ErrInvalidState
	}

	reloaded, err := s.store.GetConsultation(ctx, s.pool, c.ID)
	if err != nil {
		return nil, err
	}
	out := earlyJoinFrom(reloaded)
	return &out, nil
}
