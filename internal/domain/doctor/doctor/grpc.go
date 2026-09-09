package doctor

import (
	"context"
	"errors"
	"math"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	doctorv1 "telemed/internal/pb/doctor/v1"
	"telemed/internal/platform/middleware"
)

// GRPCServerOptions returns the options this service's gRPC listener must be
// built with. It is a named function rather than inline wiring in main.go so
// the interceptor a deployment actually runs is the one the tests drive: an
// authentication control asserted only against a server the test assembled
// itself proves nothing about the one that serves :9092.
//
// RoleService is required rather than "any valid token". This service accepts
// patient and doctor tokens on its HTTP surface (ADR-010), and without the role
// gate every one of them would also be a key to the internal mesh.
//
// auth may be nil, in which case UnaryServiceAuth refuses every call with
// UNAVAILABLE. Serving an unauthenticated gRPC surface because authentication
// failed to configure is the one outcome that is never acceptable.
func GRPCServerOptions(auth *middleware.Authenticator) []grpc.ServerOption {
	cfg := middleware.ServiceAuthConfig{
		Authenticator: auth,
		RequiredRole:  middleware.RoleService,
		// No exemptions. This server registers no health service, so there is
		// no method that legitimately needs an anonymous caller.
		AllowUnauthenticated: nil,
	}
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(middleware.UnaryServiceAuth(cfg)),
		grpc.ChainStreamInterceptor(middleware.StreamServiceAuth(cfg)),
		grpc.MaxRecvMsgSize(MaxGRPCRecvBytes),
	}
}

// MaxGRPCRecvBytes caps an inbound gRPC message.
//
// gRPC's default is 4 MiB, which on GetDoctorsBatch is about 110,000 UUIDs in
// one request. MaxDoctorsBatch already refuses such a call, but the batch cap
// is a check inside a handler and this is a bound on what the transport will
// assemble in memory before any handler runs. They stop different things: the
// cap stops the queries, this stops the allocation.
//
// 256 KiB is roughly 6,000 UUIDs -- sixty times the largest batch this service
// will serve, so no legitimate caller is near it, and no future method inherits
// a 4 MiB budget by default.
const MaxGRPCRecvBytes = 256 << 10

// GRPCServer implements doctorv1.DoctorServiceServer: the internal surface
// other services call instead of joining across the database boundary
// (ADR-004). It is a thin adapter over Service -- no SQL, no business rules
// beyond what Service already enforces.
type GRPCServer struct {
	doctorv1.UnimplementedDoctorServiceServer
	repo *Repository
	log  zerolog.Logger
}

// NewGRPCServer builds a GRPCServer.
func NewGRPCServer(repo *Repository, log zerolog.Logger) *GRPCServer {
	return &GRPCServer{repo: repo, log: log}
}

// MaxDoctorsBatch caps how many ids one GetDoctorsBatch call may resolve.
//
// 100 is the platform's existing "one page" ceiling -- httpx.Pagination clamps
// per_page to the same number -- and every legitimate caller of this method is
// resolving the doctors visible on one page of appointments or consultations.
// Anything larger is not a page.
const MaxDoctorsBatch = 100

// GetDoctor resolves one doctor by id. A doctor that does not exist (or was
// soft-deleted) is NOT_FOUND, which callers must treat as a valid outcome --
// e.g. a stale foreign reference in another service's table -- not a system
// error.
func (g *GRPCServer) GetDoctor(ctx context.Context, req *doctorv1.GetDoctorRequest) (*doctorv1.GetDoctorResponse, error) {
	id, err := uuid.Parse(req.GetDoctorId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "doctor_id must be a valid UUID")
	}

	d, err := g.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, status.Error(codes.NotFound, "doctor not found")
		}
		return nil, status.Errorf(codes.Internal, "doctor: get doctor: %v", err)
	}
	return &doctorv1.GetDoctorResponse{Doctor: toProto(d)}, nil
}

// GetDoctorsBatch resolves many ids in one round trip. Unknown ids are
// silently omitted, not an error -- a caller resolving, say, doctor names
// for a list of appointments should not fail the whole list because one
// doctor was deleted.
// The batch is capped at MaxDoctorsBatch, and the lookup is ONE query rather
// than one per id.
//
// Both halves were the same defect. The proto puts no bound on the repeated
// field and the server sets no grpc.MaxRecvMsgSize, so a single 4 MiB message
// carries about 110,000 UUIDs; the loop then issued 110,000 sequential
// `WHERE id = $1` queries while holding a pool connection. That is a database
// outage caused by one request, available to anything holding a mesh service
// token -- and the response is a bulk read of doctor identity including SLMC
// registration numbers. RoleService decides who may ask; the cap decides how
// much, and only the cap bounds what a leaked token is worth.
//
// An oversized request is REJECTED and LOGGED. Rejected rather than truncated,
// because a caller that silently receives 100 of the 5,000 doctors it asked
// for renders a wrong list and never learns why. Logged because no in-tree
// caller resolves more than a page, so an oversized batch is not a bug report
// -- it is the first observable step of an enumeration. The count and the
// caller go in the log line; the ids do not.
func (g *GRPCServer) GetDoctorsBatch(ctx context.Context, req *doctorv1.GetDoctorsBatchRequest) (*doctorv1.GetDoctorsBatchResponse, error) {
	raw := req.GetDoctorIds()
	if n := len(raw); n > MaxDoctorsBatch {
		caller := "unknown"
		if p, ok := middleware.PrincipalFrom(ctx); ok && p.Subject != "" {
			caller = p.Subject
		}
		g.log.Warn().
			Int("requested", n).
			Int("max", MaxDoctorsBatch).
			Str("caller", caller).
			Msg("oversized GetDoctorsBatch rejected: a batch this large enumerates the register, it does not render a page")
		return nil, status.Errorf(codes.InvalidArgument,
			"doctor_ids must contain at most %d ids, got %d", MaxDoctorsBatch, n)
	}

	// Unparseable ids are skipped rather than failing the call, matching the
	// method's documented "unknown ids are silently omitted" contract: a caller
	// resolving names for a list of appointments should not lose the list
	// because one reference is stale.
	ids := make([]uuid.UUID, 0, len(raw))
	for _, s := range raw {
		if id, err := uuid.Parse(s); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return &doctorv1.GetDoctorsBatchResponse{Doctors: nil}, nil
	}

	doctors, err := g.repo.ListByIDs(ctx, ids)
	if err != nil {
		return nil, status.Error(codes.Internal, "doctor: batch lookup failed")
	}

	out := make([]*doctorv1.Doctor, 0, len(doctors))
	for i := range doctors {
		out = append(out, toProto(doctors[i]))
	}
	return &doctorv1.GetDoctorsBatchResponse{Doctors: out}, nil
}

func toProto(d Doctor) *doctorv1.Doctor {
	languages := make([]string, len(d.Languages))
	for i, l := range d.Languages {
		languages[i] = string(l)
	}
	return &doctorv1.Doctor{
		Id:                 d.ID.String(),
		UserId:             d.UserID.String(),
		SlmcNumber:         d.SLMCNumber,
		Specialty:          d.Specialty,
		DisplayName:        d.DisplayName,
		FeeLkr:             d.FeeCents,
		Languages:          languages,
		VerificationStatus: string(d.VerificationStatus),
		Rating:             d.Rating,
		ReviewCount:        clampInt32(d.ReviewCount),
		AcceptsNewPatients: d.AcceptsNewPatients,
		CreatedAt:          timestamppb.New(d.CreatedAt),
	}
}

// clampInt32 narrows a count for the protobuf surface without wrapping.
//
// int is 64-bit on every platform we build for and the proto field is int32, so
// the conversion is only safe because review counts are small -- and "only safe
// because" is exactly the assumption that stops being true. Saturating is the
// honest failure: a doctor with more than two billion reviews displays a very
// large number rather than a negative one.
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
