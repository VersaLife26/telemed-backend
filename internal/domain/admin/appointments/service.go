package appointments

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/domain/admin/audit"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
)

// Service implements list/search plus the two commands scheduling-service
// must apply on this service's behalf.
type Service struct {
	pool   database.Pool
	repo   *Repository
	outbox *events.Outbox
}

func NewService(pool database.Pool, repo *Repository, outbox *events.Outbox) *Service {
	return &Service{pool: pool, repo: repo, outbox: outbox}
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Appointment, error) {
	return s.repo.Get(ctx, id)
}

func (s *Service) List(ctx context.Context, f ListFilter) ([]Appointment, int64, error) {
	return s.repo.List(ctx, f)
}

// ForceCancel publishes admin.appointment_force_cancel_requested.
// scheduling-service applies the cancellation and publishes
// appointment.cancelled, which internal/analytics.Projector consumes back
// into appointments_projection -- so the status here updates once that
// round-trip completes, not synchronously.
func (s *Service) ForceCancel(ctx context.Context, appointmentID, adminID uuid.UUID, reason string) error {
	if _, err := s.repo.Get(ctx, appointmentID); err != nil {
		return err
	}
	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return s.outbox.Enqueue(ctx, tx, events.SubjectAdminAppointmentForceCancel, appointmentID.String(), ForceCancelCommand{
			AppointmentID: appointmentID, Reason: reason, AdminID: adminID,
		})
	})
	if err != nil {
		return err
	}
	audit.Stage(ctx, audit.Draft{
		Action: "appointment.force_cancel_requested", ResourceType: "appointment", ResourceID: appointmentID.String(),
		NewValue: map[string]any{"reason": reason},
	})
	return nil
}

// ResolveDoubleBooking publishes admin.double_booking_resolve_requested.
func (s *Service) ResolveDoubleBooking(ctx context.Context, keep, cancel, adminID uuid.UUID, reason string) error {
	if _, err := s.repo.Get(ctx, keep); err != nil {
		return err
	}
	if _, err := s.repo.Get(ctx, cancel); err != nil {
		return err
	}
	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return s.outbox.Enqueue(ctx, tx, events.SubjectAdminDoubleBookingResolveRequested, keep.String(), ResolveDoubleBookingCommand{
			KeepAppointmentID: keep, CancelAppointmentID: cancel, Reason: reason, AdminID: adminID,
		})
	})
	if err != nil {
		return err
	}
	audit.Stage(ctx, audit.Draft{
		Action: "appointment.double_booking_resolve_requested", ResourceType: "appointment", ResourceID: keep.String(),
		NewValue: map[string]any{"cancel_appointment_id": cancel, "reason": reason},
	})
	return nil
}
