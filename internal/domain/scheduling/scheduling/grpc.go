package scheduling

import (
	"context"
	"errors"
	"math"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	schedulingv1 "telemed/internal/pb/scheduling/v1"
	"telemed/internal/platform/middleware"
)

// GRPCServer exposes the internal scheduling API.
//
// It is a thin translation layer over the same Service the REST handlers use.
// Two entry points into one implementation is the point: if BookSlot had a
// second implementation for gRPC, the race protection would have to be correct
// twice, and one day it would not be.
//
// # What this surface is, after the F5 fix
//
// It is authenticated, service-role-only, and READ-ONLY.
//
// Every method used to derive the actor from the request. BookSlot took a
// patient_id and booked on their behalf. CancelAppointment took an actor_role
// and believed it, honouring "admin" plus force. GetAppointment treated an
// EMPTY actor_id as a trusted internal read and defaulted the role to "admin",
// so the least effort a caller could make bought the most authority anyone
// could have. With no interceptor in front, "the caller" was whoever could
// open a TCP socket.
//
// The interceptor (middleware.UnaryServiceAuth, wired in cmd/server/main.go)
// fixes who may call. This file fixes what a call may claim: no handler reads
// an identity out of a request message any more. actor_id and actor_role are
// still on the wire because the proto is shared with telemed-infra and a field
// is never removed from a live contract -- but nothing reads them, and the
// tests pin that.
//
// The two write methods are withdrawn rather than re-authorised, because
// authorising them correctly needs a fact this surface does not have:
//
//   - Booking is a patient's act. It writes intake -- the patient's own
//     account of their symptoms -- into their clinical record, and it creates
//     a payable. A service token proves that a platform component is calling;
//     it does not name the human who decided to book, and there is nowhere in
//     the schema to record one. The HTTP handler already refuses a non-patient
//     for exactly this reason ("an administrator booking on a patient's behalf
//     goes through support tooling ... so that the audit trail records who
//     really booked"); the gRPC surface disagreeing with it was the bug.
//   - Cancelling decides a refund. RefundPolicyFor prices a patient-initiated
//     cancellation differently from a doctor-initiated one, so "who cancelled"
//     is a financial fact, not a label. Taken from the request it was a way to
//     choose your own refund; taken from a service token it is a fact about a
//     process, not about a party.
//
// Neither has a caller: nothing on the platform imports
// schedulingv1.SchedulingServiceClient today. The internal paths that really
// do need to cancel -- payment failure, unpaid timeout, doctor leave -- run
// in-process through Service and stamp "system", and they are not reachable
// from the network at all.
//
// Restoring them needs a delegation model, not a config flag: a claim that
// names the human who authorised the act, and a column to record them in.
type GRPCServer struct {
	schedulingv1.UnimplementedSchedulingServiceServer
	svc *Service
}

// NewGRPCServer builds the gRPC service.
func NewGRPCServer(svc *Service) *GRPCServer { return &GRPCServer{svc: svc} }

var _ schedulingv1.SchedulingServiceServer = (*GRPCServer)(nil)

// grpcCaller returns the verified principal the interceptor attached, or an
// error.
//
// It re-checks RoleService even though UnaryServiceAuth already did. That is
// not belt-and-braces for its own sake: it is what makes a handler correct
// independently of how it was mounted. The whole finding was a server whose
// authorisation lived somewhere other than the code doing the authorising, and
// a handler that is safe only while somebody remembers to wrap it has the same
// shape as the bug.
func grpcCaller(ctx context.Context) (middleware.Principal, error) {
	p, ok := middleware.PrincipalFrom(ctx)
	if !ok {
		// Reached only if the interceptor was removed. Fail closed, and say
		// nothing about why -- an unauthenticated caller learns nothing here.
		return middleware.Principal{}, status.Error(codes.Unauthenticated, "unauthenticated")
	}
	if !p.HasRole(middleware.RoleService) {
		return middleware.Principal{}, status.Error(codes.PermissionDenied, "caller is not permitted")
	}
	if p.UserID == uuid.Nil {
		// A service token with no subject cannot be recorded as an actor, and
		// an unattributable read of a patient's appointment is not one this
		// service is willing to serve. The zero value is refused explicitly
		// because the vulnerability being fixed here WAS a zero value: an
		// empty actor_id used to mean admin.
		return middleware.Principal{}, status.Error(codes.PermissionDenied, "caller is not permitted")
	}
	return p, nil
}

// MaxGRPCRecvBytes caps an inbound gRPC message on this service's listener.
//
// The widest request on this surface carries a single UUID, so 256 KiB is
// enormous slack and the number exists to replace gRPC's unexamined 4 MiB
// default rather than to bind any real call. It lives here, next to the
// methods, so a new method's author sees the budget it is being added to.
const MaxGRPCRecvBytes = 256 << 10

// BookSlot is withdrawn from the internal surface. See the type comment.
func (g *GRPCServer) BookSlot(ctx context.Context, _ *schedulingv1.BookSlotRequest) (*schedulingv1.BookSlotResponse, error) {
	if _, err := grpcCaller(ctx); err != nil {
		return nil, err
	}
	return nil, status.Error(codes.Unimplemented,
		"booking is a patient action: use POST /api/v1/appointments, where the patient's own token proves who is booking")
}

// CancelAppointment is withdrawn from the internal surface. See the type comment.
func (g *GRPCServer) CancelAppointment(ctx context.Context, _ *schedulingv1.CancelAppointmentRequest) (*schedulingv1.CancelAppointmentResponse, error) {
	if _, err := grpcCaller(ctx); err != nil {
		return nil, err
	}
	return nil, status.Error(codes.Unimplemented,
		"cancellation decides a refund and must name its actor: use PUT /api/v1/appointments/{id}/cancel, or the admin force-cancel route")
}

// GetAppointment reads one appointment for an authenticated internal caller.
//
// The read is deliberately unfiltered by party: consultation-service resolves
// appointments it is not a party to, and that is the one legitimate use this
// surface has. What makes it safe is that the authority now comes from a
// verified service token rather than from an empty string, and that the
// projection carries no clinical content -- appointmentToProto has no intake
// field, so the minimum-necessary line the HTTP handler draws for admins is
// drawn here by the wire format itself.
func (g *GRPCServer) GetAppointment(ctx context.Context, req *schedulingv1.GetAppointmentRequest) (*schedulingv1.GetAppointmentResponse, error) {
	caller, err := grpcCaller(ctx)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(req.GetAppointmentId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "appointment_id must be a UUID")
	}

	// req.ActorId and req.ActorRole are NOT read. The actor is the caller.
	appt, err := g.svc.GetAppointment(ctx, id, caller.UserID, ActorService)
	if err != nil {
		return nil, grpcError(err)
	}
	return &schedulingv1.GetAppointmentResponse{Appointment: appointmentToProto(appt)}, nil
}

func appointmentToProto(a Appointment) *schedulingv1.Appointment {
	out := &schedulingv1.Appointment{
		Id:                 a.ID.String(),
		PatientId:          a.PatientID.String(),
		DoctorId:           a.DoctorID.String(),
		SlotId:             a.SlotID.String(),
		StartAt:            timestamppb.New(a.SlotStartAt.UTC()),
		EndAt:              timestamppb.New(a.SlotEndAt.UTC()),
		Status:             statusToProto(a.Status),
		PrepaymentRequired: a.PrepaymentRequired,
		Version:            clampInt32(a.Version),
		CreatedAt:          timestamppb.New(a.CreatedAt.UTC()),
	}
	if a.RefundPolicy != nil {
		out.RefundPolicy = refundToProto(*a.RefundPolicy)
	}
	return out
}

func statusToProto(s AppointmentStatus) schedulingv1.AppointmentStatus {
	switch s {
	case AppointmentPendingPayment:
		return schedulingv1.AppointmentStatus_APPOINTMENT_STATUS_PENDING_PAYMENT
	case AppointmentConfirmed:
		return schedulingv1.AppointmentStatus_APPOINTMENT_STATUS_CONFIRMED
	case AppointmentCancelled:
		return schedulingv1.AppointmentStatus_APPOINTMENT_STATUS_CANCELLED
	case AppointmentCompleted:
		return schedulingv1.AppointmentStatus_APPOINTMENT_STATUS_COMPLETED
	case AppointmentNoShow:
		return schedulingv1.AppointmentStatus_APPOINTMENT_STATUS_NO_SHOW
	default:
		return schedulingv1.AppointmentStatus_APPOINTMENT_STATUS_UNSPECIFIED
	}
}

func refundToProto(p RefundPolicy) schedulingv1.RefundPolicy {
	switch p {
	case RefundFull:
		return schedulingv1.RefundPolicy_REFUND_POLICY_FULL
	case RefundPartial:
		return schedulingv1.RefundPolicy_REFUND_POLICY_PARTIAL
	case RefundNone:
		return schedulingv1.RefundPolicy_REFUND_POLICY_NONE
	default:
		return schedulingv1.RefundPolicy_REFUND_POLICY_UNSPECIFIED
	}
}

// grpcError maps domain errors to status codes. The mapping mirrors APIError so
// a caller sees the same taxonomy over either transport.
//
// FailedPrecondition rather than Aborted for a taken slot is deliberate: Aborted
// invites a client to retry, and retrying a booking for a slot somebody else
// owns will never succeed.
func grpcError(err error) error {
	switch {
	case errors.Is(err, ErrSlotUnavailable), errors.Is(err, ErrVersionConflict), errors.Is(err, ErrSlotReserved):
		return status.Error(codes.FailedPrecondition, "slot unavailable")
	case errors.Is(err, ErrSlotLocked):
		// Aborted here IS the retryable case: the slot may still be free.
		return status.Error(codes.Aborted, "slot locked, retry")
	case errors.Is(err, ErrSlotNotFound):
		return status.Error(codes.NotFound, "slot not found")
	case errors.Is(err, ErrAppointmentNotFound):
		return status.Error(codes.NotFound, "appointment not found")
	case errors.Is(err, ErrSlotInPast):
		return status.Error(codes.FailedPrecondition, "slot has already started")
	case errors.Is(err, ErrDuplicateBooking):
		return status.Error(codes.AlreadyExists, "patient already has an appointment at this time")
	case errors.Is(err, ErrAppointmentNotCancellable):
		return status.Error(codes.FailedPrecondition, "appointment cannot be cancelled")
	case errors.Is(err, ErrAppointmentAlreadyStarted):
		return status.Error(codes.FailedPrecondition, "appointment has already started")
	case errors.Is(err, ErrForbidden):
		return status.Error(codes.PermissionDenied, "not permitted")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "cancelled")
	default:
		// Never surface the underlying message: it can carry SQL or a patient
		// identifier.
		return status.Error(codes.Internal, "internal error")
	}
}

// clampInt32 narrows to the protobuf surface without wrapping. int is 64-bit
// here and the proto fields are int32; saturating is the honest failure, since
// a wrapped version number would compare as older than everything.
func clampInt32(n int) int32 {
	switch {
	case n > math.MaxInt32:
		return math.MaxInt32
	case n < math.MinInt32:
		return math.MinInt32
	default:
		return int32(n)
	}
}
