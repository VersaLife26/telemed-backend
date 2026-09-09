package user

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	userv1 "telemed/internal/pb/user/v1"
	"telemed/internal/platform/middleware"
)

// GRPCServerOptions returns the options this service's gRPC listener must be
// built with. It exists as a named function rather than inline in main.go so
// that the interceptor a deployment actually runs is the interceptor the tests
// drive -- an authentication control asserted only against a server the test
// assembled itself proves nothing about the one that serves :9091.
//
// RoleService is required, not merely "some valid token": these methods are
// machine-to-machine. GetUsersBatch returns name, phone, email, role and status
// for arbitrary ids, and a patient's own access token must not be a directory
// dump.
//
// auth may be nil. UnaryServiceAuth then refuses every call with UNAVAILABLE,
// which is the correct reading of "authentication is not configured" on a
// surface carrying the platform's PII.
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
// gRPC's default is 4 MiB, which on GetUsersBatch is about 110,000 UUIDs in a
// single request -- and this service answers that with name, phone, email,
// role and status for every one of them. MaxUsersBatch already refuses such a
// call, but the batch cap is a check inside a handler and this is a bound on
// what the transport will even assemble in memory before a handler runs. They
// stop different things: the cap stops the query, this stops the allocation.
//
// 256 KiB is roughly 6,000 UUIDs -- sixty times the largest batch this service
// will serve, so no legitimate caller is anywhere near it, and no future method
// inherits a 4 MiB budget by default.
const MaxGRPCRecvBytes = 256 << 10

// GRPCServer implements userv1.UserServiceServer, the internal surface other
// services use to resolve a user_id into a name/phone/role without a
// cross-database join (DECISIONS.md ADR-004).
type GRPCServer struct {
	userv1.UnimplementedUserServiceServer
	svc *Service
	log zerolog.Logger
}

// NewGRPCServer builds the gRPC adapter over the domain service.
func NewGRPCServer(svc *Service, log zerolog.Logger) *GRPCServer {
	return &GRPCServer{svc: svc, log: log}
}

func toProtoUser(u User) *userv1.User {
	out := &userv1.User{
		Id:       u.ID.String(),
		Phone:    u.Phone,
		Name:     u.Name,
		Language: string(u.Language),
		Role:     string(u.Role),
		Status:   string(u.Status),
		//nolint:gosec // G115: no_show_count is `INT NOT NULL DEFAULT 0` (migrations/000002_users.up.sql:24). Postgres INT *is* int32, so the value cannot exceed the destination's range; widening the column would have to change this line too.
		NoShowCount: int32(u.NoShowCount),
		CreatedAt:   timestamppb.New(u.CreatedAt),
	}
	if u.Email != nil {
		out.Email = *u.Email
	}
	return out
}

// GetUser resolves one user by id. A caller passing an id that does not
// exist gets a documented NOT_FOUND, not an internal error -- a stale
// reference in another service's table is an expected condition, not a bug.
func (g *GRPCServer) GetUser(ctx context.Context, req *userv1.GetUserRequest) (*userv1.GetUserResponse, error) {
	id, err := uuid.Parse(req.GetUserId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "user_id must be a valid UUID")
	}
	u, err := g.svc.GetUser(ctx, id)
	if errors.Is(err, ErrUserNotFound) {
		return nil, status.Error(codes.NotFound, "user not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "lookup failed")
	}
	return &userv1.GetUserResponse{User: toProtoUser(*u)}, nil
}

// GetUsersBatch resolves many ids in one round trip. Unknown ids are simply
// absent from the response rather than failing the whole call, so a caller
// rendering a list of N users does not lose all N because one id was stale.
//
// The batch is capped at MaxUsersBatch. This method returns name, phone,
// email, role and status for every id it is handed, so an uncapped batch is a
// full PII directory dump in a single call: an attacker holding a service
// token -- from a compromised pod, an SSRF, or a leaked secret -- would need
// one request rather than a paced walk that a rate limiter or an anomaly
// detector might notice. Requiring RoleService is who may ask; the cap is how
// much they may ask for, and only the second one bounds the blast radius of a
// token that has already leaked.
//
// An oversized request is REJECTED and LOGGED. Rejected rather than silently
// truncated, because a caller that quietly receives 100 of the 5000 users it
// asked for renders a wrong list and never finds out; a caller that gets
// InvalidArgument fixes its paging. Logged because there is no legitimate
// client that asks for more than a page at a time -- every in-tree caller
// resolves the ids on one screen -- so an oversized batch is not a bug report,
// it is the first observable step of the directory dump above, and it is the
// only signal that a service token is being used by something that is not a
// service. The count and the caller are logged; the ids are not, because a
// list of user ids is exactly the PII this method exists to protect.
func (g *GRPCServer) GetUsersBatch(ctx context.Context, req *userv1.GetUsersBatchRequest) (*userv1.GetUsersBatchResponse, error) {
	if n := len(req.GetUserIds()); n > MaxUsersBatch {
		caller := "unknown"
		if p, ok := middleware.PrincipalFrom(ctx); ok && p.Subject != "" {
			caller = p.Subject
		}
		g.log.Warn().
			Int("requested", n).
			Int("max", MaxUsersBatch).
			Str("caller", caller).
			Msg("oversized GetUsersBatch rejected: a batch this large is a directory dump, not a screen render")
		return nil, status.Errorf(codes.InvalidArgument,
			"user_ids must contain at most %d ids, got %d", MaxUsersBatch, n)
	}

	ids := make([]uuid.UUID, 0, len(req.GetUserIds()))
	for _, raw := range req.GetUserIds() {
		id, err := uuid.Parse(raw)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid user_id %q", raw)
		}
		ids = append(ids, id)
	}

	users, err := g.svc.GetUsersBatch(ctx, ids)
	if errors.Is(err, ErrBatchTooLarge) {
		// Unreachable through this method -- the check above already ran --
		// but mapped rather than collapsed into Internal, so the service-layer
		// guard keeps its meaning if a second transport is ever added.
		return nil, status.Errorf(codes.InvalidArgument,
			"user_ids must contain at most %d ids", MaxUsersBatch)
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "batch lookup failed")
	}

	out := make([]*userv1.User, 0, len(users))
	// Indexed rather than ranged by value: User is 208 bytes and this runs
	// over every row in the batch.
	for i := range users {
		out = append(out, toProtoUser(users[i]))
	}
	return &userv1.GetUsersBatchResponse{Users: out}, nil
}
