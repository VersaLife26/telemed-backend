package middleware

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// gRPC authentication for the internal mesh.
//
// The gRPC servers on this platform shipped with no authentication at all.
// That is not a theoretical gap: SchedulingService.BookSlot takes a patient_id
// from the request and books on their behalf; CancelAppointment takes
// actor_role from the request and believes it; GetUsersBatch returns name,
// phone, email and role for arbitrary user ids. Anything that could open a TCP
// connection to the port had full authority over every patient on the platform.
//
// These ports are never exposed publicly, but "not routed from the internet" is
// a network accident, not an access control. A misapplied NetworkPolicy, a
// debug port-forward, or one compromised pod is all it takes.

// ServiceAuthConfig configures the interceptor.
type ServiceAuthConfig struct {
	// Authenticator verifies the bearer token, exactly as the HTTP side does.
	Authenticator *Authenticator
	// RequiredRole is the role a caller must hold. Internal traffic uses
	// RoleService; a human token must not be able to drive these APIs.
	RequiredRole Role
	// AllowUnauthenticated lists full method names that legitimately need no
	// caller -- health checks and nothing else. Anything added here is a
	// deliberate hole and should be justified where it is set.
	AllowUnauthenticated []string
}

// UnaryServiceAuth returns an interceptor that rejects any call without a valid
// service token.
//
// It fails CLOSED. An HTTP rate limiter can fail open because the cost of a
// blip is some extra load; an authorisation check cannot, because the cost is
// unauthenticated access to medical records.
func UnaryServiceAuth(cfg ServiceAuthConfig) grpc.UnaryServerInterceptor {
	allowed := make(map[string]struct{}, len(cfg.AllowUnauthenticated))
	for _, m := range cfg.AllowUnauthenticated {
		allowed[m] = struct{}{}
	}

	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		if _, ok := allowed[info.FullMethod]; ok {
			return handler(ctx, req)
		}

		if cfg.Authenticator == nil {
			// Refusing every call is the right failure mode for a
			// misconfigured deployment. Serving them unauthenticated is not.
			return nil, status.Error(codes.Unavailable,
				"grpc authentication is not configured on this server")
		}

		raw, err := bearerFromMetadata(ctx)
		if err != nil {
			return nil, err
		}

		p, err := cfg.Authenticator.Verify(raw)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "invalid service token")
		}

		want := cfg.RequiredRole
		if want == "" {
			want = RoleService
		}
		if !p.HasRole(want) {
			// Deliberately not "you need role X" -- an unauthorised caller
			// learns nothing from us about what would have worked.
			return nil, status.Error(codes.PermissionDenied, "caller is not permitted")
		}

		// The verified principal is placed on the context so a handler reads
		// the CALLER's identity rather than trusting an actor_id field in the
		// request body. That field was the actual vulnerability: scheduling
		// treated an empty actor_id as an admin.
		return handler(WithPrincipal(ctx, p), req)
	}
}

// StreamServiceAuth is the streaming counterpart.
func StreamServiceAuth(cfg ServiceAuthConfig) grpc.StreamServerInterceptor {
	unary := UnaryServiceAuth(cfg)
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		_, err := unary(ss.Context(), nil,
			&grpc.UnaryServerInfo{FullMethod: info.FullMethod},
			func(ctx context.Context, _ any) (any, error) {
				return nil, handler(srv, &wrappedStream{ServerStream: ss, ctx: ctx})
			})
		return err
	}
}

type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }

func bearerFromMetadata(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "missing metadata")
	}
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return "", status.Error(codes.Unauthenticated, "missing authorization metadata")
	}
	scheme, token, found := strings.Cut(vals[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", status.Error(codes.Unauthenticated,
			"authorization metadata must be 'Bearer <token>'")
	}
	return token, nil
}
